// 板块的纯逻辑：类型、候选值、四语校验、补丁差分、展示回退。
// 契约来源（只读核对，未改服务端）：
//   internal/handler/board.go:35-61  —— 四语齐备才算通过；"所有传入键都为空串"= 显式清空
//   internal/handler/board.go:104-188—— PUT 只改传入字段，空载荷 400，code 不可改，无新建/删除
//   internal/handler/forum.go:117-132—— 列表是裸数组，10 个字段
import { BOARD_LOCALES, orderLocales } from "./locales.ts";

/** 固定语种清单由 locales.ts 提供（与错误码里的语种解析同一份，不重复声明）。 */
export { BOARD_LOCALES };
export type BoardLocale = (typeof BOARD_LOCALES)[number];

export interface BoardRow {
  code: string;
  names: Record<string, string>;
  descriptions: Record<string, string>;
  color: string;
  icon: string;
  sort_order: number;
  is_enabled: boolean;
  show_in_feed: boolean;
}

export interface BoardPatch {
  names?: Record<string, string>;
  descriptions?: Record<string, string>;
  color?: string;
  icon?: string;
  sort_order?: number;
  is_enabled?: boolean;
  show_in_feed?: boolean;
}

export interface BoardFormValues {
  names: Record<string, string>;
  descriptions: Record<string, string>;
  color: string;
  icon: string;
  sort_order: number;
  is_enabled: boolean;
  show_in_feed: boolean;
}

/** color 存的是调色板名（不是 class）；清单外的新值原样保留，不纠正。 */
export const BOARD_COLORS: readonly string[] = [
  "emerald",
  "amber",
  "sky",
  "purple",
  "cyan",
  "rose",
  "indigo",
  "teal",
];

/** 图标是 lucide 组件名（前端站点的渲染约定），候选不是白名单。 */
export const BOARD_ICON_SUGGESTIONS: readonly string[] = [
  "BookOpen",
  "Megaphone",
  "Coffee",
  "Hash",
  "Bug",
  "MessageCircle",
  "Layers",
  "Tag",
  "Sparkles",
  "Flame",
  "Bookmark",
  "MessageSquare",
  "Globe",
  "Cpu",
  "Archive",
  "Newspaper",
  "Pin",
  "Star",
];

export function trimLocaleMap(values: Record<string, string> | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(values ?? {})) out[key] = (value ?? "").trim();
  return out;
}

export type LocaleMapState = { ok: true; cleared: boolean } | { ok: false; missing: string[] };

/**
 * 四语判定，逐字对齐服务端 resolveBoardLocales：
 * 逐值 trim 后四语齐备 → 通过；"所有传入键都是空串"（且至少一个键）→ 通过但视为清空；
 * 其余（含 {} 这种键都不传）都算缺语种。
 */
export function checkLocaleMap(values: Record<string, string> | undefined): LocaleMapState {
  const trimmed = trimLocaleMap(values);
  const missing = BOARD_LOCALES.filter((code) => (trimmed[code] ?? "") === "");
  if (missing.length === 0) return { ok: true, cleared: false };
  const keys = Object.keys(trimmed);
  const allEmpty = keys.length > 0 && keys.every((key) => (trimmed[key] ?? "") === "");
  if (allEmpty) return { ok: true, cleared: true };
  return { ok: false, missing: orderLocales(missing) };
}

function sameLocaleMap(a: Record<string, string> | undefined, b: Record<string, string> | undefined): boolean {
  const left = trimLocaleMap(a);
  const right = trimLocaleMap(b);
  const keys = new Set([...Object.keys(left), ...Object.keys(right)]);
  for (const key of keys) {
    if ((left[key] ?? "") !== (right[key] ?? "")) return false;
  }
  return true;
}

/** 只挑出真正改动过的字段：空补丁会被服务端 400 拒，界面据此拦下"什么都没改"。 */
export function diffBoardPatch(row: BoardRow, next: BoardFormValues): BoardPatch {
  const patch: BoardPatch = {};
  if (!sameLocaleMap(row.names, next.names)) patch.names = trimLocaleMap(next.names);
  if (!sameLocaleMap(row.descriptions, next.descriptions)) patch.descriptions = trimLocaleMap(next.descriptions);
  const color = next.color.trim();
  const icon = next.icon.trim();
  if (color !== (row.color ?? "").trim()) patch.color = color;
  if (icon !== (row.icon ?? "").trim()) patch.icon = icon;
  if (next.sort_order !== row.sort_order) patch.sort_order = next.sort_order;
  if (next.is_enabled !== row.is_enabled) patch.is_enabled = next.is_enabled;
  if (next.show_in_feed !== row.show_in_feed) patch.show_in_feed = next.show_in_feed;
  return patch;
}

export function patchFieldNames(patch: BoardPatch): string[] {
  return Object.keys(patch);
}

/** 展示回退链：请求语言 → en-US → zh-CN → 任意非空 → fallback。 */
export function boardText(
  values: Record<string, string> | undefined,
  locale: string,
  fallback = ""
): string {
  const map = values ?? {};
  for (const code of [locale, "en-US", "zh-CN"]) {
    const hit = (map[code] ?? "").trim();
    if (hit) return hit;
  }
  for (const value of Object.values(map)) {
    const hit = (value ?? "").trim();
    if (hit) return hit;
  }
  return fallback;
}

/** 客户端过滤：按 code、任一语种名称或描述匹配（列表接口没有搜索参数）。 */
export function boardMatchesQuery(row: BoardRow, query: string): boolean {
  const needle = query.trim().toLowerCase();
  if (!needle) return true;
  const haystack = [
    row.code,
    ...Object.values(row.names ?? {}),
    ...Object.values(row.descriptions ?? {}),
  ];
  return haystack.some((value) => (value ?? "").toLowerCase().includes(needle));
}

/** 未知色的展示提示：清单外仍原样保存，但界面上要说清"不在候选里"。 */
export function isKnownColor(color: string): boolean {
  return BOARD_COLORS.includes(color.trim());
}

export function isKnownIcon(icon: string): boolean {
  return BOARD_ICON_SUGGESTIONS.includes(icon.trim());
}
