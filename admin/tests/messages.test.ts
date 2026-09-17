// 四语字典的结构断言：键集合必须完全一致、没有空值、占位符必须一一对应。
// 缺键在运行期表现为"界面显示键名"，占位符不一致表现为"文案里留下了 {count}"——
// 两者都不该等到有人在浏览器里点出来才发现。
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const locales = ["zh-CN", "zh-TW", "ja-JP", "en-US"] as const;

function load(locale: string): Record<string, string> {
  const raw = readFileSync(join(here, "..", "src", "messages", locale + ".json"), "utf8");
  return JSON.parse(raw) as Record<string, string>;
}

function placeholders(value: string): string[] {
  return [...value.matchAll(/\{(\w+)\}/g)].map((m) => m[1]!).sort();
}

test("四语字典键集合完全一致", () => {
  const dicts = locales.map((locale) => ({ locale, dict: load(locale) }));
  const base = Object.keys(dicts[0]!.dict).sort();
  assert.ok(base.length > 100, "字典规模异常：" + base.length);
  for (const { locale, dict } of dicts.slice(1)) {
    assert.deepEqual(Object.keys(dict).sort(), base, locale + " 的键集合与 zh-CN 不一致");
  }
  for (const key of base) {
    assert.ok(key.startsWith("admin."), "键名必须带 admin. 前缀：" + key);
  }
});

test("每条文案都非空，且同一键的占位符四语一致", () => {
  const zh = load("zh-CN");
  for (const value of Object.values(zh)) {
    assert.notEqual(value.trim(), "");
  }
  for (const locale of locales.slice(1)) {
    const dict = load(locale);
    for (const [key, value] of Object.entries(dict)) {
      assert.notEqual(value.trim(), "", locale + " 的 " + key + " 是空串");
      assert.deepEqual(placeholders(value), placeholders(zh[key]!), locale + " 的 " + key + " 占位符与 zh-CN 不一致");
    }
  }
});

test("错误码文案覆盖本后台会遇到的码", () => {
  const zh = load("zh-CN");
  for (const key of [
    "admin.err.unauthorized",
    "admin.err.forbidden",
    "admin.err.notFound",
    "admin.err.invalidPayload",
    "admin.err.invalidBoard",
    "admin.err.topicLocked",
    "admin.err.module",
    "admin.err.upstream",
    "admin.err.network",
    "admin.err.unknownCode",
    "admin.err.fourLocales",
  ]) {
    assert.ok(Object.prototype.hasOwnProperty.call(zh, key), "缺少错误文案：" + key);
  }
});
