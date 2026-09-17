// 板块逻辑的纯用例：四语校验、补丁差分、展示回退链——与服务端 board.go 的口径对齐。
import { test } from "node:test";
import assert from "node:assert/strict";

import {
  BOARD_LOCALES,
  boardMatchesQuery,
  boardText,
  checkLocaleMap,
  diffBoardPatch,
  isKnownColor,
  isKnownIcon,
  patchFieldNames,
  type BoardRow,
} from "../src/lib/boards.ts";

const full = { "zh-CN": "公告", "zh-TW": "公告", "ja-JP": "お知らせ", "en-US": "News" };

function row(overrides: Partial<BoardRow> = {}): BoardRow {
  return {
    code: "announcement",
    names: { ...full },
    descriptions: { "zh-CN": "描述", "zh-TW": "描述", "ja-JP": "説明", "en-US": "Description" },
    name: "公告",
    description: "描述",
    color: "amber",
    icon: "Megaphone",
    sort_order: 10,
    is_enabled: true,
    show_in_feed: true,
    ...overrides,
  };
}

test("四语齐备才通过；部分填写的按服务端顺序报缺语种", () => {
  assert.deepEqual(checkLocaleMap(full), { ok: true, cleared: false });
  assert.deepEqual(checkLocaleMap({ "zh-CN": "公告", "en-US": "News" }), {
    ok: false,
    missing: ["zh-TW", "ja-JP"],
  });
  // 键都不传（空对象）也算缺语种，不是"清空"——与服务端 resolveBoardLocales 一致。
  assert.deepEqual(checkLocaleMap({}), { ok: false, missing: [...BOARD_LOCALES] });
});

test("四个语种全传空串 = 显式清空", () => {
  const cleared = checkLocaleMap({ "zh-CN": " ", "zh-TW": "", "ja-JP": "", "en-US": "" });
  assert.deepEqual(cleared, { ok: true, cleared: true });
});

test("补丁只带改动过的字段，空白差异不算改动", () => {
  const current = row();
  assert.deepEqual(diffBoardPatch(current, {
    names: { ...full },
    descriptions: { "zh-CN": "描述 ", "zh-TW": "描述", "ja-JP": "説明", "en-US": "Description" },
    color: "amber",
    icon: "Megaphone",
    sort_order: 10,
    is_enabled: true,
    show_in_feed: true,
  }), {});

  const patch = diffBoardPatch(current, {
    names: { "zh-CN": "站务公告", "zh-TW": "站務公告", "ja-JP": "お知らせ", "en-US": "News" },
    descriptions: { "zh-CN": "描述", "zh-TW": "描述", "ja-JP": "説明", "en-US": "Description" },
    color: "emerald",
    icon: "Megaphone",
    sort_order: 15,
    is_enabled: false,
    show_in_feed: true,
  });
  assert.deepEqual(patchFieldNames(patch), ["names", "color", "sort_order", "is_enabled"]);
  assert.equal(patch.names?.["zh-CN"], "站务公告");
  assert.equal(patch.color, "emerald");
  assert.equal(patch.sort_order, 15);
  assert.equal(patch.is_enabled, false);
  assert.equal(patch.show_in_feed, undefined);
});

test("展示回退链：请求语言 → en-US → zh-CN → 任意非空 → 单值列兜底", () => {
  assert.equal(boardText(full, "ja-JP"), "お知らせ");
  assert.equal(boardText({ "en-US": "News" }, "ja-JP"), "News");
  assert.equal(boardText({ "zh-CN": "公告" }, "ja-JP"), "公告");
  assert.equal(boardText({ fr: "Annonce" }, "ja-JP"), "Annonce");
  assert.equal(boardText({}, "ja-JP", "fallback"), "fallback");
  assert.equal(boardText(undefined, "ja-JP", "fallback"), "fallback");
});

test("客户端搜索匹配 code 与任一语种名称", () => {
  assert.equal(boardMatchesQuery(row(), "announce"), true);
  assert.equal(boardMatchesQuery(row(), "お知らせ"), true);
  assert.equal(boardMatchesQuery(row(), "描述"), true);
  assert.equal(boardMatchesQuery(row(), "  "), true);
  assert.equal(boardMatchesQuery(row(), "casual"), false);
});

test("候选色/图标之外的值不被当成非法（原样保存，界面只提示）", () => {
  assert.equal(isKnownColor("emerald"), true);
  assert.equal(isKnownColor("chartreuse"), false);
  assert.equal(isKnownIcon("Megaphone"), true);
  assert.equal(isKnownIcon("Nope"), false);
});
