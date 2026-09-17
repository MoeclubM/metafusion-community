// 错误码翻译的纯用例：服务端给的是机器码，界面必须翻成能执行的话，
// 未覆盖的码要**原样带出**（不伪装成已解释）。
import { test } from "node:test";
import assert from "node:assert/strict";

import {
  describeError,
  formatLocaleList,
  orderLocales,
  parseMissingLocales,
  toErrorLike,
  type Translate,
} from "../src/lib/errors.ts";

// 假翻译器：把键与变量原样拼出来，便于断言"用了哪个键、带了什么变量"。
const t: Translate = (key, vars) => (vars ? key + "|" + JSON.stringify(vars) : key);

test("解析 four_locale_names_required 里的缺语种", () => {
  assert.deepEqual(parseMissingLocales("four_locale_names_required: zh-TW,ja-JP"), ["zh-TW", "ja-JP"]);
  assert.deepEqual(parseMissingLocales("four_locale_names_required: en-US"), ["en-US"]);
  assert.equal(parseMissingLocales("invalid_payload"), null);
  assert.equal(parseMissingLocales(""), null);
  assert.equal(parseMissingLocales(null), null);
  // 没有语种清单时（只有前缀）不臆造缺失语种，交给通用文案。
  assert.equal(parseMissingLocales("four_locale_names_required: "), null);
});

test("缺语种按服务端顺序排列，未知语种不丢", () => {
  assert.deepEqual(orderLocales(["en-US", "zh-CN"]), ["zh-CN", "en-US"]);
  assert.deepEqual(orderLocales(["fr", "ja-JP"]), ["ja-JP", "fr"]);
  assert.equal(formatLocaleList(["zh-TW", "ja-JP"], "zh-CN"), "zh-TW、ja-JP");
  assert.equal(formatLocaleList(["zh-TW", "ja-JP"], "en-US"), "zh-TW, ja-JP");
});

test("缺语种错误翻成「还缺哪些语种」", () => {
  const text = describeError(
    { status: 400, code: "four_locale_names_required: zh-TW,ja-JP" },
    t,
    "zh-CN"
  );
  assert.equal(text, 'admin.err.fourLocales|{"locales":"zh-TW、ja-JP"}');
});

test("403 点名缺的权限码；401/404/5xx/网络各有对应文案", () => {
  assert.equal(
    describeError({ status: 403, code: "forbidden" }, t, "zh-CN", "community.board.manage"),
    'admin.err.forbidden|{"code":"community.board.manage"}'
  );
  assert.equal(describeError({ status: 401, code: "authentication_required" }, t, "zh-CN"), "admin.err.unauthorized");
  assert.equal(describeError({ status: 404, code: "not_found" }, t, "zh-CN"), "admin.err.notFound");
  assert.equal(describeError({ status: 400, code: "invalid_payload" }, t, "zh-CN"), "admin.err.invalidPayload");
  assert.equal(describeError({ status: 500, code: "module_error" }, t, "zh-CN"), "admin.err.module");
  assert.equal(describeError({ status: 503, code: "" }, t, "zh-CN"), 'admin.err.upstream|{"status":503}');
  assert.equal(describeError({ status: 0, code: "", message: "failed" }, t, "zh-CN"), 'admin.err.network|{"message":"failed"}');
});

test("未解释的码原样带出（不伪装成已解释）", () => {
  assert.equal(
    describeError({ status: 400, code: "brand_new_code" }, t, "zh-CN"),
    'admin.err.unknownCode|{"status":400,"code":"brand_new_code"}'
  );
});

test("异常换算成 {status, code, message}：非 2xx 与网络错误都不丢状态", () => {
  assert.deepEqual(toErrorLike({ status: 503, code: "module_error", message: "boom" }), {
    status: 503,
    code: "module_error",
    message: "boom",
  });
  assert.equal(toErrorLike(new Error("nope")).status, 0);
  assert.equal(toErrorLike(new Error("nope")).message, "nope");
  assert.equal(toErrorLike("plain").message, "plain");
});
