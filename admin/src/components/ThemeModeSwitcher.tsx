"use client";

import { useEffect, useRef, useState } from "react";
import { useI18n } from "@/lib/i18n/provider";

type Mode = "dark" | "light" | "system";
const KEY = "metafusion_theme_mode";

function apply(mode: Mode) {
  const effective = mode === "system" ? (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light") : mode;
  document.documentElement.setAttribute("data-theme-mode", effective);
  document.documentElement.style.colorScheme = effective;
}

export function ThemeModeSwitcher() {
  const { t } = useI18n();
  const [mode, setMode] = useState<Mode>("dark");
  const [open, setOpen] = useState(false);
  const container = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const sync = () => {
      const value = localStorage.getItem(KEY);
      const next: Mode = value === "light" || value === "system" ? value : "dark";
      setMode(next);
      apply(next);
    };
    sync();
    const media = matchMedia("(prefers-color-scheme: dark)");
    media.addEventListener("change", sync);
    window.addEventListener("storage", sync);
    return () => { media.removeEventListener("change", sync); window.removeEventListener("storage", sync); };
  }, []);
  useEffect(() => {
    if (!open) return;
    const onPointer = (event: MouseEvent) => { if (!container.current?.contains(event.target as Node)) setOpen(false); };
    const onKey = (event: KeyboardEvent) => { if (event.key === "Escape") setOpen(false); };
    document.addEventListener("mousedown", onPointer);
    document.addEventListener("keydown", onKey);
    return () => { document.removeEventListener("mousedown", onPointer); document.removeEventListener("keydown", onKey); };
  }, [open]);
  return <div className="relative" ref={container}>
    <button type="button" aria-label={t("admin.theme.modeLabel")} title={t("admin.theme.modeLabel")} aria-expanded={open} aria-haspopup="menu" onClick={() => setOpen(!open)} className="grid h-9 w-9 place-items-center rounded-full border border-line bg-panel text-ink hover:border-accent"><span aria-hidden="true">{mode === "light" ? "☀" : "☾"}</span></button>
    {open ? <div role="menu" aria-label={t("admin.theme.modeLabel")} className="absolute right-0 z-50 mt-2 w-44 rounded-xl border border-line bg-panel p-1.5 shadow-xl">
      {(["dark", "light", "system"] as const).map((item) => <button key={item} type="button" role="menuitemradio" aria-checked={mode === item} onClick={() => { localStorage.setItem(KEY, item); setMode(item); apply(item); setOpen(false); }} className={"flex w-full items-center justify-between rounded-lg px-3 py-2 text-left text-xs hover:bg-accent/10 " + (mode === item ? "font-semibold text-accent" : "text-ink")}>{t(`admin.theme.${item}`)}{mode === item ? "✓" : null}</button>)}
    </div> : null}
  </div>;
}
