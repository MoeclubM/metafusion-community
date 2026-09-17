// 权限判定的纯用例：口径必须与服务端 internal/auth/permission.go 的 Can 完全一致——
// 前端只是"不把注定 403 的入口给出去"，口径漂移会直接表现成"按钮点了就报错"。
import { test } from "node:test";
import assert from "node:assert/strict";

import {
  COMMUNITY_BOARD_MANAGE,
  COMMUNITY_POST_CREATE,
  COMMUNITY_POST_MODERATE,
  COMMUNITY_TOPIC_PIN,
  can,
  canAny,
} from "../src/lib/permissions.ts";

test("带权限码的令牌只认码，* 通配即全权", () => {
  const moderator = { role: "user", permissions: [COMMUNITY_POST_CREATE, COMMUNITY_POST_MODERATE] };
  assert.equal(can(moderator, COMMUNITY_POST_MODERATE), true);
  assert.equal(can(moderator, COMMUNITY_TOPIC_PIN), false);
  assert.equal(can(moderator, COMMUNITY_BOARD_MANAGE), false);

  const adminGroup = { role: "user", permissions: ["*"] };
  assert.equal(can(adminGroup, COMMUNITY_BOARD_MANAGE), true);
  assert.equal(can(adminGroup, COMMUNITY_TOPIC_PIN), true);
});

test("带码但角色是 admin 时不再按角色放行（避免角色兜底变成后门）", () => {
  const adminWithOneCode = { role: "admin", permissions: [COMMUNITY_POST_CREATE] };
  assert.equal(can(adminWithOneCode, COMMUNITY_BOARD_MANAGE), false);
  assert.equal(can(adminWithOneCode, COMMUNITY_TOPIC_PIN), false);
});

test("没有 permissions 声明的老令牌按历史边界兜底", () => {
  const legacyAdmin = { role: "admin", permissions: [] };
  assert.equal(can(legacyAdmin, COMMUNITY_BOARD_MANAGE), true);
  assert.equal(can(legacyAdmin, COMMUNITY_TOPIC_PIN), true);
  assert.equal(can(legacyAdmin, COMMUNITY_POST_MODERATE), true);

  const legacyMember = { role: "user", permissions: [] };
  // 发帖码在老令牌下"登录即可"（服务端 legacyOpenCodes），治理码只认 admin。
  assert.equal(can(legacyMember, COMMUNITY_POST_CREATE), true);
  assert.equal(can(legacyMember, COMMUNITY_POST_MODERATE), false);
  assert.equal(can(legacyMember, COMMUNITY_BOARD_MANAGE), false);

  const legacyEditor = { role: "editor", permissions: null };
  assert.equal(can(legacyEditor, COMMUNITY_POST_CREATE), true);
  assert.equal(can(legacyEditor, COMMUNITY_POST_MODERATE), false);
});

test("匿名与空身份一律不放行", () => {
  assert.equal(can(null, COMMUNITY_POST_CREATE), false);
  assert.equal(can(undefined, COMMUNITY_BOARD_MANAGE), false);
  assert.equal(canAny(null, [COMMUNITY_BOARD_MANAGE, COMMUNITY_TOPIC_PIN]), false);
});

test("canAny 是「任一码」的入口判定", () => {
  const pinOnly = { role: "user", permissions: [COMMUNITY_TOPIC_PIN] };
  assert.equal(canAny(pinOnly, [COMMUNITY_BOARD_MANAGE, COMMUNITY_TOPIC_PIN]), true);
  assert.equal(canAny(pinOnly, [COMMUNITY_BOARD_MANAGE, COMMUNITY_POST_MODERATE]), false);
});
