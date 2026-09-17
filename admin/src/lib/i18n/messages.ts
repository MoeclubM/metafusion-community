import zhCN from "@/messages/zh-CN.json";
import zhTW from "@/messages/zh-TW.json";
import jaJP from "@/messages/ja-JP.json";
import enUS from "@/messages/en-US.json";
import { normalizeLocale, type Locale } from "./routing";

// 四语字典整包静态引入：后台文案量小（不足 200 键），拆包带来的复杂度大于收益。
const catalog: Record<Locale, Record<string, string>> = {
  "zh-CN": zhCN as Record<string, string>,
  "zh-TW": zhTW as Record<string, string>,
  "ja-JP": jaJP as Record<string, string>,
  "en-US": enUS as Record<string, string>,
};

export function messagesFor(locale?: string | null): Record<string, string> {
  return catalog[normalizeLocale(locale)];
}

/** 缺键回退到 zh-CN，再不行返回 key 本身：不静默返回空串，缺键要能看见。 */
export function translate(
  messages: Record<string, string>,
  key: string,
  vars?: Record<string, string | number>
): string {
  let text = messages[key];
  if (text == null) text = catalog["zh-CN"][key];
  if (text == null) return key;
  if (vars) for (const [k, v] of Object.entries(vars)) text = text.split(`{${k}}`).join(String(v));
  return text;
}
