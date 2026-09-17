"use client";

import React, { createContext, useCallback, useContext, useEffect, useMemo, useState } from "react";
import {
  detectBrowserLocale,
  localeCookieName,
  normalizeLocale,
  readLocaleCookie,
  type Locale,
} from "./routing";
import { messagesFor, translate } from "./messages";

export type Translate = (key: string, vars?: Record<string, string | number>) => string;

type Context = {
  locale: Locale;
  t: Translate;
  setLocale: (next: Locale) => void;
};

const I18nContext = createContext<Context>({
  locale: "zh-CN",
  t: (key) => key,
  setLocale: () => {},
});

function writeLocaleCookie(locale: Locale) {
  document.cookie = `${localeCookieName}=${encodeURIComponent(locale)}; Path=/; Max-Age=${60 * 60 * 24 * 365}; SameSite=Lax`;
}

export function I18nProvider({ children }: { children: React.ReactNode }) {
  // 首屏固定 zh-CN（SSR 与首次客户端渲染必须一致，否则 hydration 报错），
  // 挂载后再按 cookie / 浏览器语言纠正一次。
  const [locale, setLocaleState] = useState<Locale>("zh-CN");

  useEffect(() => {
    setLocaleState(normalizeLocale(readLocaleCookie() ?? detectBrowserLocale()));
  }, []);

  useEffect(() => {
    document.documentElement.lang = locale;
  }, [locale]);

  const setLocale = useCallback((next: Locale) => {
    setLocaleState(next);
    writeLocaleCookie(next);
  }, []);

  const messages = useMemo(() => messagesFor(locale), [locale]);
  const t = useCallback<Translate>((key, vars) => translate(messages, key, vars), [messages]);

  return <I18nContext.Provider value={{ locale, t, setLocale }}>{children}</I18nContext.Provider>;
}

export function useI18n(): Context {
  return useContext(I18nContext);
}
