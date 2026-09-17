"use client";

// 板块管理：列表 + 搜索 + 编辑入口。列表匿名可读，编辑需要 community.board.manage
// （没有码就不给编辑入口——服务端同样会拒，这里只是不把用户引到注定 403 的按钮上）。

import { useCallback, useEffect, useMemo, useState } from "react";
import { useI18n } from "@/lib/i18n/provider";
import { fetchBoards } from "@/lib/api/boards";
import { boardMatchesQuery, boardText, type BoardRow } from "@/lib/boards";
import { describeError } from "@/lib/errors";
import { COMMUNITY_BOARD_MANAGE } from "@/lib/permissions";
import { useSession } from "@/lib/session-context";
import { BoardEditor, BOARD_FIELD_KEYS } from "./BoardEditor";
import { Badge, Button, Card, CELL_CLASS, EmptyRow, HEAD_CLASS, LoadingRow, Notice, TextInput } from "./ui";

export function BoardsPanel({ reloadKey }: { reloadKey: number }) {
  const { t, locale } = useI18n();
  const { can } = useSession();
  const allowed = can(COMMUNITY_BOARD_MANAGE);

  const [rows, setRows] = useState<BoardRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [query, setQuery] = useState("");
  const [editing, setEditing] = useState<BoardRow | null>(null);
  const [message, setMessage] = useState<{ kind: "ok" | "info"; text: string } | null>(null);
  const [nonce, setNonce] = useState(0);

  const load = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    let alive = true;
    setLoading(true);
    fetchBoards()
      .then((data) => {
        if (!alive) return;
        setRows(Array.isArray(data) ? data : []);
        setError("");
      })
      .catch((err) => {
        if (alive) setError(describeError(err, t, locale, COMMUNITY_BOARD_MANAGE));
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [nonce, reloadKey, t, locale]);

  const visible = useMemo(() => rows.filter((row) => boardMatchesQuery(row, query)), [rows, query]);

  if (!allowed) {
    return (
      <Card title={t("admin.boards.title")} desc={t("admin.boards.desc")}>
        <Notice kind="err" text={t("admin.denied.body", { codes: COMMUNITY_BOARD_MANAGE })} />
      </Card>
    );
  }

  const onSaved = (updated: BoardRow, fields: string[]) => {
    setRows((prev) => prev.map((row) => (row.code === updated.code ? updated : row)));
    setEditing(null);
    const names = fields.map((name) => t(BOARD_FIELD_KEYS[name] ?? name)).join("、");
    setMessage({ kind: "ok", text: t("admin.boards.saved", { code: updated.code, fields: names }) });
  };

  return (
    <Card
      title={t("admin.boards.title")}
      desc={t("admin.boards.desc")}
      actions={
        <Button type="button" onClick={load} disabled={loading}>
          {t("admin.refresh")}
        </Button>
      }
    >
      <div className="space-y-3">
        <div className="flex flex-wrap items-center gap-3">
          <div className="w-full max-w-xs">
            <TextInput value={query} placeholder={t("admin.boards.searchPlaceholder")} onChange={(e) => setQuery(e.target.value)} />
          </div>
          <span className="text-[11px] text-muted">{t("admin.boards.showing", { shown: visible.length, total: rows.length })}</span>
        </div>

        {message ? <Notice kind={message.kind} text={message.text} onClose={() => setMessage(null)} /> : null}
        {error ? <Notice kind="err" text={error} /> : null}

        <div className="overflow-x-auto rounded-lg border border-line">
          <table className="w-full min-w-[720px] border-collapse">
            <thead className="bg-white/5">
              <tr>
                <th className={HEAD_CLASS}>{t("admin.boards.colCode")}</th>
                <th className={HEAD_CLASS}>{t("admin.boards.colName")}</th>
                <th className={HEAD_CLASS}>{t("admin.boards.colDisplay")}</th>
                <th className={HEAD_CLASS}>{t("admin.boards.colSort")}</th>
                <th className={HEAD_CLASS}>{t("admin.boards.colEnabled")}</th>
                <th className={HEAD_CLASS}>{t("admin.boards.colFeed")}</th>
                <th className={HEAD_CLASS}>{t("admin.actions")}</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-line">
              {loading ? <LoadingRow colSpan={7} text={t("admin.loading")} /> : null}
              {!loading && visible.length === 0 ? <EmptyRow colSpan={7} text={t("admin.boards.empty")} /> : null}
              {!loading &&
                visible.map((row) => (
                  <tr key={row.code} className="hover:bg-white/5">
                    <td className={CELL_CLASS + " font-mono text-[11px]"}>{row.code}</td>
                    <td className={CELL_CLASS}>
                      <span className="block text-ink">{boardText(row.names, locale, row.name)}</span>
                      <span className="block text-[11px] text-muted">{boardText(row.descriptions, locale, row.description)}</span>
                    </td>
                    <td className={CELL_CLASS}>
                      <span className="inline-flex items-center gap-2">
                        <span className="rounded px-1.5 py-0.5 text-[10px] font-mono bg-white/5">{row.color}</span>
                        <span className="text-[11px] text-muted">{row.icon}</span>
                      </span>
                    </td>
                    <td className={CELL_CLASS}>{row.sort_order}</td>
                    <td className={CELL_CLASS}>
                      <Badge tone={row.is_enabled ? "ok" : "off"}>{row.is_enabled ? t("admin.on") : t("admin.off")}</Badge>
                    </td>
                    <td className={CELL_CLASS}>
                      <Badge tone={row.show_in_feed ? "ok" : "off"}>{row.show_in_feed ? t("admin.yes") : t("admin.no")}</Badge>
                    </td>
                    <td className={CELL_CLASS}>
                      <Button type="button" onClick={() => setEditing(row)}>
                        {t("admin.boards.edit")}
                      </Button>
                    </td>
                  </tr>
                ))}
            </tbody>
          </table>
        </div>
      </div>

      {editing ? <BoardEditor row={editing} onClose={() => setEditing(null)} onSaved={onSaved} /> : null}
    </Card>
  );
}
