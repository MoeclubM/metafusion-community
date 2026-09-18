// 举报与申诉的纯用例：词表、处置动作可用性、查询串、字典键映射。
//
// 这些判据错了不会报错：界面只会静默少一个动作、或者把原始码显示给运营看。
// 词表与字典键同时被这里钉住，加一个枚举值忘了加文案会在断言里露出来。
import { test } from "node:test";
import assert from "node:assert/strict";

import {
  APPEAL_STATUSES,
  REPORT_ENFORCEMENTS,
  REPORT_REASONS,
  REPORT_STATUSES,
  REPORT_TARGET_TYPES,
  allowedEnforcements,
  appealStatusLabelKey,
  buildQueueQuery,
  contentKindOf,
  enforcementLabelKey,
  mayRemoveContent,
  reasonLabelKey,
  statusLabelKey,
  statusTone,
  targetSummary,
  targetTypeLabelKey,
} from "../src/lib/reports.ts";
import type { ReportRow } from "../src/lib/api/reports.ts";

function row(overrides: Partial<ReportRow>): ReportRow {
  return {
    id: "r1",
    target_type: "comment",
    target_id: "c1",
    target_context: { content_kind: "comment" },
    reason: "spam",
    reporter_id: "u1",
    reporter_name: "reporter",
    status: "pending",
    created_at: "2026-09-19T00:00:00Z",
    updated_at: "2026-09-19T00:00:00Z",
    ...overrides,
  };
}

test("词表与服务端一致（改动这里就必须改 internal/store/reports.go 与迁移的 CHECK）", () => {
  assert.deepEqual([...REPORT_TARGET_TYPES], ["entity", "comment", "post", "user", "resource"]);
  assert.deepEqual([...REPORT_STATUSES], ["pending", "accepted", "rejected", "resolved"]);
  assert.deepEqual([...REPORT_ENFORCEMENTS], ["none", "content_removed", "user_banned"]);
  assert.deepEqual([...APPEAL_STATUSES], ["pending", "accepted", "rejected"]);
  assert.deepEqual(
    [...REPORT_REASONS],
    ["illegal", "copyright", "privacy", "abuse", "harassment", "spam", "misinformation", "other"],
  );
});

test("状态配色：待处理与已受理不是同一档（队列要一眼看出还没处理完的）", () => {
  assert.equal(statusTone("pending"), "warn");
  assert.equal(statusTone("accepted"), "info");
  assert.equal(statusTone("resolved"), "ok");
  assert.equal(statusTone("rejected"), "off");
});

test("每个枚举值都有字典键（否则界面会显示原始码）", () => {
  for (const status of REPORT_STATUSES) assert.match(statusLabelKey(status), /^admin\.reports\.status\.[a-z_]+$/);
  for (const reason of REPORT_REASONS) assert.match(reasonLabelKey(reason), /^admin\.reports\.reason\.[a-z_]+$/);
  for (const value of REPORT_ENFORCEMENTS) assert.match(enforcementLabelKey(value), /^admin\.reports\.enforcement\.[a-z_]+$/);
  for (const status of APPEAL_STATUSES) assert.match(appealStatusLabelKey(status), /^admin\.reports\.appealStatus\.[a-z_]+$/);
  for (const target of REPORT_TARGET_TYPES) assert.match(targetTypeLabelKey(target), /^admin\.reports\.target\.[a-z_]+$/);
  // 空结论（还没处置）与 none 是同一档文案。
  assert.equal(enforcementLabelKey(""), "admin.reports.enforcement.none");
});

test("下线内容只对本服务里的内容开放（实体/资源/用户不给按钮）", () => {
  assert.equal(mayRemoveContent(row({ target_context: { content_kind: "comment" } })), true);
  assert.equal(mayRemoveContent(row({ target_context: { content_kind: "topic" } })), true);
  assert.equal(mayRemoveContent(row({ target_context: { content_kind: "reply" } })), true);
  assert.equal(mayRemoveContent(row({ target_type: "entity", target_context: {} })), false);
  assert.equal(mayRemoveContent(row({ target_type: "resource", target_context: {} })), false);
  assert.equal(mayRemoveContent(row({ target_type: "user", target_context: { content_kind: "user" } })), false);
  assert.equal(contentKindOf(row({ target_context: { content_kind: "reply" } })), "reply");
});

test("可选的处置结论随对象与目标作者变化", () => {
  // 本地内容 + 有作者：三种结论都能选。
  assert.deepEqual(
    allowedEnforcements(row({ target_author_id: "author-1", target_context: { content_kind: "comment" } })),
    ["none", "content_removed", "user_banned"],
  );
  // 本地内容但没有作者快照：不能记"封禁用户"（没有可封禁的对象）。
  assert.deepEqual(allowedEnforcements(row({ target_context: { content_kind: "comment" } })), ["none", "content_removed"]);
  // 实体：既不能下线内容也不能封禁（作者不在本地）。
  assert.deepEqual(allowedEnforcements(row({ target_type: "entity", target_context: {} })), ["none"]);
  // 用户对象：只能记"封禁用户"或无动作。
  assert.deepEqual(
    allowedEnforcements(row({ target_type: "user", target_context: { content_kind: "user" }, target_author_id: "u9" })),
    ["none", "user_banned"],
  );
});

test("查询串：空过滤项不出现在 URL 里，多选状态用逗号", () => {
  assert.equal(buildQueueQuery({ page: 1, pageSize: 20 }), "page=1&page_size=20");
  assert.equal(
    buildQueueQuery({ statuses: ["pending", "accepted"], targetType: "comment", q: "  刷屏 ", page: 2, pageSize: 50 }),
    "status=pending%2Caccepted&target_type=comment&q=%E5%88%B7%E5%B1%8F&page=2&page_size=50",
  );
  // target_type=all 与空状态都不进查询串。
  assert.equal(buildQueueQuery({ statuses: [], targetType: "all", q: "  ", page: 0, pageSize: 20 }), "page=1&page_size=20");
});

test("对象摘要优先用快照（内容可能已经被下线，快照是留下的证物）", () => {
  assert.equal(targetSummary(row({ target_context: { topic_title: "标题", excerpt: "正文摘要" } })), "标题 · 正文摘要");
  assert.equal(targetSummary(row({ target_context: { topic_title: "标题" } })), "标题");
  assert.equal(targetSummary(row({ target_context: {} })), "c1");
});
