"use client";

import React, { createContext, useCallback, useContext, useEffect, useMemo, useState } from "react";
import { getCurrentUser, type SessionUser } from "./api/session";
import { toErrorLike, type ErrorLike } from "./errors";
import { can as canCode } from "./permissions";

type Status = "loading" | "anonymous" | "ready" | "error";

type Context = {
  status: Status;
  user: SessionUser | null;
  error: ErrorLike | null;
  reload: () => void;
  /** 与服务端 internal/auth/permission.go 的 Can 同口径（见 lib/permissions.ts）。 */
  can: (code: string) => boolean;
};

const SessionContext = createContext<Context>({
  status: "loading",
  user: null,
  error: null,
  reload: () => {},
  can: () => false,
});

export function SessionProvider({ children }: { children: React.ReactNode }) {
  const [status, setStatus] = useState<Status>("loading");
  const [user, setUser] = useState<SessionUser | null>(null);
  const [error, setError] = useState<ErrorLike | null>(null);
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    let alive = true;
    setStatus("loading");
    setError(null);
    getCurrentUser()
      .then((me) => {
        if (!alive) return;
        setUser(me);
        setStatus(me ? "ready" : "anonymous");
      })
      .catch((err) => {
        if (!alive) return;
        // 取不到身份时**不放行任何权限**：查询失败不等于"是管理员"。
        setUser(null);
        setError(toErrorLike(err));
        setStatus("error");
      });
    return () => {
      alive = false;
    };
  }, [nonce]);

  const reload = useCallback(() => setNonce((n) => n + 1), []);
  const can = useCallback((code: string) => (status === "ready" ? canCode(user, code) : false), [status, user]);

  const value = useMemo<Context>(() => ({ status, user, error, reload, can }), [status, user, error, reload, can]);
  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): Context {
  return useContext(SessionContext);
}
