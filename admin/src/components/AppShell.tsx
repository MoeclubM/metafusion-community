"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
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
import { ThemeModeSwitcher } from "./ThemeModeSwitcher";

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
  const { t, locale } = useI18n();
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
      <header className="sticky top-0 z-30 border-b border-line bg-panel/95 backdrop-blur">
        <div className="mx-auto flex h-14 w-full max-w-[80rem] items-center justify-between gap-3 px-4 sm:px-6">
          <div className="flex min-w-0 items-center gap-3">
            <a href="/admin" className="inline-flex shrink-0 items-center gap-1 text-xs text-muted hover:text-ink"><span aria-hidden="true">←</span>{t("admin.backToHub")}</a>
            <span className="text-muted">/</span>
            <h1 className="min-w-0 truncate text-sm font-semibold text-ink">{t("admin.appTitle")}</h1>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            <LocaleMenu />
            <ThemeModeSwitcher />
            {user ? <span className="hidden max-w-[9rem] truncate rounded border border-accent/30 bg-accent/15 px-2 py-0.5 font-mono text-xs text-accent sm:inline-flex" title={user.username}>{user.username}</span> : null}
          </div>
        </div>
      </header>

      <div className="mx-auto flex w-full max-w-[80rem] flex-1 flex-col gap-6 px-4 py-6 sm:px-6 lg:flex-row">
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
          {status === "ready" ? <div className="flex justify-end"><Button type="button" onClick={refreshAll}>{t("admin.refresh")}</Button></div> : null}
          {status === "ready" && user ? (
            <details className="rounded-xl border border-line bg-surface px-4 py-3 text-xs">
              <summary className="cursor-pointer font-medium text-ink">
                {t("admin.session.title")} · {user.display_name || user.username}
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
      <footer className="mx-auto w-full max-w-[80rem] border-t border-line px-4 py-4 text-[11px] leading-relaxed text-muted sm:px-6">{t("admin.footer")}</footer>
    </div>
  );
}

function LocaleMenu() {
  const { t, locale, setLocale } = useI18n();
  const [open, setOpen] = useState(false);
  const container = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onPointer = (event: MouseEvent) => { if (!container.current?.contains(event.target as Node)) setOpen(false); };
    const onKey = (event: KeyboardEvent) => { if (event.key === "Escape") setOpen(false); };
    document.addEventListener("mousedown", onPointer);
    document.addEventListener("keydown", onKey);
    return () => { document.removeEventListener("mousedown", onPointer); document.removeEventListener("keydown", onKey); };
  }, [open]);
  return <div className="relative" ref={container}>
    <button type="button" aria-label={t("admin.localeLabel")} title={t("admin.localeLabel")} aria-expanded={open} aria-haspopup="menu" onClick={() => setOpen(!open)} className="grid h-9 w-9 place-items-center rounded-full border border-line bg-panel text-ink hover:border-accent"><span aria-hidden="true">文</span></button>
    {open ? <div role="menu" aria-label={t("admin.localeLabel")} className="absolute right-0 z-50 mt-2 w-44 rounded-xl border border-line bg-panel p-1.5 shadow-xl">
      {locales.map((code) => <button key={code} type="button" role="menuitemradio" aria-checked={locale === code} onClick={() => { setLocale(code as Locale); setOpen(false); }} className={"flex w-full items-center justify-between rounded-lg px-3 py-2 text-left text-xs hover:bg-accent/10 " + (locale === code ? "font-semibold text-accent" : "text-ink")}>{localeLabels[code]}{locale === code ? "✓" : null}</button>)}
    </div> : null}
  </div>;
}
