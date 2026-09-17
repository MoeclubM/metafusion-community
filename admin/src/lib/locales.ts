// 语种清单与错误码里的语种解析。零依赖：本模块被 errors.ts 与 boards.ts 共用，
// 放进任何一边都会形成循环 import（Node 直接跑 .ts 用例时尤其敏感）。
export const BOARD_LOCALES = ["zh-CN", "zh-TW", "ja-JP", "en-US"] as const;
export type BoardLocale = (typeof BOARD_LOCALES)[number];

/** 与目录侧同名的机器码：四语齐备才通过，缺语种时冒号后是逗号分隔的语种清单。 */
const FOUR_LOCALE_MARKER = "four_locale_names_required";

export function parseMissingLocales(raw: string | undefined | null): string[] | null {
  if (!raw) return null;
  const at = raw.indexOf(FOUR_LOCALE_MARKER);
  if (at < 0) return null;
  const rest = raw.slice(at + FOUR_LOCALE_MARKER.length).replace(/^:\s*/, "").trim();
  const list = rest.split(",").map((item) => item.trim()).filter(Boolean);
  return list.length > 0 ? list : null;
}

/** 语种清单的展示分隔：中日文用顿号，拉丁语境用逗号加空格。 */
export function formatLocaleList(list: readonly string[], locale: string): string {
  const separator = locale.startsWith("zh") || locale.startsWith("ja") ? "、" : ", ";
  return list.join(separator);
}

/** 按服务端 boardLocales 的顺序排列缺语种，未知语种排在后面（不丢信息）。 */
export function orderLocales(list: readonly string[]): string[] {
  const known = BOARD_LOCALES.filter((code) => list.includes(code));
  const unknown = list.filter((code) => !(BOARD_LOCALES as readonly string[]).includes(code));
  return [...known, ...unknown];
}
