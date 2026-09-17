"use client";

// 帖子治理：两块列表，各自对应服务端的一张表与一个端点。
//
//   1. 回复（community.posts）：GET /api/community/posts —— 跨主题、带所属主题与板块、
//      分页 page/page_size、q 匹配主题标题或回复正文；**只读**，不会像主题详情那样把 view_count +1。
//      删除走 DELETE /api/community/topics/{topicId}/posts/{postId}。
//   2. 短评（community.topics 的评论板块）：GET /api/community/feed —— 它不是 community.posts 的行，
//      因此不在上面的端点里；feed 只有 limit（没有 total/offset），这里固定取最近一页。
//      删除走 DELETE /api/community/posts/{id}。
//
// 两个删除入口的闸门都是 community.post.moderate（服务端还会校验作者本人）。

import { useCallback, useEffect, useMemo, useState } from "react";
import { useI18n } from "@/lib/i18n/provider";
import { deleteComment, deleteReply, fetchComments, fetchModerationPosts, type ModerationPostRow } from "@/lib/api/posts";
import { describeError } from "@/lib/errors";
import { formatDateTime, formatNumber } from "@/lib/format";
import { COMMUNITY_POST_MODERATE } from "@/lib/permissions";
import { COMMENT_PAGE_SIZE, floorLabel, type CommentRow } from "@/lib/posts";
import { useSession } from "@/lib/session-context";
import {
  Button,
  Card,
  CELL_CLASS,
  ConfirmDialog,
  EmptyRow,
  HEAD_CLASS,
  LoadingRow,
  Notice,
  Pagination,
  TextInput,
} from "./ui";

const REPLY_PAGE_SIZE = 20;

export function PostsPanel({ reloadKey }: { reloadKey: number }) {
  const { t, locale } = useI18n();
  const { can } = useSession();
  const mayModerate = can(COMMUNITY_POST_MODERATE);

  // ── 回复 ──
  const [searchText, setSearchText] = useState("");
  const [query, setQuery] = useState("");
  const [page, setPage] = useState(1);
  const [rows, setRows] = useState<ModerationPostRow[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const [busyId, setBusyId] = useState("");
  const [pending, setPending] = useState<ModerationPostRow | null>(null);
  const [nonce, setNonce] = useState(0);

  // ── 短评 ──
  const [commentText, setCommentText] = useState("");
  const [commentQuery, setCommentQuery] = useState("");
  const [comments, setComments] = useState<CommentRow[]>([]);
  const [commentsLoading, setCommentsLoading] = useState(true);
  const [commentsError, setCommentsError] = useState("");
  const [pendingComment, setPendingComment] = useState<CommentRow | null>(null);
  const [commentNonce, setCommentNonce] = useState(0);

  const reloadReplies = useCallback(() => setNonce((n) => n + 1), []);
  const reloadComments = useCallback(() => setCommentNonce((n) => n + 1), []);

  useEffect(() => {
    let alive = true;
    setLoading(true);
    fetchModerationPosts({ q: query, page, pageSize: REPLY_PAGE_SIZE })
      .then((data) => {
        if (!alive) return;
        setRows(Array.isArray(data.items) ? data.items : []);
        setTotal(typeof data.total === "number" ? data.total : 0);
        setError("");
      })
      .catch((err) => {
        if (alive) setError(describeError(err, t, locale, COMMUNITY_POST_MODERATE));
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [query, page, nonce, reloadKey, t, locale]);

  useEffect(() => {
    let alive = true;
    setCommentsLoading(true);
    fetchComments(commentQuery, COMMENT_PAGE_SIZE)
      .then((data) => {
        if (!alive) return;
        setComments(data);
        setCommentsError("");
      })
      .catch((err) => {
        if (alive) setCommentsError(describeError(err, t, locale, COMMUNITY_POST_MODERATE));
      })
      .finally(() => {
        if (alive) setCommentsLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [commentQuery, commentNonce, reloadKey, t, locale]);

  const pages = useMemo(() => Math.max(1, Math.ceil((total || 0) / REPLY_PAGE_SIZE)), [total]);
  useEffect(() => {
    if (page > pages) setPage(pages);
  }, [page, pages]);

  if (!mayModerate) {
    return (
      <Card title={t("admin.posts.title")} desc={t("admin.posts.desc")}>
        <Notice kind="err" text={t("admin.denied.body", { codes: COMMUNITY_POST_MODERATE })} />
      </Card>
    );
  }

  const removeReply = (row: ModerationPostRow) => {
    setBusyId(row.id);
    setMessage("");
    deleteReply(row.topic_id, row.id)
      .then(() => {
        setPending(null);
        setMessage(t("admin.posts.deleted"));
        if (rows.length === 1 && page > 1) setPage(page - 1);
        else reloadReplies();
      })
      .catch((err) => setError(describeError(err, t, locale, COMMUNITY_POST_MODERATE)))
      .finally(() => setBusyId(""));
  };

  const removeComment = (row: CommentRow) => {
    setBusyId(row.id);
    setMessage("");
    deleteComment(row.id)
      .then(() => {
        setPendingComment(null);
        setMessage(t("admin.posts.commentsDeleted"));
        reloadComments();
      })
      .catch((err) => setCommentsError(describeError(err, t, locale, COMMUNITY_POST_MODERATE)))
      .finally(() => setBusyId(""));
  };

  return (
    <div className="space-y-5">
      <Card
        title={t("admin.posts.title")}
        desc={t("admin.posts.desc")}
        actions={
          <Button type="button" onClick={reloadReplies} disabled={loading}>
            {t("admin.refresh")}
          </Button>
        }
      >
        <div className="space-y-3">
          <div className="flex flex-wrap items-end gap-3">
            <div className="w-full max-w-sm">
              <label className="block space-y-1">
                <span className="block text-[11px] text-muted">{t("admin.search")}</span>
                <TextInput
                  value={searchText}
                  placeholder={t("admin.posts.searchPlaceholder")}
                  onChange={(e) => setSearchText(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") {
                      setQuery(searchText);
                      setPage(1);
                    }
                  }}
                />
              </label>
            </div>
            <Button
              type="button"
              variant="primary"
              onClick={() => {
                setQuery(searchText);
                setPage(1);
              }}
            >
              {t("admin.search")}
            </Button>
            <span className="text-[11px] text-muted">{t("admin.total", { total: formatNumber(total, locale) })}</span>
          </div>

          {message ? <Notice kind="ok" text={message} onClose={() => setMessage("")} /> : null}
          {error ? <Notice kind="err" text={error} onClose={() => setError("")} /> : null}

          <div className="overflow-x-auto rounded-lg border border-line">
            <table className="w-full min-w-[880px] border-collapse">
              <thead className="bg-white/5">
                <tr>
                  <th className={HEAD_CLASS}>{t("admin.posts.colFloor")}</th>
                  <th className={HEAD_CLASS}>{t("admin.posts.colTopic")}</th>
                  <th className={HEAD_CLASS}>{t("admin.posts.colBoard")}</th>
                  <th className={HEAD_CLASS}>{t("admin.posts.colAuthor")}</th>
                  <th className={HEAD_CLASS}>{t("admin.posts.colCreated")}</th>
                  <th className={HEAD_CLASS}>{t("admin.posts.colExcerpt")}</th>
                  <th className={HEAD_CLASS}>{t("admin.actions")}</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {loading ? <LoadingRow colSpan={7} text={t("admin.loading")} /> : null}
                {!loading && rows.length === 0 ? <EmptyRow colSpan={7} text={t("admin.posts.empty")} /> : null}
                {!loading &&
                  rows.map((row) => (
                    <tr key={row.id} className="hover:bg-white/5">
                      <td className={CELL_CLASS + " font-mono text-[11px]"}>
                        {floorLabel(row.post_number)}
                        {row.reply_to_post_number ? (
                          <span className="mt-1 block text-muted">{t("admin.posts.replyTo", { floor: row.reply_to_post_number })}</span>
                        ) : null}
                      </td>
                      <td className={CELL_CLASS}>
                        <a href={"/community/" + row.topic_id} target="_blank" rel="noreferrer" className="text-ink no-underline hover:underline">
                          {row.topic_title || row.topic_id}
                        </a>
                      </td>
                      <td className={CELL_CLASS + " font-mono text-[11px]"}>{row.board_code}</td>
                      <td className={CELL_CLASS}>{row.author_name || t("admin.unknown")}</td>
                      <td className={CELL_CLASS}>{formatDateTime(row.created_at, locale)}</td>
                      <td className={CELL_CLASS}>
                        <span className="block max-w-md whitespace-pre-wrap break-words">{row.excerpt}</span>
                        {row.truncated ? <span className="text-[11px] text-muted">{t("admin.posts.truncated")}</span> : null}
                      </td>
                      <td className={CELL_CLASS}>
                        <Button type="button" variant="danger" disabled={busyId === row.id} onClick={() => setPending(row)}>
                          {t("admin.delete")}
                        </Button>
                      </td>
                    </tr>
                  ))}
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
      </Card>

      <Card
        title={t("admin.posts.commentsTitle")}
        desc={t("admin.posts.commentsDesc")}
        actions={
          <Button type="button" onClick={reloadComments} disabled={commentsLoading}>
            {t("admin.refresh")}
          </Button>
        }
      >
        <div className="space-y-3">
          <div className="flex flex-wrap items-end gap-3">
            <div className="w-full max-w-sm">
              <label className="block space-y-1">
                <span className="block text-[11px] text-muted">{t("admin.search")}</span>
                <TextInput
                  value={commentText}
                  placeholder={t("admin.posts.commentsSearchPlaceholder")}
                  onChange={(e) => setCommentText(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") setCommentQuery(commentText);
                  }}
                />
              </label>
            </div>
            <Button type="button" variant="primary" onClick={() => setCommentQuery(commentText)}>
              {t("admin.search")}
            </Button>
            <span className="text-[11px] text-muted">{t("admin.posts.commentsLimit", { count: comments.length })}</span>
          </div>

          {commentsError ? <Notice kind="err" text={commentsError} onClose={() => setCommentsError("")} /> : null}

          <div className="overflow-x-auto rounded-lg border border-line">
            <table className="w-full min-w-[720px] border-collapse">
              <thead className="bg-white/5">
                <tr>
                  <th className={HEAD_CLASS}>{t("admin.posts.commentsColEntity")}</th>
                  <th className={HEAD_CLASS}>{t("admin.posts.commentsColAuthor")}</th>
                  <th className={HEAD_CLASS}>{t("admin.posts.colCreated")}</th>
                  <th className={HEAD_CLASS}>{t("admin.posts.commentsColBody")}</th>
                  <th className={HEAD_CLASS}>{t("admin.actions")}</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {commentsLoading ? <LoadingRow colSpan={5} text={t("admin.loading")} /> : null}
                {!commentsLoading && comments.length === 0 ? <EmptyRow colSpan={5} text={t("admin.posts.commentsEmpty")} /> : null}
                {!commentsLoading &&
                  comments.map((row) => (
                    <tr key={row.id} className="hover:bg-white/5">
                      <td className={CELL_CLASS}>
                        {row.entity_id ? (
                          <a href={"/catalog/" + row.entity_id} target="_blank" rel="noreferrer" className="text-ink no-underline hover:underline">
                            {row.entity_title || row.entity_id}
                          </a>
                        ) : (
                          <span className="text-muted">{t("admin.unknown")}</span>
                        )}
                      </td>
                      <td className={CELL_CLASS}>{row.author_name || t("admin.unknown")}</td>
                      <td className={CELL_CLASS}>{formatDateTime(row.created_at, locale)}</td>
                      <td className={CELL_CLASS}>
                        <span className="block max-w-md whitespace-pre-wrap break-words">{row.body}</span>
                      </td>
                      <td className={CELL_CLASS}>
                        <Button type="button" variant="danger" disabled={busyId === row.id} onClick={() => setPendingComment(row)}>
                          {t("admin.delete")}
                        </Button>
                      </td>
                    </tr>
                  ))}
              </tbody>
            </table>
          </div>
        </div>
      </Card>

      {pending ? (
        <ConfirmDialog
          title={t("admin.posts.deleteTitle")}
          body={t("admin.posts.deleteBody", { floor: pending.post_number, excerpt: pending.excerpt })}
          confirmLabel={t("admin.delete")}
          cancelLabel={t("admin.cancel")}
          danger
          busy={busyId === pending.id}
          onClose={() => setPending(null)}
          onConfirm={() => removeReply(pending)}
        />
      ) : null}

      {pendingComment ? (
        <ConfirmDialog
          title={t("admin.posts.commentsDeleteTitle")}
          body={t("admin.posts.commentsDeleteBody")}
          confirmLabel={t("admin.delete")}
          cancelLabel={t("admin.cancel")}
          danger
          busy={busyId === pendingComment.id}
          onClose={() => setPendingComment(null)}
          onConfirm={() => removeComment(pendingComment)}
        />
      ) : null}
    </div>
  );
}
