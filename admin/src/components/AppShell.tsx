"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { useI18n } from "@/lib/i18n/provider";
import { localeLabels, locales, type Locale } from "@/lib/i18n/routing";
import { useSession } from "@/lib/session-context";
import { describeError, formatLocaleList } from "@/lib/errors";
import { LOGIN_PATH, signInHrefOnce } from "@/lib/sign-in";
import { COMMUNITY_BOARD_MANAGE, COMMUNITY_POST_MODERATE, COMMUNITY_TOPIC_PIN } from "@/lib/permissions";
import { BoardsPanel } from "./BoardsPanel";
import { TopicsPanel } from "./TopicsPanel";
import { PostsPanel } from "./PostsPanel";
import { Badge, Button, Card, Notice } from "./ui";

type TabId = "boards" | "topics" | "posts";

// 页签与权限码的对应：与服务端各端点的闸门一致（板块配置 / 置顶 / 治理内容）。
const TABS: { id: TabId; labelKey: string; codes: string[] }[] = [
  { id: "boards", labelKey: "admin.tab.boards", codes: [COMMUNITY_BOARD_MANAGE] },
  { id: "topics", labelKey: "admin.tab.topics", codes: [COMMUNITY_TOPIC_PIN, COMMUNITY_POST_MODERATE] },
  { id: "posts", labelKey: "admin.tab.posts", codes: [COMMUNITY_POST_MODERATE] },
];

function readHash(): string {
  if (typeof window === "undefined") return "";
  return window.location.hash.replace(/^#/, "");
}

export function AppShell() {
  const { t, locale, setLocale } = useI18n();
  const { status, user, error, reload, can } = useSession();
  const [tab, setTab] = useState<TabId | "">("");
  const [reloadKey, setReloadKey] = useState(0);
  // 首帧恒定 /login（服务端没有 location），挂载后再补上一次性算出的回跳参数。
  const [signInHref, setSignInHref] = useState<string>(LOGIN_PATH);

  useEffect(() => {
    setSignInHref(signInHrefOnce());
  }, []);

  const allowed = useMemo(() => {
    const map: Record<string, boolean> = {};
    for (const item of TABS) map[item.id] = item.codes.some((code) => can(code));
    return map;
  }, [can]);

  // 页签用 hash 记忆：刷新后回到同一页，也不经过任何服务端状态。
  useEffect(() => {
    const apply = () => {
      const wanted = readHash() as TabId;
      if (wanted && allowed[wanted]) setTab(wanted);
      else setTab((current) => (current && allowed[current] ? current : (TABS.find((x) => allowed[x.id])?.id ?? "")));
    };
    apply();
    window.addEventListener("hashchange", apply);
    return () => window.removeEventListener("hashchange", apply);
  }, [allowed]);

  const selectTab = useCallback((next: TabId) => {
    setTab(next);
    if (typeof window !== "undefined") window.history.replaceState(null, "", "#" + next);
  }, []);

  const refreshAll = useCallback(() => {
    reload();
    setReloadKey((n) => n + 1);
  }, [reload]);

  const anyAllowed = TABS.some((item) => allowed[item.id]);
  const permissionSummary = useMemo(() => {
    const perms = user?.permissions ?? [];
    if (perms.includes("*")) return t("admin.session.permissionsWildcard");
    return t("admin.session.permissionsCount", { count: perms.length });
  }, [user, t]);

  return (
    <div className="mx-auto flex min-h-screen max-w-6xl flex-col gap-5 px-4 py-8">
      <header className="space-y-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h1 className="text-lg font-semibold text-ink">{t("admin.appTitle")}</h1>
            <p className="mt-1 text-xs text-muted">{t("admin.appSubtitle")}</p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <label className="flex items-center gap-2 text-[11px] text-muted">
              {t("admin.localeLabel")}
              <select
                value={locale}
                onChange={(e) => setLocale(e.target.value as Locale)}
                className="rounded-lg border border-line bg-surface px-2 py-1 text-xs text-ink focus:border-accent focus:outline-none"
              >
                {locales.map((code) => (
                  <option key={code} value={code}>
                    {localeLabels[code]}
                  </option>
                ))}
              </select>
            </label>
            <Button type="button" onClick={refreshAll} disabled={status === "loading"}>
              {t("admin.refresh")}
            </Button>
          </div>
        </div>

        <Card
          title={t("admin.session.title")}
          desc={t("admin.session.signInHint")}
          actions={<Badge tone="info">{t("admin.serviceLabel")}</Badge>}
        >
          {status === "loading" ? <p className="text-xs text-muted">{t("admin.loading")}</p> : null}
          {status === "anonymous" ? (
            <div className="flex flex-wrap items-center justify-between gap-3">
              <p className="text-xs text-muted">{t("admin.session.anonymous")}</p>
              <a className="text-xs" href={signInHref}>
                {t("admin.session.goSignIn")}
              </a>
            </div>
          ) : null}
          {status === "error" && error ? (
            <div className="space-y-2">
              <Notice kind="err" text={t("admin.session.loadFailed", { message: describeError(error, t, locale) })} />
              <Button type="button" onClick={reload}>
                {t("admin.retry")}
              </Button>
            </div>
          ) : null}
          {status === "ready" && user ? (
            <dl className="grid gap-2 text-xs sm:grid-cols-2">
              <div>
                <dt className="text-[11px] text-muted">{t("admin.session.labelName")}</dt>
                <dd className="text-ink">{user.display_name || user.username}</dd>
              </div>
              <div>
                <dt className="text-[11px] text-muted">{t("admin.session.labelRole")}</dt>
                <dd className="text-ink">{user.role || t("admin.unknown")}</dd>
              </div>
              <div>
                <dt className="text-[11px] text-muted">{t("admin.session.labelGroups")}</dt>
                <dd className="text-ink">{(user.groups ?? []).join(", ") || t("admin.session.groupsNone")}</dd>
              </div>
              <div>
                <dt className="text-[11px] text-muted">{t("admin.session.labelPermissions")}</dt>
                <dd className="text-ink">{permissionSummary}</dd>
              </div>
            </dl>
          ) : null}
        </Card>

        {status === "ready" ? (
          <nav className="flex flex-wrap gap-2">
            {TABS.filter((item) => allowed[item.id]).map((item) => (
              <button
                key={item.id}
                type="button"
                onClick={() => selectTab(item.id)}
                className={
                  "rounded-lg border px-3 py-1.5 text-xs font-medium transition " +
                  (tab === item.id ? "border-accent bg-accent/15 text-ink" : "border-line text-muted hover:text-ink")
                }
              >
                {t(item.labelKey)}
              </button>
            ))}
          </nav>
        ) : null}
      </header>

      <main className="flex-1">
        {status === "ready" && !anyAllowed ? (
          <Card title={t("admin.denied.title")} desc={t("admin.denied.serviceNote")}>
            <Notice
              kind="err"
              text={t("admin.denied.body", {
                codes: formatLocaleList(
                  TABS.flatMap((item) => item.codes),
                  locale
                ),
              })}
            />
          </Card>
        ) : null}
        {status === "ready" && anyAllowed && tab === "boards" && allowed.boards ? <BoardsPanel reloadKey={reloadKey} /> : null}
        {status === "ready" && anyAllowed && tab === "topics" && allowed.topics ? <TopicsPanel reloadKey={reloadKey} /> : null}
        {status === "ready" && anyAllowed && tab === "posts" && allowed.posts ? <PostsPanel reloadKey={reloadKey} /> : null}
      </main>

      <footer className="border-t border-line pt-3 text-[11px] leading-relaxed text-muted">{t("admin.footer")}</footer>
    </div>
  );
}
