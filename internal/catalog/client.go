// Package catalog 是互动服务对元数据目录的唯一出口：可见性、标题与关系邻居。
// 本服务不复制目录数据、不直连目录库，目录的可见性规则只有一处实现。
//
// 出站调用统一走 internal/upstream（超时分层 + 有界重试 + 熔断），并且**失败可分辨**：
// 零值 + nil error = 目录明确回答"不存在或对调用者不可见"；err != nil = 取不到（调用方按
// 503 + upstream_unavailable 回）。此前两者共用同一个 bool，于是"上游挂了"在界面上表现为
// 条目标题消失、帖子详情 404、关联合集为空——故障对用户和运维都不可观测。
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/upstream"
)

// Entity 是本服务需要的目录实体最小投影。
type Entity struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	RedirectID string `json:"redirect_id"`
}

// catalogPolicy 是目录读的调用策略。目录读在列表/详情渲染路径上，本服务不是它的代理，
// 因此允许一次额外重试（3 次尝试、单次 2s），但总预算压在 7s 以内，不让页面等成慢响应：
// 7s = 三次尝试全打满（6s）+ 退避上限（100/200ms）+ 抖动余量（0.5）。
// 连续 5 次调用最终失败即熔断 10s，冷却后只放一个半开探测——上游整段时间不可用时，
// 快速失败比每次都把请求拖满 7s 更诚实。
func catalogPolicy() upstream.Policy {
	p := upstream.DefaultPolicy("catalog")
	p.Attempts = 3
	p.AttemptTimeout = 2 * time.Second
	p.Budget = 7 * time.Second
	p.BaseBackoff = 100 * time.Millisecond
	p.MaxBackoff = 500 * time.Millisecond
	p.Jitter = 0.5
	p.BreakerThreshold = 5
	p.BreakerOpenFor = 10 * time.Second
	return p
}

type Client struct {
	base string
	up   *upstream.Client
	// notify 是**投递站内通知**专用的执行器（策略见 notify.go）：它与读路径共用地址、
	// 但超时与重试预算更短（挂在回帖请求尾巴上，不该把真人等待的写请求拖长），
	// 因此各自一个 upstream.Client——熔断器与连接池也各自一套，互不影响。
	notify *upstream.Client
	// internalToken 是目录侧跨服务投递的共享密钥（INTERNAL_API_TOKEN）。
	// 为空即"未配置"：通知不发，评论照常成功（见 notify.go 的 Configured/ErrNotConfigured）。
	internalToken string
}

// New 建目录客户端。budgetFloor 是运维给的总预算下限（COMMUNITY_CATALOG_TIMEOUT_MS）：
// 它只用于**放宽**——小于策略自带的 7s 不生效。调小预算会砍掉重试（"配了重试却假重试"），
// 而重试与否由策略决定，不由这个历史遗留的"单次超时"旋钮决定。
func New(base string, budgetFloor time.Duration) *Client {
	p := catalogPolicy()
	if budgetFloor > p.Budget {
		p.Budget = budgetFloor
	}
	return newClient(base, upstream.New(p))
}

// newClient 允许注入执行器：测试要断言"重试有界""熔断打开"不能靠真等 2s 的单次超时。
func newClient(base string, up *upstream.Client) *Client {
	return &Client{base: strings.TrimRight(base, "/"), up: up, notify: upstream.New(notifyPolicy())}
}

// Upstream 返回出站执行器：/ready?deep=1 的深探针与请求路径共用它——
// 探测结果才会喂给同一个熔断器，而不是各看一套健康状态。
func (c *Client) Upstream() *upstream.Client { return c.up }

// LookupRaw 原样返回目录服务的实体 JSON。本服务不重新定义目录 DTO：
// 需要完整实体（例如收藏列表里的 entity 字段）时直接透传，字段不会在搬运中丢失。
//
// 返回 (nil, nil) 表示目录明确回答"不存在或对调用者不可见"（404）；
// (nil, err) 表示取不到——调用方必须按依赖不可用处理，不能折成"这个实体不存在"。
//
// 已合并的旧身份会先 404（合并后旧 id 不再可见），此时再用 /resolve 跟随重定向：
// 合并只广播事件、不在别人表里改引用，跟随重定向是引用方自己的责任，
// 否则用户收藏/互动记录里指向旧身份的条目会静默消失。
func (c *Client) LookupRaw(ctx context.Context, entityID string) (json.RawMessage, error) {
	// 未配置上游地址：与"没有这个实体"同解（既有行为，本服务不因此改成 503）。
	if c.base == "" || entityID == "" {
		return nil, nil
	}
	raw, err := c.fetchEntity(ctx, entityID)
	if err != nil {
		// 上游不可用：不再试 /resolve。那只是把同一份故障再打一遍，还多花一次调用预算。
		return nil, err
	}
	if raw != nil {
		return raw, nil
	}
	return c.fetchEntity(ctx, entityID+"/resolve")
}

// fetchEntity 取一次实体端点（suffix 为空即 GET /entities/{id}，为 /resolve 时跟随合并重定向）。
// 404 是目录的明确回答（不可见/不存在），按"没有结果"返回；其余非 200 一律按依赖故障上报：
// 把 4xx 折成"不存在"会把我们自己的调用错误伪装成"这个实体不可见"。
func (c *Client) fetchEntity(ctx context.Context, path string) (json.RawMessage, error) {
	resp, err := c.up.Do(ctx, upstream.Request{
		Method: http.MethodGet,
		URL:    c.base + "/api/catalog/entities/" + path,
		Header: outboundHeaders(ctx),
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		var raw json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return nil, upstreamError(c.name(), upstream.ReasonBadResponse, 0, err)
		}
		return raw, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil
	default:
		return nil, upstreamError(c.name(), statusReason(resp.StatusCode), resp.StatusCode, nil)
	}
}

// Lookup 读取单个实体的最小投影（kind/title/status），用于可见性与标题判定。
// 零值 + nil error = 不可见/不存在；零值 + err = 取不到。
func (c *Client) Lookup(ctx context.Context, entityID string) (Entity, error) {
	raw, err := c.LookupRaw(ctx, entityID)
	if err != nil {
		return Entity{}, err
	}
	if raw == nil {
		return Entity{}, nil
	}
	var e Entity
	if err := json.Unmarshal(raw, &e); err != nil {
		return Entity{}, upstreamError(c.name(), upstream.ReasonBadResponse, 0, err)
	}
	if e.ID == "" {
		// 200 却没有 id 是跨服务契约漂移：不能当成"不可见"——那会把契约漂移伪装成"条目不存在"。
		return Entity{}, upstreamError(c.name(), upstream.ReasonBadResponse, 0, errors.New("entity payload without id"))
	}
	return e, nil
}

// ResolveCanonical 把请求 ID 归一到存活身份（canonical）：合并 A→B 后对 A 的新写
// 必须落到 B，否则 B 页永远看不到 A 的历史评论/收藏（见 X01）。实现即 Lookup
// （已跟随 /resolve）：返回 "" + nil 表示目录明确回答不可见/不存在，调用方按 404；
// err != nil 表示取不到，调用方按 503。未配置上游地址时原样返回请求 ID（既有行为）。
func (c *Client) ResolveCanonical(ctx context.Context, entityID string) (string, error) {
	if entityID == "" {
		return "", nil
	}
	if c.base == "" {
		return entityID, nil
	}
	e, err := c.Lookup(ctx, entityID)
	if err != nil {
		return "", err
	}
	if e.ID == "" {
		return "", nil
	}
	return e.ID, nil
}

// ResolveMany 批量归一请求 ID 到 canonical（目录暂无批量解析端点，这里复用
// LookupMany 的有界并发逐条取；目录补齐批量契约后改这一处即可，调用方不动）。
// 返回 requested→canonical（不可见的记 ""，调用方按 404/跳过）；err != nil 表示上游
// 不可用，调用方必须整请求 503（部分结果里缺的条目分不清“不可见”还是“取不到”）。
func (c *Client) ResolveMany(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	if c.base == "" {
		for _, id := range ids {
			out[id] = id
		}
		return out, nil
	}
	meta, err := c.LookupMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if e, ok := meta[id]; ok && e.ID != "" {
			out[id] = e.ID
		} else {
			out[id] = ""
		}
	}
	return out, nil
}

// IdentityResolution 是目录身份契约的只读投影（主仓 lifecycle.go 的 IdentityResolution）：
// canonical_id 为存活身份，aliases 为请求 ID 沿 merged 链走过的历史别名。
// 注意它今天是前向的：查存活身份 D 拿不到曾合入的历史 A/B（目录侧只沿请求 ID 向前走），
// 存活→历史的反向枚举待目录契约补齐；本服务把展开收敛在 ResolveAliasSet 一处，
// 反向契约就绪后调用方不动（见 ResolveAliasSet 的 X01-compat 注释）。
type IdentityResolution struct {
	CanonicalID string   `json:"canonical_id"`
	Aliases     []string `json:"aliases"`
	Entity      Entity   `json:"entity"`
}

// Identity 取单个身份解析。零值 + nil = 目录明确回答不可见/不存在；err != nil = 取不到。
//
// 404 表示目录明确回答不可见/不存在；接口不存在属于部署契约错误。
func (c *Client) Identity(ctx context.Context, entityID string) (IdentityResolution, error) {
	if c.base == "" || entityID == "" {
		if entityID == "" {
			return IdentityResolution{}, nil
		}
		return IdentityResolution{CanonicalID: entityID}, nil
	}
	resp, err := c.up.Do(ctx, upstream.Request{
		Method: http.MethodGet,
		URL:    c.base + "/api/catalog/entities/" + entityID + "/identity",
		Header: outboundHeaders(ctx),
	})
	if err != nil {
		return IdentityResolution{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var v IdentityResolution
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			return IdentityResolution{}, upstreamError(c.name(), upstream.ReasonBadResponse, 0, err)
		}
		if v.CanonicalID == "" {
			return IdentityResolution{}, upstreamError(c.name(), upstream.ReasonBadResponse, 0, errors.New("identity payload without canonical_id"))
		}
		return v, nil
	case http.StatusNotFound:
		var payload struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || payload.Error != "not_found" {
			return IdentityResolution{}, upstreamError(c.name(), upstream.ReasonBadResponse, resp.StatusCode, errors.New("identity endpoint missing or unexpected 404 response"))
		}
		return IdentityResolution{}, nil
	default:
		return IdentityResolution{}, upstreamError(c.name(), statusReason(resp.StatusCode), resp.StatusCode, nil)
	}
}

// IdentityMany 批量取身份解析（目录 POST /api/catalog/entities/identity，上限 500）。
// 返回 requested→解析（不可见的不在结果里，调用方按 404/跳过）；err != nil = 上游不可用。
func (c *Client) IdentityMany(ctx context.Context, ids []string) (map[string]IdentityResolution, error) {
	out := map[string]IdentityResolution{}
	if c.base == "" || len(ids) == 0 {
		if c.base == "" {
			for _, id := range ids {
				if id != "" {
					out[id] = IdentityResolution{CanonicalID: id}
				}
			}
		}
		return out, nil
	}
	raw, err := json.Marshal(map[string]any{"ids": ids})
	if err != nil {
		return nil, err
	}
	header := outboundHeaders(ctx)
	header.Set("Content-Type", "application/json")
	resp, err := c.up.Do(ctx, upstream.Request{
		Method: http.MethodPost,
		URL:    c.base + "/api/catalog/entities/identity",
		Header: header,
		Body:   raw,
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var payload struct {
			Items   map[string]IdentityResolution `json:"items"`
			Missing []string                      `json:"missing"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return nil, upstreamError(c.name(), upstream.ReasonBadResponse, 0, err)
		}
		for id, v := range payload.Items {
			if v.CanonicalID != "" {
				out[id] = v
			}
		}
		return out, nil
	default:
		return nil, upstreamError(c.name(), statusReason(resp.StatusCode), resp.StatusCode, nil)
	}
}

// ResolveAliasSet 是 X01 读路径唯一的别名展开点：一次调用拿齐 {canonical + 全量历史别名}。
// 成功时返回的集合已含请求 ID 自身并去重，调用方直接用于 entity_id = ANY($集)；
// ("", nil, nil) = 不可见/不存在；err != nil = 取不到（调用方 503）。
//
// X01-compat（反向契约未落地）：目录今天只回前向别名，读存活身份 D 拿不到历史 A/B，
// 此时集合退化为 {D + 请求 ID}——读 A 覆盖前向链正确，读 D 仍只含 D（历史 A/B 行待回填）。
// 目录补齐存活→历史反向后，同一调用自动拿到全量，SQL 与调用方都不用改。
func (c *Client) ResolveAliasSet(ctx context.Context, requested string) (string, []string, error) {
	if requested == "" {
		return "", nil, nil
	}
	if c.base == "" {
		// 未配置上游地址与“没有这个实体”同解（同 LookupRaw 既有口径）：读路径按 404，
		// 且调用方不得再查库（见 handler 的 TestAuthBoundaryBeforeDatabase）。
		return "", nil, nil
	}
	v, err := c.Identity(ctx, requested)
	if err != nil {
		return "", nil, err
	}
	if v.CanonicalID == "" {
		return "", nil, nil
	}
	return v.CanonicalID, AliasSet(v.CanonicalID, append(append([]string{requested}, v.Aliases...), v.Entity.ID)...), nil
}

// AliasSet 组装一次读取要覆盖的 ID 集合：{canonical + 传入的全部历史别名} 去重保序。
// 它是纯函数：全量历史由调用方经 ResolveAliasSet/IdentityMany 从目录身份契约取来，
// 这里只做去重；目录反向契约未落地前，调用方只能传 {请求 ID}，读存活页的聚合因此不完整
// （见 ResolveAliasSet 的 X01-compat 注释），不要在本函数里把“集合很小”当成正确。
func AliasSet(canonical string, requested ...string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, id := range append(append([]string{}, requested...), canonical) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// LookupMany 批量取实体投影：目录没有批量端点，这里用有界并发逐条取。
//
// 返回部分结果 + err，其中只有"上游不可用"会置 err（单条不可见不是错误，只是不在结果里）。
// 调用方拿到 err 必须整请求 503：部分结果里缺的那些条目，分不清是"不可见"还是"取不到"，
// 拿它渲染列表就是"条目凭空消失"。
func (c *Client) LookupMany(ctx context.Context, ids []string) (map[string]Entity, error) {
	out := map[string]Entity{}
	if c.base == "" || len(ids) == 0 {
		return out, nil
	}
	const workers = 8
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
	)
	queue := make(chan string)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range queue {
				e, err := c.Lookup(ctx, id)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					continue
				}
				if e.ID == "" {
					continue // 不可见/已删除：不是错误，只是不进结果
				}
				mu.Lock()
				out[id] = e
				mu.Unlock()
			}
		}()
	}
	delivered := 0
deliver:
	for _, id := range ids {
		select {
		case queue <- id:
			delivered++
		case <-ctx.Done():
			// 必须跳出整个 for：break 在 select 里只跳出 select，上游不可用时 ctx 已被取消，
			// 而整个 ids 会被继续灌进队列（旧实现就是这个 bug）。
			break deliver
		}
	}
	close(queue)
	wg.Wait()
	if firstErr == nil && delivered < len(ids) {
		// 投递被 ctx 打断：map 不完整，不能当成"剩下的都不可见"。
		firstErr = upstreamError(c.name(), upstream.ReasonCancelled, 0, ctx.Err())
	}
	return out, firstErr
}

// Related 取与实体相邻的、kind 命中白名单且已发布的实体（用于"所属集合"）。
// 关系与端点实体一次取回，避免逐条请求。
// (nil, nil) = 目录明确回答"没有邻居"；err != nil = 取不到（调用方必须 503，不能回空 items）。
func (c *Client) Related(ctx context.Context, entityID string, kinds []string) ([]Entity, error) {
	if c.base == "" || entityID == "" {
		return nil, nil
	}
	resp, err := c.up.Do(ctx, upstream.Request{
		Method: http.MethodGet,
		URL:    c.base + "/api/catalog/entities/" + entityID + "/relations",
		Header: outboundHeaders(ctx),
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil
	default:
		return nil, upstreamError(c.name(), statusReason(resp.StatusCode), resp.StatusCode, nil)
	}
	var payload struct {
		Items []struct {
			SourceID string `json:"source_id"`
			TargetID string `json:"target_id"`
		} `json:"items"`
		Entities map[string]Entity `json:"entities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, upstreamError(c.name(), upstream.ReasonBadResponse, 0, err)
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
	return out, nil
}

func (c *Client) name() string { return c.up.Name() }

// outboundHeaders 组装出站请求头：调用者的凭据必须原样转发，否则目录侧的可见性判定会退化成
// 匿名，草稿/待审条目会被误判成不可见（见 auth.WithCredentials）。
func outboundHeaders(ctx context.Context) http.Header {
	h := http.Header{}
	bearer, cookie := auth.CredentialsFrom(ctx)
	if bearer != "" {
		h.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		h.Add("Cookie", (&http.Cookie{Name: "mf_session", Value: cookie}).String())
	}
	return h
}

// upstreamError 造一个"上游不可用"错误（稳定机器码 upstream_unavailable）。
// 状态码非 200/404 与响应体解析失败都走这里：两者都是"我们没法得到可信答案"，
// 而不是"这个实体不存在"。
func upstreamError(name, reason string, status int, cause error) error {
	return &upstream.Error{Upstream: name, Reason: reason, Status: status, Cause: cause}
}

// statusReason 与 upstream 包内的原因命名同构（status_<码>）：日志与探针输出里的原因码可统一检索。
func statusReason(code int) string { return "status_" + strconv.Itoa(code) }

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}
