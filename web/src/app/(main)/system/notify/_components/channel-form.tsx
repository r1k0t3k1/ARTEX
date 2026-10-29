"use client";

import { CheckIcon } from "lucide-react";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import type { NotificationFilter } from "@/lib/types";

// asText / inputType は本ファイル内の値取得ヘルパー（コントロールの描画と強く結びつく）なので、channel-fields には置かない。
import { type FieldDef, type FieldKind, SEVERITY_OPTIONS } from "./channel-fields";

// asText は任意の設定値を入力欄で使える文字列にする。
// config は JSON 由来で、値は string / number / boolean / array / null があり得る。
// ここでは「テキストボックスに入れられるか」だけを気にし、具体的な直列化は
// buildConfig が担う。
function asText(v: unknown): string {
  if (typeof v === "string") return v;
  if (v === null || v === undefined) return "";
  return String(v);
}

// inputType はフィールド種別を input の type 属性へ対応づける。
function inputType(kind: FieldKind): "text" | "password" | "number" {
  if (kind === "password") return "password";
  if (kind === "number") return "number";
  return "text";
}

// ConfigField はフィールド定義に従って対応するコントロールを描画する。
//
// マスク済みフィールドの扱いがここで唯一のこだわり：入力欄にはマスク値そのものを
// **表示せず**、「保存済み」の 1 行ヒントだけを出す。これにより画面上のルールは
// 1 つだけになる——欄に文字があればユーザーが入力したもの、空欄なら空値。
// "__masked__:…abc123" を入力欄に入れてしまうと、ユーザーはそれを自分で消すべき
// プレースホルダだと誤解し、かえって資格情報を誤って消しやすくなる。
export function ConfigField({
  def,
  value,
  isSecret,
  onChange,
}: {
  def: FieldDef;
  value: unknown;
  isSecret: boolean;
  onChange: (v: unknown) => void;
}) {
  const id = `n-cfg-${def.key}`;
  const raw = asText(value);
  // バックエンドが返すマスク値："__masked__:…abc123" の形で、末尾は元値の識別可能な断片。
  const masked = isSecret && raw.startsWith("__masked__");
  const maskedTail = masked ? (raw.split("…")[1] ?? "") : "";

  if (def.kind === "switch") {
    return (
      <div className="flex items-center gap-2 text-sm">
        <Switch checked={value === true} onCheckedChange={onChange} aria-label={def.label} />
        {def.label}
        {def.help && <span className="text-muted-foreground">（{def.help}）</span>}
      </div>
    );
  }

  if (def.kind === "select") {
    return (
      <div className="grid gap-2">
        <Label>{def.label}</Label>
        <Select value={raw || def.options?.[0]?.value} onValueChange={onChange}>
          <SelectTrigger>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {(def.options ?? []).map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    );
  }

  // コントロールはフィールド種別で分岐する。ネスト三項ではなく if 連鎖を使うのは、
  // ここで 4 種類のコントロールを区別する必要があり、三項を 3 段重ねると読むときに
  // 括弧を数えないと追えなくなるため。
  function control() {
    if (def.kind === "textarea" || def.kind === "kv") {
      return (
        <Textarea
          id={id}
          className="font-mono"
          placeholder={def.placeholder}
          value={masked ? "" : raw}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    }
    if (def.kind === "list") {
      return (
        <Input
          id={id}
          value={Array.isArray(value) ? (value as string[]).join(", ") : raw}
          onChange={(e) => onChange(e.target.value)}
          placeholder={def.placeholder}
        />
      );
    }
    return (
      <Input
        id={id}
        className={def.kind === "text" ? "font-mono" : ""}
        type={inputType(def.kind)}
        placeholder={def.placeholder}
        value={masked ? "" : raw}
        onChange={(e) => onChange(e.target.value)}
      />
    );
  }

  const hint = masked ? (
    <p className="text-muted-foreground flex items-center gap-1 text-xs">
      <CheckIcon className="size-3" />
      保存済み{maskedTail ? `（末尾 ${maskedTail}）` : ""} · 新しい値を入れると上書き、空欄にするとこの項目を削除
    </p>
  ) : (
    def.help && <p className="text-muted-foreground text-xs">{def.help}</p>
  );

  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{def.label}</Label>
      {control()}
      {hint}
    </div>
  );
}

// FilterSummary はフィルタ条件を 1 行に要約し、カードを開かなくてもこのチャネルが何を配信するか分かるようにする。
export function FilterSummary({ filter }: { filter: NotificationFilter }) {
  const parts: string[] = [];
  if (filter.min_severity) {
    parts.push(SEVERITY_OPTIONS.find((o) => o.value === filter.min_severity)?.label ?? filter.min_severity);
  }
  if (filter.vulnclass_include?.length) parts.push(`種別に ${filter.vulnclass_include.length} 語を含む`);
  if (filter.vulnclass_exclude?.length) parts.push(`${filter.vulnclass_exclude.length} 語を除外`);
  if (filter.task_ids?.length) parts.push(`${filter.task_ids.length} 件のタスク`);
  if (filter.asset_ids?.length) parts.push(`${filter.asset_ids.length} 件の資産`);
  if (filter.on_status_change) parts.push("ステータス変更を含む");
  if (parts.length === 0) {
    return <p className="text-muted-foreground text-sm">すべての脆弱性</p>;
  }
  return <p className="text-muted-foreground text-sm">{parts.join(" · ")}</p>;
}
