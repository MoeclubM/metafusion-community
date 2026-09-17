// 语种清单与归一化：四语铁律（zh-CN / zh-TW / ja-JP / en-US）来自主仓库约定，
// 与论坛服务 boardLocales、主前端 i18n/routing.ts 同一份清单与同一枚 cookie 名。
// 后台与站点共用 NEXT_LOCALE，切换语言在两边都生效。
export const locales = ["zh-CN", "zh-TW", "ja-JP", "en-US"] as const;
export type Locale = (typeof locales)[number];
export const defaultLocale: Locale = "zh-CN";
export const localeCookieName = "NEXT_LOCALE";

const validLocales = new Set<string>(locales);

export function normalizeLocale(input?: string | null): Locale {
  if (!input) return defaultLocale;
  const v = input.trim();
  if (validLocales.has(v)) return v as Locale;
  const low = v.toLowerCase().replace(/_/g, "-");
  if (low.startsWith("ja")) return "ja-JP";
  if (low.startsWith("zh-tw") || low.startsWith("zh-hk") || low.includes("hant")) return "zh-TW";
  if (low.startsWith("zh")) return "zh-CN";
  if (low.startsWith("en")) return "en-US";
  return defaultLocale;
}

export function readLocaleCookie(): string | null {
  if (typeof document === "undefined") return null;
  const m = document.cookie.match(new RegExp(`(?:^|;\\s*)${localeCookieName}=([^;]+)`));
  return m ? decodeURIComponent(m[1]!) : null;
}

export function detectBrowserLocale(): Locale {
  if (typeof navigator === "undefined") return defaultLocale;
  for (const tag of navigator.languages ?? [navigator.language]) {
    if (tag && validLocales.has(tag)) return tag as Locale;
  }
  return normalizeLocale(navigator.language);
}

export const localeLabels: Record<Locale, string> = {
  "zh-CN": "简体中文",
  "zh-TW": "繁體中文",
  "ja-JP": "日本語",
  "en-US": "English",
};
