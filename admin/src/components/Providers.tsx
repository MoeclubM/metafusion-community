"use client";

import { I18nProvider } from "@/lib/i18n/provider";
import { SessionProvider } from "@/lib/session-context";

/** 客户端上下文：四语字典 + 当前身份（权限码来自 /api/auth/me）。 */
export function Providers({ children }: { children: React.ReactNode }) {
  return (
    <I18nProvider>
      <SessionProvider>{children}</SessionProvider>
    </I18nProvider>
  );
}
