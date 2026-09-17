// 时间展示：只依赖 Intl，locale 直接复用四语代码（都是合法的 BCP-47 标签）。
export function formatDateTime(value: string | undefined | null, locale: string): string {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  try {
    return new Intl.DateTimeFormat(locale, {
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
    }).format(date);
  } catch {
    return date.toISOString();
  }
}

export function formatNumber(value: number | undefined | null, locale: string): string {
  if (typeof value !== "number" || Number.isNaN(value)) return "0";
  try {
    return new Intl.NumberFormat(locale).format(value);
  } catch {
    return String(value);
  }
}
