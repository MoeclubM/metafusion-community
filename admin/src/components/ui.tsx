"use client";

// 后台的展示件：只做样式与可访问性，不含任何业务判定（权限码判定在 lib/permissions.ts，
// 错误文案在 lib/errors.ts）。所有文案都由调用方传入，不在组件里写死语言。

import React from "react";
import { useI18n } from "@/lib/i18n/provider";

export const HEAD_CLASS = "px-3 py-2 text-left text-[11px] font-semibold uppercase tracking-wide text-muted";
export const CELL_CLASS = "px-3 py-2 text-xs align-top text-ink";

export function Card(props: { title: string; desc?: string; actions?: React.ReactNode; children: React.ReactNode }) {
  return (
    <section className="rounded-xl border border-line bg-panel/80">
      <header className="flex flex-wrap items-start justify-between gap-3 border-b border-line px-4 py-3">
        <div>
          <h2 className="text-sm font-semibold text-ink">{props.title}</h2>
          {props.desc ? <p className="mt-1 max-w-3xl text-xs leading-relaxed text-muted">{props.desc}</p> : null}
        </div>
        {props.actions ? <div className="flex items-center gap-2">{props.actions}</div> : null}
      </header>
      <div className="p-4">{props.children}</div>
    </section>
  );
}

const NOTICE_TONE: Record<string, string> = {
  ok: "border-emerald-500/40 bg-emerald-500/10 text-ink",
  err: "border-danger/40 bg-danger/10 text-ink",
  info: "border-accent/40 bg-accent/10 text-ink",
};

export function Notice(props: { kind: "ok" | "err" | "info"; text: string; onClose?: () => void }) {
  const { t } = useI18n();
  if (!props.text) return null;
  return (
    <div role={props.kind === "err" ? "alert" : "status"} className={"flex items-start justify-between gap-3 rounded-lg border px-3 py-2 text-xs " + NOTICE_TONE[props.kind]}>
      <p className="min-w-0 break-words leading-relaxed">{props.text}</p>
      {props.onClose ? (
        <button type="button" onClick={props.onClose} className="shrink-0 opacity-70 hover:opacity-100" aria-label={t("admin.close")}>
          ×
        </button>
      ) : null}
    </div>
  );
}

const BADGE_TONE: Record<string, string> = {
  ok: "bg-emerald-500/15 text-ink",
  off: "bg-white/5 text-muted",
  info: "bg-accent/15 text-ink",
  warn: "bg-amber-500/15 text-ink",
};

export function Badge(props: { tone?: "ok" | "off" | "info" | "warn"; children: React.ReactNode }) {
  return (
    <span className={"inline-flex items-center rounded px-1.5 py-0.5 text-[10px] font-medium " + BADGE_TONE[props.tone ?? "off"]}>
      {props.children}
    </span>
  );
}

const BUTTON_TONE: Record<string, string> = {
  primary: "bg-accent text-white hover:bg-accent/85 disabled:bg-accent/40",
  ghost: "border border-line text-ink hover:bg-white/5",
  danger: "border border-danger/50 text-ink hover:bg-danger/15",
};

export function Button(
  props: React.ButtonHTMLAttributes<HTMLButtonElement> & { variant?: "primary" | "ghost" | "danger" }
) {
  const { variant = "ghost", className, ...rest } = props;
  return (
    <button
      {...rest}
      className={
        "inline-flex items-center gap-1 rounded-lg px-2.5 py-1.5 text-xs font-medium transition disabled:cursor-not-allowed disabled:opacity-50 " +
        BUTTON_TONE[variant] +
        " " +
        (className ?? "")
      }
    />
  );
}

export function TextInput(props: React.InputHTMLAttributes<HTMLInputElement>) {
  const { className, ...rest } = props;
  return (
    <input
      {...rest}
      className={
        "w-full rounded-lg border border-line bg-surface px-2.5 py-1.5 text-xs text-ink placeholder:text-muted/70 focus:border-accent focus:outline-none " +
        (className ?? "")
      }
    />
  );
}

export function Select(props: React.SelectHTMLAttributes<HTMLSelectElement>) {
  const { className, children, ...rest } = props;
  return (
    <select
      {...rest}
      className={
        "w-full rounded-lg border border-line bg-surface px-2.5 py-1.5 text-xs text-ink focus:border-accent focus:outline-none " +
        (className ?? "")
      }
    >
      {children}
    </select>
  );
}

export function Field(props: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <label className="block space-y-1">
      <span className="block text-[11px] font-medium text-muted">{props.label}</span>
      {props.children}
      {props.hint ? <span className="block text-[11px] leading-relaxed text-muted/80">{props.hint}</span> : null}
    </label>
  );
}

export function Toggle(props: { checked: boolean; onChange: (next: boolean) => void; label: string; hint?: string; disabled?: boolean }) {
  return (
    <div className="space-y-1">
      <label className="flex items-center gap-2 text-xs text-ink">
        <input
          type="checkbox"
          checked={props.checked}
          disabled={props.disabled}
          onChange={(e) => props.onChange(e.target.checked)}
          className="h-3.5 w-3.5 accent-accent"
        />
        {props.label}
      </label>
      {props.hint ? <p className="text-[11px] leading-relaxed text-muted/80">{props.hint}</p> : null}
    </div>
  );
}

export function EmptyRow(props: { colSpan: number; text: string }) {
  return (
    <tr>
      <td colSpan={props.colSpan} className="px-3 py-8 text-center text-xs text-muted">
        {props.text}
      </td>
    </tr>
  );
}

export function LoadingRow(props: { colSpan: number; text: string }) {
  return (
    <tr>
      <td colSpan={props.colSpan} className="px-3 py-8 text-center text-xs text-muted">
        {props.text}
      </td>
    </tr>
  );
}

export function Pagination(props: {
  page: number;
  pages: number;
  onChange: (next: number) => void;
  labels: { prev: string; next: string; info: string };
  disabled?: boolean;
}) {
  const atStart = props.page <= 1;
  const atEnd = props.page >= props.pages;
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 pt-3">
      <span className="text-[11px] text-muted">{props.labels.info}</span>
      <div className="flex items-center gap-2">
        <Button type="button" disabled={props.disabled || atStart} onClick={() => props.onChange(props.page - 1)}>
          {props.labels.prev}
        </Button>
        <Button type="button" disabled={props.disabled || atEnd} onClick={() => props.onChange(props.page + 1)}>
          {props.labels.next}
        </Button>
      </div>
    </div>
  );
}

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
const dialogStack: HTMLElement[] = [];
let originalOverflow = "";

export function Modal(props: { title: string; onClose: () => void; children: React.ReactNode; footer?: React.ReactNode; wide?: boolean; busy?: boolean }) {
  const {onClose, title, children, busy = false} = props;
  const open = true;
  const maxWidth = props.wide ? "max-w-3xl" : "max-w-xl";
  const { t } = useI18n();
  const titleId = React.useId();
  const panel = React.useRef<HTMLDivElement>(null);
  const latest = React.useRef({onClose, busy});
  latest.current = {onClose, busy};
  const close = () => { if (!latest.current.busy) latest.current.onClose(); };
  React.useEffect(() => {
    if (!open || !panel.current) return;
    const element = panel.current;
    const previous = document.activeElement as HTMLElement | null;
    dialogStack.push(element);
    if (dialogStack.length === 1) { originalOverflow = document.body.style.overflow; document.body.style.overflow = "hidden"; }
    const items = () => Array.from(element.querySelectorAll<HTMLElement>(FOCUSABLE));
    (items()[0] ?? element).focus();
    const onKey = (e: KeyboardEvent) => {
      if (dialogStack.at(-1) !== element) return;
      if (e.key === "Escape") { e.preventDefault(); close(); }
      if (e.key !== "Tab") return;
      const all = items(); const first = all[0]; const last = all.at(-1);
      if (!first || !last) { e.preventDefault(); element.focus(); }
      else if (e.shiftKey && (document.activeElement === first || document.activeElement === element || !element.contains(document.activeElement))) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    };
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("keydown", onKey);
      const index = dialogStack.indexOf(element); if (index >= 0) dialogStack.splice(index, 1);
      if (!dialogStack.length) document.body.style.overflow = originalOverflow;
      if (previous?.isConnected) previous.focus();
    };
  }, [open]);
  if (!open) return null;
  return <div className="fixed inset-0 z-50 bg-black/70 backdrop-blur-sm flex items-center justify-center p-4" onMouseDown={(e) => { if (e.target === e.currentTarget) close(); }}>
    <div ref={panel} role="dialog" aria-modal="true" aria-labelledby={titleId} tabIndex={-1}
      className={`w-full ${maxWidth} min-w-0 rounded-xl border border-line bg-panel p-5 sm:p-6 space-y-4 shadow-xl max-h-[90vh] overflow-y-auto outline-none`}>
      <div className="flex items-center justify-between gap-3 border-b border-line/60 pb-3">
        <h3 id={titleId} className="min-w-0 break-words text-sm font-semibold text-ink flex items-center gap-2">{title}</h3>
        <button type="button" onClick={close} disabled={busy} aria-label={t("admin.close")} className="shrink-0 rounded-lg p-2 text-muted hover:bg-accent/10 disabled:opacity-50"><span aria-hidden="true">×</span></button>
      </div>
      <div className="space-y-4">{children}</div>
      {props.footer ? <footer className="flex flex-wrap justify-end gap-2 border-t border-line pt-3">{props.footer}</footer> : null}
    </div>
  </div>;
}

export function ConfirmDialog(props: {
  title: string;
  body: string;
  confirmLabel: string;
  cancelLabel: string;
  busy?: boolean;
  danger?: boolean;
  onConfirm: () => void;
  onClose: () => void;
}) {
  return (
    <Modal
      title={props.title}
      onClose={props.onClose}
      busy={props.busy}
      footer={
        <>
          <Button type="button" onClick={props.onClose} disabled={props.busy}>
            {props.cancelLabel}
          </Button>
          <Button type="button" variant={props.danger ? "danger" : "primary"} onClick={props.onConfirm} disabled={props.busy}>
            {props.confirmLabel}
          </Button>
        </>
      }
    >
      <p className="text-xs leading-relaxed text-ink">{props.body}</p>
    </Modal>
  );
}
