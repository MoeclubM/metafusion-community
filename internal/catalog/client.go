// Package catalog 是互动服务对元数据目录的唯一出口：可见性、标题与关系邻居。
// 本服务不复制目录数据、不直连目录库，目录的可见性规则只有一处实现。
package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MoeclubM/metafusion-community/internal/auth"
)

// Entity 是本服务需要的目录实体最小投影。
type Entity struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	RedirectID string `json:"redirect_id"`
}

type Client struct {
	base string
	http *http.Client
}

func New(base string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: timeout}}
}

// LookupRaw 原样返回目录服务的实体 JSON。本服务不重新定义目录 DTO：
// 需要完整实体（例如收藏列表里的 entity 字段）时直接透传，字段不会在搬运中丢失。
// 实体不存在或对调用者不可见时返回 false（目录侧统一 404）。
//
// 已合并的旧身份会先 404（合并后旧 id 不再可见），此时再用 /resolve 跟随重定向：
// 合并只广播事件、不在别人表里改引用，跟随重定向是引用方自己的责任，
// 否则用户收藏/互动记录里指向旧身份的条目会静默消失。
func (c *Client) LookupRaw(ctx context.Context, entityID string) (json.RawMessage, bool) {
	if c.base == "" || entityID == "" {
		return nil, false
	}
	if raw, ok := c.fetchEntity(ctx, entityID); ok {
		return raw, true
	}
	return c.fetchEntity(ctx, entityID+"/resolve")
}

// fetchEntity 取一次实体端点（suffix 为空即 GET /entities/{id}，为 /resolve 时跟随合并重定向）。
func (c *Client) fetchEntity(ctx context.Context, path string) (json.RawMessage, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/catalog/entities/"+path, nil)
	if err != nil {
		return nil, false
	}
	c.decorate(ctx, req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var raw json.RawMessage
	if err = json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, false
	}
	return raw, true
}

// Lookup 读取单个实体的最小投影（kind/title/status），用于可见性与标题判定。
func (c *Client) Lookup(ctx context.Context, entityID string) (Entity, bool) {
	raw, ok := c.LookupRaw(ctx, entityID)
	if !ok {
		return Entity{}, false
	}
	var e Entity
	if err := json.Unmarshal(raw, &e); err != nil || e.ID == "" {
		return Entity{}, false
	}
	return e, true
}

// LookupMany 批量取实体投影：目录没有批量端点，这里用有界并发逐条取，
// 单条失败只跳过该条（与主仓库 moduleapi.Catalog.LookupMany 的宽容语义一致）。
func (c *Client) LookupMany(ctx context.Context, ids []string) map[string]Entity {
	out := map[string]Entity{}
	if len(ids) == 0 {
		return out
	}
	const workers = 8
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	queue := make(chan string)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range queue {
				if e, ok := c.Lookup(ctx, id); ok {
					mu.Lock()
					out[id] = e
					mu.Unlock()
				}
			}
		}()
	}
	for _, id := range ids {
		select {
		case queue <- id:
		case <-ctx.Done():
			break
		}
	}
	close(queue)
	wg.Wait()
	return out
}

// Related 取与实体相邻的、kind 命中白名单且已发布的实体（用于"所属集合"）。
// 关系与端点实体一次取回，避免逐条请求。
func (c *Client) Related(ctx context.Context, entityID string, kinds []string) []Entity {
	if c.base == "" || entityID == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/catalog/entities/"+entityID+"/relations", nil)
	if err != nil {
		return nil
	}
	c.decorate(ctx, req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var payload struct {
		Items []struct {
			SourceID string `json:"source_id"`
			TargetID string `json:"target_id"`
		} `json:"items"`
		Entities map[string]Entity `json:"entities"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil
	}
	seen := map[string]bool{}
	out := []Entity{}
	for _, r := range payload.Items {
		for _, peer := range []string{r.SourceID, r.TargetID} {
			if peer == "" || peer == entityID || seen[peer] {
				continue
			}
			seen[peer] = true
			e, ok := payload.Entities[peer]
			if !ok {
				continue
			}
			if len(kinds) > 0 && !contains(kinds, e.Kind) {
				continue
			}
			if e.Status != "published" {
				continue
			}
			out = append(out, e)
		}
	}
	return out
}

func (c *Client) decorate(ctx context.Context, req *http.Request) {
	bearer, cookie := auth.CredentialsFrom(ctx)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "mf_session", Value: cookie})
	}
}

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}
