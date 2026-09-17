"use client";

// 板块编辑器：四语名称/描述 + 颜色/图标/排序/两个开关。
//
// 三条与服务端契约对齐的规则（internal/handler/board.go）：
//   1. PUT 是补丁语义——只发改动过的字段，空补丁会被 400 拒，所以界面上先拦一次；
//   2. 名称四语齐备且非空（缺语种 = 400 four_locale_names_required），描述允许四语全空 = 清空；
//   3. code 不可改、板块不可新建或删除——编辑器里就不提供这些入口。

import { useMemo, useState } from "react";
import { useI18n } from "@/lib/i18n/provider";
import { updateBoard } from "@/lib/api/boards";
import {
  BOARD_COLORS,
  BOARD_ICON_SUGGESTIONS,
  checkLocaleMap,
  diffBoardPatch,
  isKnownColor,
  isKnownIcon,
  patchFieldNames,
  trimLocaleMap,
  type BoardFormValues,
  type BoardPatch,
  type BoardRow,
} from "@/lib/boards";
import { describeError } from "@/lib/errors";
import { formatLocaleList } from "@/lib/locales";
import { BOARD_LOCALES } from "@/lib/boards";
import { COMMUNITY_BOARD_MANAGE } from "@/lib/permissions";
import { Button, ConfirmDialog, Field, Modal, Notice, Select, TextInput, Toggle } from "./ui";

/** 补丁字段名 → 字典键：提示里说人话（"名称、颜色"），不是字段名。 */
export const BOARD_FIELD_KEYS: Record<string, string> = {
  names: "admin.boards.fieldNames",
  descriptions: "admin.boards.fieldDescriptions",
  color: "admin.boards.fieldColor",
  icon: "admin.boards.fieldIcon",
  sort_order: "admin.boards.fieldSort",
  is_enabled: "admin.boards.fieldEnabled",
  show_in_feed: "admin.boards.fieldFeed",
};

function formFrom(row: BoardRow): BoardFormValues {
  const names: Record<string, string> = {};
  const descriptions: Record<string, string> = {};
  for (const code of BOARD_LOCALES) {
    names[code] = row.names?.[code] ?? "";
    descriptions[code] = row.descriptions?.[code] ?? "";
  }
  return {
    names,
    descriptions,
    color: row.color ?? "",
    icon: row.icon ?? "",
    sort_order: row.sort_order ?? 0,
    is_enabled: row.is_enabled === true,
    show_in_feed: row.show_in_feed === true,
  };
}

export function BoardEditor(props: { row: BoardRow; onClose: () => void; onSaved: (row: BoardRow, fields: string[]) => void }) {
  const { t, locale } = useI18n();
  const { row } = props;
  const [form, setForm] = useState<BoardFormValues>(() => formFrom(row));
  const [sortText, setSortText] = useState(String(row.sort_order ?? 0));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [info, setInfo] = useState("");
  const [pending, setPending] = useState<{ patch: BoardPatch; fields: string[] } | null>(null);

  const fieldsText = (fields: string[]) =>
    fields.map((name) => t(BOARD_FIELD_KEYS[name] ?? name)).join(locale.startsWith("zh") || locale.startsWith("ja") ? "、" : ", ");

  // 实时差分：让"这次会提交什么"在点击保存前就可见（空补丁 = 未提交）。
  const patch = useMemo<BoardPatch>(() => {
    const sort = Number.parseInt(sortText, 10);
    return diffBoardPatch(row, {
      ...form,
      names: trimLocaleMap(form.names),
      descriptions: trimLocaleMap(form.descriptions),
      sort_order: Number.isNaN(sort) ? row.sort_order : sort,
    });
  }, [form, row, sortText]);
  const patchFields = patchFieldNames(patch);

  const setLocaleValue = (field: "names" | "descriptions", code: string, value: string) =>
    setForm((prev) => ({ ...prev, [field]: { ...prev[field], [code]: value } }));

  const submit = (payload: BoardPatch, fields: string[]) => {
    setBusy(true);
    setError("");
    setInfo("");
    updateBoard(row.code, payload)
      .then((updated) => {
        props.onSaved(updated, fields);
      })
      .catch((err) => setError(describeError(err, t, locale, COMMUNITY_BOARD_MANAGE)))
      .finally(() => setBusy(false));
  };

  const onSave = () => {
    setError("");
    setInfo("");
    const names = checkLocaleMap(form.names);
    if (!names.ok) {
      setError(t("admin.boards.missingLocales", { locales: formatLocaleList(names.missing, locale) }));
      return;
    }
    if (names.cleared) {
      setError(t("admin.boards.namesRequired"));
      return;
    }
    const descriptions = checkLocaleMap(form.descriptions);
    if (!descriptions.ok) {
      setError(t("admin.boards.missingLocales", { locales: formatLocaleList(descriptions.missing, locale) }));
      return;
    }
    const sort = Number.parseInt(sortText, 10);
    if (Number.isNaN(sort)) {
      setError(t("admin.err.invalidPayload"));
      return;
    }
    if (form.color.trim() === "" || form.icon.trim() === "") {
      setError(t("admin.err.invalidPayload"));
      return;
    }
    if (patchFields.length === 0) {
      setInfo(t("admin.boards.noChange"));
      return;
    }
    // 停用会让板块不能再发新主题：破坏性变更先确认（确认框拿的是同一份 patch，不会被表单漂移改掉）。
    if (patch.is_enabled === false && row.is_enabled) {
      setPending({ patch, fields: patchFields });
      return;
    }
    submit(patch, patchFields);
  };

  return (
    <>
      <Modal
        wide
        title={t("admin.boards.editTitle", { code: row.code })}
        onClose={props.onClose}
        footer={
          <>
            <Button type="button" onClick={props.onClose} disabled={busy}>
              {t("admin.cancel")}
            </Button>
            <Button type="button" variant="primary" onClick={onSave} disabled={busy}>
              {busy ? t("admin.saving") : t("admin.save")}
            </Button>
          </>
        }
      >
        <p className="text-[11px] leading-relaxed text-muted">{t("admin.boards.codeNote")}</p>

        <div className="grid gap-3 sm:grid-cols-2">
          {BOARD_LOCALES.map((code) => (
            <Field key={"names-" + code} label={t("admin.boards.fieldNames") + " · " + code} hint={code === BOARD_LOCALES[0] ? t("admin.boards.namesHint") : undefined}>
              <TextInput value={form.names[code] ?? ""} onChange={(e) => setLocaleValue("names", code, e.target.value)} />
            </Field>
          ))}
        </div>

        <div className="grid gap-3 sm:grid-cols-2">
          {BOARD_LOCALES.map((code) => (
            <Field key={"descriptions-" + code} label={t("admin.boards.fieldDescriptions") + " · " + code} hint={code === BOARD_LOCALES[0] ? t("admin.boards.descriptionsHint") : undefined}>
              <TextInput value={form.descriptions[code] ?? ""} onChange={(e) => setLocaleValue("descriptions", code, e.target.value)} />
            </Field>
          ))}
        </div>

        <div className="grid gap-3 sm:grid-cols-2">
          <Field label={t("admin.boards.fieldColor")} hint={isKnownColor(form.color) ? undefined : t("admin.boards.colorCustom")}>
            <Select value={isKnownColor(form.color) ? form.color : "__custom"} onChange={(e) => setForm((p) => ({ ...p, color: e.target.value === "__custom" ? p.color : e.target.value }))}>
              <option value="__custom">{form.color || "—"}</option>
              {BOARD_COLORS.map((color) => (
                <option key={color} value={color}>
                  {color}
                </option>
              ))}
            </Select>
          </Field>
          <Field label={t("admin.boards.fieldIcon")} hint={isKnownIcon(form.icon) ? undefined : t("admin.boards.iconCustom")}>
            <Select value={isKnownIcon(form.icon) ? form.icon : "__custom"} onChange={(e) => setForm((p) => ({ ...p, icon: e.target.value === "__custom" ? p.icon : e.target.value }))}>
              <option value="__custom">{form.icon || "—"}</option>
              {BOARD_ICON_SUGGESTIONS.map((icon) => (
                <option key={icon} value={icon}>
                  {icon}
                </option>
              ))}
            </Select>
          </Field>
          <Field label={t("admin.boards.fieldSort")}>
            <TextInput value={sortText} inputMode="numeric" onChange={(e) => setSortText(e.target.value)} />
          </Field>
          <div className="space-y-3 pt-1">
            <Toggle checked={form.is_enabled} onChange={(next) => setForm((p) => ({ ...p, is_enabled: next }))} label={t("admin.boards.fieldEnabled")} />
            <Toggle
              checked={form.show_in_feed}
              onChange={(next) => setForm((p) => ({ ...p, show_in_feed: next }))}
              label={t("admin.boards.fieldFeed")}
              hint={t("admin.boards.feedHint")}
            />
          </div>
        </div>

        {error ? <Notice kind="err" text={error} /> : null}
        {info ? <Notice kind="info" text={info} /> : null}
        <p className="text-[11px] leading-relaxed text-muted">
          {patchFields.length > 0
            ? t("admin.boards.patchPreview", { fields: fieldsText(patchFields) })
            : t("admin.boards.noChange")}
        </p>
      </Modal>

      {pending ? (
        <ConfirmDialog
          title={t("admin.boards.fieldEnabled")}
          body={t("admin.boards.disableConfirm", { code: row.code })}
          confirmLabel={t("admin.confirm")}
          cancelLabel={t("admin.cancel")}
          danger
          busy={busy}
          onClose={() => setPending(null)}
          onConfirm={() => {
            const next = pending;
            setPending(null);
            submit(next.patch, next.fields);
          }}
        />
      ) : null}
    </>
  );
}
