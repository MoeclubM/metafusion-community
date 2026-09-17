"use client";

// 主题治理：列表（板块/关键词筛选）、置顶/取消置顶、删除。
//
// 权限：置顶要 community.topic.pin，删他人的主题要 community.post.moderate；
// 作者删自己的主题不需要码（服务端口径），所以按钮可见性按"持有码 或 是本人"判断。
// 分页用 limit/offset——这是 /api/community/topics 的口径（服务端 paging.go 的兼容期说明）。

import { useCallback, useEffect, useMemo, useState } from "react";
import { useI18n } from "@/lib/i18n/provider";
import { fetchBoards } from "@/lib/api/boards";
import { deleteTopic, fetchTopics, setTopicPin } from "@/lib/api/topics";
import type { BoardRow } from "@/lib/boards";
import { boardText } from "@/lib/boards";
import { describeError } from "@/lib/errors";
import { formatDateTime, formatNumber } from "@/lib/format";
import { COMMUNITY_POST_MODERATE, COMMUNITY_TOPIC_PIN } from "@/lib/permissions";
import { useSession } from "@/lib/session-context";
import { COMMENT_BOARD, TOPIC_PAGE_SIZE, pageCount, topicTitle, type TopicRow } from "@/lib/topics";
import {
  Badge,
  Button,
  Card,
  CELL_CLASS,
  ConfirmDialog,
  EmptyRow,
  HEAD_CLASS,
  LoadingRow,
  Notice,
  Pagination,
  Select,
  TextInput,
} from "./ui";

export function TopicsPanel({ reloadKey }: { reloadKey: number }) {
  const { t, locale } = useI18n();
  const { can, user } = useSession();
  const mayPin = can(COMMUNITY_TOPIC_PIN);
  const mayModerate = can(COMMUNITY_POST_MODERATE);
  const maySee = mayPin || mayModerate;

  const [boards, setBoards] = useState<BoardRow[]>([]);
  const [boardCode, setBoardCode] = useState("all");
  const [searchText, setSearchText] = useState("");
  const [query, setQuery] = useState("");
  const [page, setPage] = useState(1);
  const [items, setItems] = useState<TopicRow[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const [busyId, setBusyId] = useState("");
  const [pending, setPending] = useState<TopicRow | null>(null);
  const [nonce, setNonce] = useState(0);

  // 板块下拉：评论板块不是文章板块，选择它只会拿到空列表，因此不列。
  useEffect(() => {
    let alive = true;
    fetchBoards()
      .then((list) => {
        if (alive) setBoards((Array.isArray(list) ? list : []).filter((b) => b.code !== COMMENT_BOARD));
      })
      .catch(() => {
        if (alive) setBoards([]);
      });
    return () => {
      alive = false;
    };
  }, [reloadKey]);

  useEffect(() => {
    let alive = true;
    setLoading(true);
    const offset = (page - 1) * TOPIC_PAGE_SIZE;
    fetchTopics({ boardCode, q: query, limit: TOPIC_PAGE_SIZE, offset })
      .then((data) => {
        if (!alive) return;
        setItems(data.items);
        setTotal(data.total);
        setError("");
      })
      .catch((err) => {
        if (alive) setError(describeError(err, t, locale, mayPin ? COMMUNITY_TOPIC_PIN : COMMUNITY_POST_MODERATE));
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [boardCode, query, page, nonce, reloadKey, t, locale, mayPin]);

  const pages = useMemo(() => pageCount(total, TOPIC_PAGE_SIZE), [total]);
  useEffect(() => {
    if (page > pages) setPage(pages);
  }, [page, pages]);

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  if (!maySee) {
    return (
      <Card title={t("admin.topics.title")} desc={t("admin.topics.desc")}>
        <Notice kind="err" text={t("admin.denied.body", { codes: COMMUNITY_TOPIC_PIN + ", " + COMMUNITY_POST_MODERATE })} />
      </Card>
    );
  }

  const applySearch = () => {
    setQuery(searchText);
    setPage(1);
  };

  const togglePin = (row: TopicRow) => {
    setBusyId(row.id);
    setMessage("");
    setTopicPin(row.id, !row.is_pinned)
      .then(() => {
        setItems((prev) => prev.map((item) => (item.id === row.id ? { ...item, is_pinned: !row.is_pinned } : item)));
        setMessage(t("admin.topics.pinChanged", { title: topicTitle(row), state: t(!row.is_pinned ? "admin.topics.statePinned" : "admin.topics.stateUnpinned") }));
      })
      .catch((err) => setError(describeError(err, t, locale, COMMUNITY_TOPIC_PIN)))
      .finally(() => setBusyId(""));
  };

  const removeTopic = (row: TopicRow) => {
    setBusyId(row.id);
    setMessage("");
    deleteTopic(row.id)
      .then(() => {
        setPending(null);
        setMessage(t("admin.topics.deleted", { title: topicTitle(row) }));
        // 删掉当前页最后一条时回退一页，避免停在空页上。
        if (items.length === 1 && page > 1) setPage(page - 1);
        else reload();
      })
      .catch((err) => setError(describeError(err, t, locale, COMMUNITY_POST_MODERATE)))
      .finally(() => setBusyId(""));
  };

  return (
    <Card
      title={t("admin.topics.title")}
      desc={t("admin.topics.desc")}
      actions={
        <Button type="button" onClick={reload} disabled={loading}>
          {t("admin.refresh")}
        </Button>
      }
    >
      <div className="space-y-3">
        <div className="flex flex-wrap items-end gap-3">
          <div className="w-48">
            <label className="block space-y-1">
              <span className="block text-[11px] text-muted">{t("admin.topics.boardFilter")}</span>
              <Select
                value={boardCode}
                onChange={(e) => {
                  setBoardCode(e.target.value);
                  setPage(1);
                }}
              >
                <option value="all">{t("admin.topics.allBoards")}</option>
                {boards.map((board) => (
                  <option key={board.code} value={board.code}>
                    {board.code + " · " + boardText(board.names, locale, board.name)}
                  </option>
                ))}
              </Select>
            </label>
          </div>
          <div className="w-full max-w-xs">
            <label className="block space-y-1">
              <span className="block text-[11px] text-muted">{t("admin.search")}</span>
              <TextInput
                value={searchText}
                placeholder={t("admin.topics.searchPlaceholder")}
                onChange={(e) => setSearchText(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") applySearch();
                }}
              />
            </label>
          </div>
          <Button type="button" variant="primary" onClick={applySearch}>
            {t("admin.search")}
          </Button>
          <span className="text-[11px] text-muted">{t("admin.total", { total: formatNumber(total, locale) })}</span>
          <span className="text-[11px] text-muted">{t("admin.topics.commentNote")}</span>
        </div>

        {message ? <Notice kind="ok" text={message} onClose={() => setMessage("")} /> : null}
        {error ? <Notice kind="err" text={error} onClose={() => setError("")} /> : null}

        <div className="overflow-x-auto rounded-lg border border-line">
          <table className="w-full min-w-[820px] border-collapse">
            <thead className="bg-white/5">
              <tr>
                <th className={HEAD_CLASS}>{t("admin.topics.colTitle")}</th>
                <th className={HEAD_CLASS}>{t("admin.topics.colBoard")}</th>
                <th className={HEAD_CLASS}>{t("admin.topics.colAuthor")}</th>
                <th className={HEAD_CLASS}>{t("admin.topics.colActivity")}</th>
                <th className={HEAD_CLASS}>{t("admin.topics.colCounts")}</th>
                <th className={HEAD_CLASS}>{t("admin.actions")}</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-line">
              {loading ? <LoadingRow colSpan={6} text={t("admin.loading")} /> : null}
              {!loading && items.length === 0 ? <EmptyRow colSpan={6} text={t("admin.topics.empty")} /> : null}
              {!loading &&
                items.map((row) => {
                  const own = user?.id && row.user_id === user.id;
                  const canDelete = mayModerate || own;
                  return (
                    <tr key={row.id} className="hover:bg-white/5">
                      <td className={CELL_CLASS}>
                        <span className="flex flex-wrap items-center gap-1.5">
                          {row.is_pinned ? <Badge tone="warn">{t("admin.pinned")}</Badge> : null}
                          {row.is_locked ? <Badge tone="off">{t("admin.locked")}</Badge> : null}
                          <a href={"/community/" + row.id} target="_blank" rel="noreferrer" className="text-ink no-underline hover:underline">
                            {topicTitle(row)}
                          </a>
                        </span>
                        {row.entity_id ? (
                          <span className="mt-1 block text-[11px] text-muted">
                            {t("admin.topics.entityAnchor", { title: row.entity_title || row.entity_id })}
                          </span>
                        ) : null}
                      </td>
                      <td className={CELL_CLASS + " font-mono text-[11px]"}>{row.board_code}</td>
                      <td className={CELL_CLASS}>{row.author_name || t("admin.unknown")}</td>
                      <td className={CELL_CLASS}>{formatDateTime(row.last_activity_at || row.created_at, locale)}</td>
                      <td className={CELL_CLASS}>
                        {t("admin.topics.countsFormat", {
                          views: formatNumber(row.view_count, locale),
                          replies: formatNumber(row.reply_count, locale),
                        })}
                      </td>
                      <td className={CELL_CLASS}>
                        <span className="flex flex-wrap gap-2">
                          {mayPin ? (
                            <Button type="button" disabled={busyId === row.id} onClick={() => togglePin(row)}>
                              {row.is_pinned ? t("admin.topics.unpin") : t("admin.topics.pin")}
                            </Button>
                          ) : null}
                          {canDelete ? (
                            <Button type="button" variant="danger" disabled={busyId === row.id} onClick={() => setPending(row)}>
                              {t("admin.delete")}
                            </Button>
                          ) : null}
                        </span>
                      </td>
                    </tr>
                  );
                })}
            </tbody>
          </table>
        </div>

        <Pagination
          page={page}
          pages={pages}
          onChange={setPage}
          disabled={loading}
          labels={{ prev: t("admin.prev"), next: t("admin.next"), info: t("admin.pageInfo", { page, pages }) }}
        />
      </div>

      {pending ? (
        <ConfirmDialog
          title={t("admin.topics.deleteConfirmTitle")}
          body={t("admin.topics.deleteConfirmBody", { title: topicTitle(pending) })}
          confirmLabel={t("admin.delete")}
          cancelLabel={t("admin.cancel")}
          danger
          busy={busyId === pending.id}
          onClose={() => setPending(null)}
          onConfirm={() => removeTopic(pending)}
        />
      ) : null}
    </Card>
  );
}
