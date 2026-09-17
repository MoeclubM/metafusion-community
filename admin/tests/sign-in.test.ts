// 登录回跳地址的用例：三条实测踩坑的规则各来一条，外加"套娃"的回归。
import { test } from "node:test";
import assert from "node:assert/strict";

import { buildSignInHref, isLoginPath, isOwnPath } from "../src/lib/sign-in.ts";

test("只回跳本应用 basePath 内的路径", () => {
  assert.equal(buildSignInHref("/admin/community/"), "/login?redirect=" + encodeURIComponent("/admin/community/"));
  assert.equal(buildSignInHref("/admin/community", "?tab=topics"), "/login?redirect=" + encodeURIComponent("/admin/community?tab=topics"));
  // basePath 之外（站点首页、其它应用）一律只给干净登录页：回跳过去只会再吃一个 404。
  assert.equal(buildSignInHref("/"), "/login");
  assert.equal(buildSignInHref("/admin/storage/"), "/login");
  assert.equal(buildSignInHref(""), "/login");
});

test("登录页自身不再拼 redirect（防套娃）", () => {
  assert.equal(buildSignInHref("/login"), "/login");
  assert.equal(buildSignInHref("/login/"), "/login");
  assert.equal(buildSignInHref("/login", "?redirect=%2Fadmin%2Fcommunity%2F"), "/login");
  // 即便站点把登录页挂在应用前缀下，也不把它当成回跳目标。
  assert.equal(buildSignInHref("/admin/community/login"), "/login");
});

test("路径判定：basePath 前缀不算命中（/admin/communityx 不是本应用）", () => {
  assert.equal(isOwnPath("/admin/community"), true);
  assert.equal(isOwnPath("/admin/community/"), true);
  assert.equal(isOwnPath("/admin/community/topics"), true);
  assert.equal(isOwnPath("/admin/communityx"), false);
  assert.equal(isOwnPath("/admin/"), false);
  assert.equal(isLoginPath("/login"), true);
  assert.equal(isLoginPath("/login/"), true);
  assert.equal(isLoginPath("/login/oauth"), false);
});
