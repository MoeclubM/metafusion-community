"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { useI18n } from "@/lib/i18n/provider";
import { localeLabels, locales, type Locale } from "@/lib/i18n/routing";
import { useSession } from "@/lib/session-context";
import { describeError, formatLocaleList } from "@/lib/errors";
import { LOGIN_PATH, signInHrefOnce } from "@/lib/sign-in";
import { COMMUNITY_BOARD_MANAGE, COMMUNITY_POST_MODERATE, COMMUNITY_REPORT_REVIEW, COMMUNITY_TOPIC_PIN } from "@/lib/permissions";
import { BoardsPanel } from "./BoardsPanel";
import { TopicsPanel } from "./TopicsPanel";
import { PostsPanel } from "./PostsPanel";
import { ReportsPanel } from "./ReportsPanel";
import { Button, Card, Notice } from "./ui";

type TabId = "boards" | "topics" | "posts" | "reports";
const TABS: { id: TabId; labelKey: string; codes: string[] }[] = [
  { id: "reports", labelKey: "admin.tab.reports", codes: [COMMUNITY_REPORT_REVIEW] },
  { id: "topics", labelKey: "admin.tab.topics", codes: [COMMUNITY_TOPIC_PIN, COMMUNITY_POST_MODERATE] },
  { id: "posts", labelKey: "admin.tab.posts", codes: [COMMUNITY_POST_MODERATE] },
  { id: "boards", labelKey: "admin.tab.boards", codes: [COMMUNITY_BOARD_MANAGE] },
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
  const [signInHref, setSignInHref] = useState<string>(LOGIN_PATH);

  useEffect(() => { setSignInHref(signInHrefOnce()); }, []);
  const allowed = useMemo(() => {
    const map: Record<string, boolean> = {};
    for (const item of TABS) map[item.id] = item.codes.some((code) => can(code));
    return map;
  }, [can]);

  useEffect(() => {
    const apply = () => {
      const wanted = readHash() as TabId;
      if (wanted && allowed[wanted]) setTab(wanted);
      else setTab(TABS.find((item) => allowed[item.id])?.id ?? "");
    };
    apply();
    window.addEventListener("hashchange", apply);
    window.addEventListener("popstate", apply);
    return () => { window.removeEventListener("hashchange", apply); window.removeEventListener("popstate", apply); };
  }, [allowed]);

  const selectTab = useCallback((next: TabId) => {
    setTab(next);
    if (typeof window !== "undefined" && window.location.hash !== `#${next}`) window.history.pushState(null, "", `#${next}`);
  }, []);
  const refreshAll = useCallback(() => { reload(); setReloadKey((n) => n + 1); }, [reload]);
  const visibleTabs = TABS.filter((item) => allowed[item.id]);
  const anyAllowed = visibleTabs.length > 0;
  const permissionSummary = useMemo(() => {
    const perms = user?.permissions ?? [];
    if (perms.includes("*")) return t("admin.session.permissionsWildcard");
    return t("admin.session.permissionsCount", { count: perms.length });
  }, [user, t]);

  return (
    <div className="flex min-h-screen flex-col bg-surface text-ink">
      <header className="sticky top-0 z-30 border-b border-line bg-surface/95 backdrop-blur">
        <div className="mx-auto flex max-w-6xl flex-wrap items-center gap-3 px-4 py-3 sm:px-6">
          <div className="min-w-0">
            <h1 className="text-base font-semibold text-ink">{t("admin.appTitle")}</h1>
            <p className="text-xs text-muted">{t("admin.appSubtitle")}</p>
          </div>
          <div className="ml-auto flex flex-wrap items-center gap-2">
            <label className="flex items-center gap-2 text-xs text-muted">
              <span className="sr-only sm:not-sr-only">{t("admin.localeLabel")}</span>
              <select value={locale} onChange={(e) => setLocale(e.target.value as Locale)} className="rounded-lg border border-line bg-surface px-2 py-2 text-xs text-ink focus:border-accent focus:outline-none">
                {locales.map((code) => <option key={code} value={code}>{localeLabels[code]}</option>)}
              </select>
            </label>
            <a href="/admin" className="rounded-lg border border-line px-3 py-2 text-xs text-ink hover:bg-accent/10">{t("admin.backToHub")}</a>
            <Button type="button" onClick={refreshAll} disabled={status === "loading"}>{t("admin.refresh")}</Button>
          </div>
        </div>
      </header>

      <div className="mx-auto flex w-full max-w-6xl flex-1 flex-col gap-6 px-4 py-6 sm:px-6 lg:flex-row">
        {status === "ready" && anyAllowed ? (
          <aside className="w-full shrink-0 lg:w-52">
            <div className="rounded-xl border border-line bg-surface p-3 lg:hidden">
              <label htmlFor="community-admin-section" className="mb-2 block text-xs font-medium text-muted">{t("admin.navLabel")}</label>
              <select id="community-admin-section" value={tab} onChange={(e) => selectTab(e.target.value as TabId)} className="w-full rounded-lg border border-line bg-surface px-3 py-2.5 text-sm text-ink">
                {visibleTabs.map((item) => <option key={item.id} value={item.id}>{t(item.labelKey)}</option>)}
              </select>
            </div>
            <nav aria-label={t("admin.navLabel")} className="sticky top-24 hidden rounded-xl border border-line bg-surface p-2 lg:block">
              {visibleTabs.map((item) => (
                <button key={item.id} type="button" aria-current={tab === item.id ? "page" : undefined} onClick={() => selectTab(item.id)} className={`w-full rounded-lg px-3 py-2.5 text-left text-sm transition-colors ${tab === item.id ? "bg-accent/15 font-semibold text-ink" : "text-muted hover:bg-accent/10 hover:text-ink"}`}>
                  {t(item.labelKey)}
                </button>
              ))}
            </nav>
          </aside>
        ) : null}

        <main className="min-w-0 flex-1 space-y-4">
          {status === "ready" && user ? (
            <details className="rounded-xl border border-line bg-surface px-4 py-3 text-xs">
              <summary className="cursor-pointer font-medium text-ink">
                {t("admin.session.title")} · {user.display_name || user.username} · {user.role || t("admin.unknown")}
              </summary>
              <dl className="mt-3 grid gap-3 border-t border-line pt-3 sm:grid-cols-2">
                <div><dt className="text-muted">{t("admin.session.labelGroups")}</dt><dd className="mt-1 text-ink">{(user.groups ?? []).join(", ") || t("admin.session.groupsNone")}</dd></div>
                <div><dt className="text-muted">{t("admin.session.labelPermissions")}</dt><dd className="mt-1 text-ink">{permissionSummary}</dd></div>
              </dl>
            </details>
          ) : null}
          {status === "loading" ? <Card title={t("admin.session.title")}><p className="text-xs text-muted">{t("admin.loading")}</p></Card> : null}
          {status === "anonymous" ? (
            <Card title={t("admin.session.title")} desc={t("admin.session.signInHint")}>
              <a className="text-xs" href={signInHref}>{t("admin.session.goSignIn")}</a>
            </Card>
          ) : null}
          {status === "error" && error ? (
            <Card title={t("admin.session.title")}>
              <Notice kind="err" text={t("admin.session.loadFailed", { message: describeError(error, t, locale) })} />
              <div className="mt-3"><Button type="button" onClick={reload}>{t("admin.retry")}</Button></div>
            </Card>
          ) : null}
          {status === "ready" && !anyAllowed ? (
            <Card title={t("admin.denied.title")} desc={t("admin.denied.serviceNote")}>
              <Notice kind="err" text={t("admin.denied.body", { codes: formatLocaleList(TABS.flatMap((item) => item.codes), locale) })} />
            </Card>
          ) : null}
          {status === "ready" && anyAllowed && tab === "boards" && allowed.boards ? <BoardsPanel reloadKey={reloadKey} /> : null}
          {status === "ready" && anyAllowed && tab === "topics" && allowed.topics ? <TopicsPanel reloadKey={reloadKey} /> : null}
          {status === "ready" && anyAllowed && tab === "posts" && allowed.posts ? <PostsPanel reloadKey={reloadKey} /> : null}
          {status === "ready" && anyAllowed && tab === "reports" && allowed.reports ? <ReportsPanel reloadKey={reloadKey} /> : null}
        </main>
      </div>
      <footer className="mx-auto w-full max-w-6xl border-t border-line px-4 py-4 text-[11px] leading-relaxed text-muted sm:px-6">{t("admin.footer")}</footer>
    </div>
  );
}
