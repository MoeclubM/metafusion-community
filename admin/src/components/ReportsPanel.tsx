"use client";

// 举报与申诉：两条队列（举报 / 申诉）+ 处置动作。
//
// 契约（服务端 handler/reports.go 与 reports_admin.go）：
//   GET  /api/community/admin/reports            队列：status 多选 + target_type + q + page/page_size
//   GET  /api/community/admin/reports/{id}       详情：报告 + 时间线 + 申诉 + target_present
//   POST /api/community/admin/reports/{id}/accept|reject|resolve
//   GET  /api/community/admin/appeals            申诉队列（默认待处理）
//   POST /api/community/admin/appeals/{id}/review
//
// 两条"不新造动作"的口径写进界面：
//   - "下线内容"先用**既有删除端点**删掉内容，再由服务端复核后记处置结论；
//     内容还在时服务端回 409 content_still_present，界面按码提示"先下线内容"。
//   - "封禁用户"由账号控制台（既有的 PUT /api/admin/users/{id}/ban）执行，本后台只记录结论。
//
// 空态与失败态严格分开：取数失败显示错误 + 重试，绝不显示成"队列已清空"。

import { useCallback, useEffect, useMemo, useState } from "react";
import { useI18n } from "@/lib/i18n/provider";
import {
  acceptReport,
  fetchAppealQueue,
  fetchReportDetail,
  fetchReportQueue,
  rejectReport,
  removeReportedContent,
  resolveReport,
  reviewAppeal,
  type AppealRow,
  type ReportDetail,
  type ReportQueueRow,
  type ReportRow,
} from "@/lib/api/reports";
import { describeError, toErrorLike } from "@/lib/errors";
import { formatDateTime, formatNumber } from "@/lib/format";
import { COMMUNITY_REPORT_REVIEW } from "@/lib/permissions";
import {
  APPEAL_STATUSES,
  REPORT_REASONS,
  REPORT_STATUSES,
  REPORT_TARGET_TYPES,
  allowedEnforcements,
  appealStatusLabelKey,
  appealTone,
  buildQueueQuery,
  eventLabelKey,
  enforcementLabelKey,
  mayRemoveContent,
  reasonLabelKey,
  statusLabelKey,
  statusTone,
  targetSummary,
  targetTypeLabelKey,
} from "@/lib/reports";
import { useSession } from "@/lib/session-context";
import {
  Badge,
  Button,
  Card,
  CELL_CLASS,
  ConfirmDialog,
  EmptyRow,
  Field,
  HEAD_CLASS,
  LoadingRow,
  Modal,
  Notice,
  Pagination,
  Select,
  TextInput,
} from "./ui";

const PAGE_SIZE = 20;

export function ReportsPanel({ reloadKey }: { reloadKey: number }) {
  const { t, locale } = useI18n();
  const { can } = useSession();
  const mayReview = can(COMMUNITY_REPORT_REVIEW);

  // ── 举报队列 ──
  const [statusFilter, setStatusFilter] = useState<string>("pending");
  const [typeFilter, setTypeFilter] = useState<string>("all");
  const [searchText, setSearchText] = useState("");
  const [query, setQuery] = useState("");
  const [page, setPage] = useState(1);
  const [rows, setRows] = useState<ReportQueueRow[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const [nonce, setNonce] = useState(0);

  // ── 详情与处置 ──
  const [detail, setDetail] = useState<ReportDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState("");
  const [note, setNote] = useState("");
  const [enforcement, setEnforcement] = useState("none");
  const [pendingAction, setPendingAction] = useState<"" | "accept" | "reject" | "resolve">("");
  const [busy, setBusy] = useState(false);

  // ── 申诉队列 ──
  const [appealStatus, setAppealStatus] = useState("pending");
  const [appeals, setAppeals] = useState<AppealRow[]>([]);
  const [appealTotal, setAppealTotal] = useState(0);
  const [appealPage, setAppealPage] = useState(1);
  const [appealLoading, setAppealLoading] = useState(true);
  const [appealError, setAppealError] = useState("");
  const [appealOpen, setAppealOpen] = useState<AppealRow | null>(null);
  const [appealNote, setAppealNote] = useState("");
  const [appealAction, setAppealAction] = useState<"" | "accepted" | "rejected">("");
  const [appealBusy, setAppealBusy] = useState(false);

  const reload = useCallback(() => setNonce((n) => n + 1), []);


  useEffect(() => {
    let alive = true;
    setLoading(true);
    fetchReportQueue({
      statuses: statusFilter === "all" ? [] : [statusFilter],
      targetType: typeFilter,
      q: query,
      page,
      pageSize: PAGE_SIZE,
    })
      .then((data) => {
        if (!alive) return;
        setRows(Array.isArray(data.items) ? data.items : []);
        setTotal(typeof data.total === "number" ? data.total : 0);
        setError("");
      })
      .catch((err) => {
        if (!alive) return;
        // 取数失败**不清空 rows**：清空会让"服务挂了"看起来像"队列空了"。
        setError(describeError(err, t, locale, COMMUNITY_REPORT_REVIEW));
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [statusFilter, typeFilter, query, page, nonce, reloadKey, t, locale]);

  useEffect(() => {
    let alive = true;
    setAppealLoading(true);
    fetchAppealQueue({
      statuses: appealStatus === "all" ? [] : [appealStatus],
      page: appealPage,
      pageSize: PAGE_SIZE,
    })
      .then((data) => {
        if (!alive) return;
        setAppeals(Array.isArray(data.items) ? data.items : []);
        setAppealTotal(typeof data.total === "number" ? data.total : 0);
        setAppealError("");
      })
      .catch((err) => {
        if (alive) setAppealError(describeError(err, t, locale, COMMUNITY_REPORT_REVIEW));
      })
      .finally(() => {
        if (alive) setAppealLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [appealStatus, appealPage, nonce, reloadKey, t, locale]);

  const pages = useMemo(() => Math.max(1, Math.ceil((total || 0) / PAGE_SIZE)), [total]);
  useEffect(() => {
    if (page > pages) setPage(pages);
  }, [page, pages]);
  const appealPages = useMemo(() => Math.max(1, Math.ceil((appealTotal || 0) / PAGE_SIZE)), [appealTotal]);

  const openDetail = (row: ReportRow) => {
    setDetail(null);
    setDetailError("");
    setNote("");
    setEnforcement("none");
    setDetailLoading(true);
    fetchReportDetail(row.id)
      .then((data) => {
        setDetail(data);
        const options = allowedEnforcements(data.item);
        setEnforcement(options.includes("content_removed") ? "content_removed" : "none");
      })
      .catch((err) => setDetailError(describeError(err, t, locale, COMMUNITY_REPORT_REVIEW)))
      .finally(() => setDetailLoading(false));
  };

  const afterAction = (text: string) => {
    setMessage(text);
    setDetail(null);
    setPendingAction("");
    reload();
  };

  const runAction = () => {
    if (!detail || pendingAction === "") return;
    const id = detail.item.id;
    setBusy(true);
    const done = (text: string) => {
      setBusy(false);
      afterAction(text);
    };
    const failed = (err: unknown) => {
      setBusy(false);
      setPendingAction("");
      // toErrorLike：catch 到的值是 unknown，先归一成 {status, code} 再翻译，避免把任意对象当错误用。
      setDetailError(describeError(toErrorLike(err), t, locale, COMMUNITY_REPORT_REVIEW));
    };
    if (pendingAction === "accept") {
      acceptReport(id, note).then(() => done(t("admin.reports.doneAccepted"))).catch(failed);
      return;
    }
    if (pendingAction === "reject") {
      rejectReport(id, note).then(() => done(t("admin.reports.doneRejected"))).catch(failed);
      return;
    }
    // 处置：enforcement=content_removed 时**先走既有删除端点**，再由服务端复核并记录结论。
    const chain =
      enforcement === "content_removed"
        ? removeReportedContent(detail.item).then(() => resolveReport(id, enforcement, note))
        : resolveReport(id, enforcement, note);
    chain.then(() => done(t("admin.reports.doneResolved"))).catch(failed);
  };

  const runAppealAction = () => {
    if (!appealOpen || appealAction === "") return;
    setAppealBusy(true);
    reviewAppeal(appealOpen.id, appealAction, appealNote)
      .then(() => {
        setAppealOpen(null);
        setAppealAction("");
        setAppealNote("");
        setMessage(appealAction === "accepted" ? t("admin.reports.appealAdopted") : t("admin.reports.appealRejected"));
        setNonce((n) => n + 1);
      })
      .catch((err) => setAppealError(describeError(err, t, locale, COMMUNITY_REPORT_REVIEW)))
      .finally(() => setAppealBusy(false));
  };

  if (!mayReview) {
    return (
      <Card title={t("admin.reports.title")} desc={t("admin.reports.desc")}>
        <Notice kind="err" text={t("admin.denied.body", { codes: COMMUNITY_REPORT_REVIEW })} />
      </Card>
    );
  }

  return (
    <div className="space-y-5">
      <Card
        title={t("admin.reports.title")}
        desc={t("admin.reports.desc")}
        actions={
          <Button type="button" onClick={reload} disabled={loading}>
            {t("admin.refresh")}
          </Button>
        }
      >
        <div className="space-y-3">
          <div className="flex flex-wrap items-end gap-3">
            <div className="w-40">
              <Field label={t("admin.reports.filterStatus")}>
                <Select
                  value={statusFilter}
                  onChange={(e) => {
                    setStatusFilter(e.target.value);
                    setPage(1);
                  }}
                >
                  <option value="all">{t("admin.reports.statusFilterAll")}</option>
                  {REPORT_STATUSES.map((status) => (
                    <option key={status} value={status}>
                      {t(statusLabelKey(status))}
                    </option>
                  ))}
                </Select>
              </Field>
            </div>
            <div className="w-44">
              <Field label={t("admin.reports.filterType")}>
                <Select
                  value={typeFilter}
                  onChange={(e) => {
                    setTypeFilter(e.target.value);
                    setPage(1);
                  }}
                >
                  <option value="all">{t("admin.reports.typeFilterAll")}</option>
                  {REPORT_TARGET_TYPES.map((type) => (
                    <option key={type} value={type}>
                      {t(targetTypeLabelKey(type))}
                    </option>
                  ))}
                </Select>
              </Field>
            </div>
            <div className="w-full max-w-sm">
              <Field label={t("admin.search")}>
                <TextInput
                  value={searchText}
                  placeholder={t("admin.reports.searchPlaceholder")}
                  onChange={(e) => setSearchText(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") {
                      setQuery(searchText);
                      setPage(1);
                    }
                  }}
                />
              </Field>
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
            <table className="w-full min-w-[900px] border-collapse">
              <thead className="bg-white/5">
                <tr>
                  <th className={HEAD_CLASS}>{t("admin.reports.colTarget")}</th>
                  <th className={HEAD_CLASS}>{t("admin.reports.colReason")}</th>
                  <th className={HEAD_CLASS}>{t("admin.reports.colReporter")}</th>
                  <th className={HEAD_CLASS}>{t("admin.reports.colCreated")}</th>
                  <th className={HEAD_CLASS}>{t("admin.reports.colStatus")}</th>
                  <th className={HEAD_CLASS}>{t("admin.reports.colHandler")}</th>
                  <th className={HEAD_CLASS}>{t("admin.actions")}</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {loading ? <LoadingRow colSpan={7} text={t("admin.loading")} /> : null}
                {/* 空态只在这三种都没发生时才显示：加载中 / 取数失败 / 真的没有数据。 */}
                {!loading && !error && rows.length === 0 ? <EmptyRow colSpan={7} text={t("admin.reports.empty")} /> : null}
                {rows.map((row) => (
                  <tr key={row.report.id} className="hover:bg-white/5">
                    <td className={CELL_CLASS}>
                      <span className="block max-w-md break-words">{targetSummary(row.report)}</span>
                      <span className="mt-1 block text-[11px] text-muted">
                        {t(targetTypeLabelKey(row.report.target_type))} · {row.report.target_id}
                      </span>
                    </td>
                    <td className={CELL_CLASS}>{t(reasonLabelKey(row.report.reason))}</td>
                    <td className={CELL_CLASS}>{row.report.reporter_name || t("admin.unknown")}</td>
                    <td className={CELL_CLASS}>{formatDateTime(row.report.created_at, locale)}</td>
                    <td className={CELL_CLASS}>
                      <Badge tone={statusTone(row.report.status)}>{t(statusLabelKey(row.report.status))}</Badge>
                      {row.appeal ? (
                        <span className="mt-1 block text-[11px] text-muted">
                          {t("admin.reports.hasAppeal", { status: t(appealStatusLabelKey(row.appeal.status)) })}
                        </span>
                      ) : null}
                    </td>
                    <td className={CELL_CLASS}>
                      {row.report.reviewer_name ? row.report.reviewer_name : <span className="text-muted">{t("admin.reports.unhandled")}</span>}
                    </td>
                    <td className={CELL_CLASS}>
                      <Button type="button" onClick={() => openDetail(row.report)}>
                        {t("admin.reports.openDetail")}
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
        title={t("admin.reports.appealsTitle")}
        desc={t("admin.reports.appealsDesc")}
        actions={
          <Button type="button" onClick={reload} disabled={appealLoading}>
            {t("admin.refresh")}
          </Button>
        }
      >
        <div className="space-y-3">
          <div className="w-40">
            <Field label={t("admin.reports.filterStatus")}>
              <Select
                value={appealStatus}
                onChange={(e) => {
                  setAppealStatus(e.target.value);
                  setAppealPage(1);
                }}
              >
                <option value="all">{t("admin.reports.statusFilterAll")}</option>
                {APPEAL_STATUSES.map((status) => (
                  <option key={status} value={status}>
                    {t(appealStatusLabelKey(status))}
                  </option>
                ))}
              </Select>
            </Field>
          </div>

          {appealError ? <Notice kind="err" text={appealError} onClose={() => setAppealError("")} /> : null}

          <div className="overflow-x-auto rounded-lg border border-line">
            <table className="w-full min-w-[820px] border-collapse">
              <thead className="bg-white/5">
                <tr>
                  <th className={HEAD_CLASS}>{t("admin.reports.appealColAppellant")}</th>
                  <th className={HEAD_CLASS}>{t("admin.reports.appealColReport")}</th>
                  <th className={HEAD_CLASS}>{t("admin.reports.appealColBody")}</th>
                  <th className={HEAD_CLASS}>{t("admin.reports.appealColCreated")}</th>
                  <th className={HEAD_CLASS}>{t("admin.reports.appealColStatus")}</th>
                  <th className={HEAD_CLASS}>{t("admin.actions")}</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {appealLoading ? <LoadingRow colSpan={6} text={t("admin.loading")} /> : null}
                {!appealLoading && !appealError && appeals.length === 0 ? (
                  <EmptyRow colSpan={6} text={t("admin.reports.appealsEmpty")} />
                ) : null}
                {appeals.map((appeal) => (
                  <tr key={appeal.id} className="hover:bg-white/5">
                    <td className={CELL_CLASS}>{appeal.appellant_name || t("admin.unknown")}</td>
                    <td className={CELL_CLASS}>
                      <span className="block text-[11px] text-muted">
                        {appeal.target_type ? t(targetTypeLabelKey(appeal.target_type)) : t("admin.unknown")} ·{" "}
                        {appeal.reason ? t(reasonLabelKey(appeal.reason)) : t("admin.unknown")}
                      </span>
                      <span className="mt-1 block font-mono text-[11px]">{appeal.report_id}</span>
                    </td>
                    <td className={CELL_CLASS}>
                      <span className="block max-w-md whitespace-pre-wrap break-words">{appeal.body}</span>
                    </td>
                    <td className={CELL_CLASS}>{formatDateTime(appeal.created_at, locale)}</td>
                    <td className={CELL_CLASS}>
                      <Badge tone={appealTone(appeal.status)}>{t(appealStatusLabelKey(appeal.status))}</Badge>
                    </td>
                    <td className={CELL_CLASS}>
                      <Button
                        type="button"
                        disabled={appeal.status !== "pending"}
                        onClick={() => {
                          setAppealOpen(appeal);
                          setAppealAction("");
                          setAppealNote("");
                        }}
                      >
                        {t("admin.reports.appealReview")}
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          <Pagination
            page={appealPage}
            pages={appealPages}
            onChange={setAppealPage}
            disabled={appealLoading}
            labels={{ prev: t("admin.prev"), next: t("admin.next"), info: t("admin.pageInfo", { page: appealPage, pages: appealPages }) }}
          />
        </div>
      </Card>

      {detailLoading ? (
        <Modal title={t("admin.reports.detailTitle")} onClose={() => setDetail(null)} wide>
          <p className="text-xs text-muted">{t("admin.loading")}</p>
        </Modal>
      ) : null}

      {detail ? (
        <Modal
          title={t("admin.reports.detailTitle")}
          onClose={() => setDetail(null)}
          wide
          footer={
            <>
              <Button type="button" onClick={() => setDetail(null)} disabled={busy}>
                {t("admin.close")}
              </Button>
              <Button type="button" onClick={() => { setPendingAction("accept"); }} disabled={busy || detail.item.status !== "pending"}>
                {t("admin.reports.accept")}
              </Button>
              <Button type="button" variant="danger" onClick={() => setPendingAction("reject")} disabled={busy || detail.item.status === "rejected" || detail.item.status === "resolved"}>
                {t("admin.reports.reject")}
              </Button>
              <Button type="button" variant="primary" onClick={() => setPendingAction("resolve")} disabled={busy || detail.item.status === "rejected" || detail.item.status === "resolved"}>
                {t("admin.reports.resolve")}
              </Button>
            </>
          }
        >
          <div className="space-y-3">
            {detailError ? <Notice kind="err" text={detailError} onClose={() => setDetailError("")} /> : null}
            <dl className="grid gap-2 text-xs sm:grid-cols-2">
              <div>
                <dt className="text-[11px] text-muted">{t("admin.reports.colTarget")}</dt>
                <dd className="text-ink">
                  {t(targetTypeLabelKey(detail.item.target_type))} · <span className="font-mono">{detail.item.target_id}</span>
                </dd>
              </div>
              <div>
                <dt className="text-[11px] text-muted">{t("admin.reports.colReason")}</dt>
                <dd className="text-ink">{t(reasonLabelKey(detail.item.reason))}</dd>
              </div>
              <div>
                <dt className="text-[11px] text-muted">{t("admin.reports.colReporter")}</dt>
                <dd className="text-ink">{detail.item.reporter_name || t("admin.unknown")}</dd>
              </div>
              <div>
                <dt className="text-[11px] text-muted">{t("admin.reports.colStatus")}</dt>
                <dd className="text-ink">
                  <Badge tone={statusTone(detail.item.status)}>{t(statusLabelKey(detail.item.status))}</Badge>
                </dd>
              </div>
              <div>
                <dt className="text-[11px] text-muted">{t("admin.reports.targetAuthor")}</dt>
                <dd className="text-ink">{detail.item.target_author_name || t("admin.reports.targetAuthorUnknown")}</dd>
              </div>
              <div>
                <dt className="text-[11px] text-muted">{t("admin.reports.contentState")}</dt>
                <dd className="text-ink">{detail.target_present ? t("admin.reports.contentPresent") : t("admin.reports.contentGone")}</dd>
              </div>
            </dl>

            {detail.item.detail ? (
              <div>
                <p className="text-[11px] text-muted">{t("admin.reports.detailLabel")}</p>
                <p className="mt-1 whitespace-pre-wrap break-words text-xs text-ink">{detail.item.detail}</p>
              </div>
            ) : null}
            {detail.item.evidence_url ? (
              <div>
                <p className="text-[11px] text-muted">{t("admin.reports.evidenceLabel")}</p>
                <a className="mt-1 block break-all text-xs text-accent hover:underline" href={detail.item.evidence_url} target="_blank" rel="noreferrer">
                  {detail.item.evidence_url}
                </a>
              </div>
            ) : null}

            <div>
              <p className="text-[11px] text-muted">{t("admin.reports.timeline")}</p>
              <ul className="mt-1 space-y-1 text-xs text-ink">
                {detail.events.length === 0 ? <li className="text-muted">{t("admin.reports.timelineEmpty")}</li> : null}
                {detail.events.map((event, index) => (
                  <li key={index} className="flex flex-wrap items-baseline gap-2">
                    <span className="text-muted">{formatDateTime(event.at, locale)}</span>
                    <span>{t(eventLabelKey(event.kind))}</span>
                    <span className="text-muted">{event.actor_name || t("admin.unknown")}</span>
                    {event.note ? <span className="text-muted">— {event.note}</span> : null}
                  </li>
                ))}
              </ul>
            </div>

            {detail.appeals.length > 0 ? (
              <div>
                <p className="text-[11px] text-muted">{t("admin.reports.appealsTitle")}</p>
                <ul className="mt-1 space-y-2 text-xs text-ink">
                  {detail.appeals.map((appeal) => (
                    <li key={appeal.id} className="rounded-lg border border-line px-3 py-2">
                      <div className="flex flex-wrap items-center gap-2">
                        <Badge tone={appealTone(appeal.status)}>{t(appealStatusLabelKey(appeal.status))}</Badge>
                        <span>{appeal.appellant_name || t("admin.unknown")}</span>
                        <span className="text-muted">{formatDateTime(appeal.created_at, locale)}</span>
                      </div>
                      <p className="mt-1 whitespace-pre-wrap break-words">{appeal.body}</p>
                      {appeal.review_note ? <p className="mt-1 text-muted">{appeal.review_note}</p> : null}
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}

            {detail.item.status !== "rejected" && detail.item.status !== "resolved" ? (
              <div className="space-y-3 rounded-lg border border-line px-3 py-3">
                <Field label={t("admin.reports.noteLabel")} hint={t("admin.reports.noteHint")}>
                  <TextInput value={note} onChange={(e) => setNote(e.target.value)} placeholder={t("admin.reports.notePlaceholder")} />
                </Field>
                <Field label={t("admin.reports.enforcementLabel")} hint={t("admin.reports.enforcementHint")}>
                  <Select value={enforcement} onChange={(e) => setEnforcement(e.target.value)}>
                    {allowedEnforcements(detail.item).map((value) => (
                      <option key={value} value={value}>
                        {t(enforcementLabelKey(value))}
                      </option>
                    ))}
                  </Select>
                </Field>
                {enforcement === "content_removed" ? (
                  <p className="text-[11px] leading-relaxed text-muted">
                    {mayRemoveContent(detail.item)
                      ? t(detail.target_present ? "admin.reports.removeHint" : "admin.reports.removeHintGone")
                      : t("admin.reports.removeUnsupported")}
                  </p>
                ) : null}
                {enforcement === "user_banned" ? (
                  <p className="text-[11px] leading-relaxed text-muted">{t("admin.reports.banHint")}</p>
                ) : null}
              </div>
            ) : null}
          </div>
        </Modal>
      ) : null}

      {pendingAction === "reject" ? (
        <ConfirmDialog
          title={t("admin.reports.reject")}
          body={t("admin.reports.confirmReject")}
          confirmLabel={t("admin.reports.reject")}
          cancelLabel={t("admin.cancel")}
          danger
          busy={busy}
          onClose={() => setPendingAction("")}
          onConfirm={runAction}
        />
      ) : null}

      {pendingAction === "accept" ? (
        <ConfirmDialog
          title={t("admin.reports.accept")}
          body={t("admin.reports.confirmAccept")}
          confirmLabel={t("admin.reports.accept")}
          cancelLabel={t("admin.cancel")}
          busy={busy}
          onClose={() => setPendingAction("")}
          onConfirm={runAction}
        />
      ) : null}

      {pendingAction === "resolve" ? (
        <ConfirmDialog
          title={t("admin.reports.resolve")}
          body={enforcement === "content_removed" ? t("admin.reports.confirmResolveRemove") : t("admin.reports.confirmResolve")}
          confirmLabel={t("admin.reports.resolve")}
          cancelLabel={t("admin.cancel")}
          danger={enforcement === "content_removed"}
          busy={busy}
          onClose={() => setPendingAction("")}
          onConfirm={runAction}
        />
      ) : null}

      {/* 申诉详情：选了"采用/驳回"后先收起详情弹窗，让确认框独占一层。 */}
      {appealOpen && appealAction === "" ? (
        <Modal
          title={t("admin.reports.appealDetailTitle")}
          onClose={() => setAppealOpen(null)}
          footer={
            <>
              <Button type="button" onClick={() => setAppealOpen(null)}>
                {t("admin.close")}
              </Button>
              <Button type="button" variant="danger" onClick={() => setAppealAction("rejected")}>
                {t("admin.reports.appealReject")}
              </Button>
              <Button type="button" variant="primary" onClick={() => setAppealAction("accepted")}>
                {t("admin.reports.appealAdopt")}
              </Button>
            </>
          }
        >
          <div className="space-y-3">
            <p className="whitespace-pre-wrap break-words text-xs text-ink">{appealOpen.body}</p>
            <Field label={t("admin.reports.appealNoteLabel")} hint={t("admin.reports.noteHint")}>
              <TextInput value={appealNote} onChange={(e) => setAppealNote(e.target.value)} placeholder={t("admin.reports.notePlaceholder")} />
            </Field>
          </div>
        </Modal>
      ) : null}

      {appealOpen && appealAction === "accepted" ? (
        <ConfirmDialog
          title={t("admin.reports.appealAdopt")}
          body={t("admin.reports.confirmAppealAdopt")}
          confirmLabel={t("admin.reports.appealAdopt")}
          cancelLabel={t("admin.cancel")}
          busy={appealBusy}
          onClose={() => setAppealAction("")}
          onConfirm={runAppealAction}
        />
      ) : null}

      {appealOpen && appealAction === "rejected" ? (
        <ConfirmDialog
          title={t("admin.reports.appealReject")}
          body={t("admin.reports.confirmAppealReject")}
          confirmLabel={t("admin.reports.appealReject")}
          cancelLabel={t("admin.cancel")}
          danger
          busy={appealBusy}
          onClose={() => setAppealAction("")}
          onConfirm={runAppealAction}
        />
      ) : null}
    </div>
  );
}
