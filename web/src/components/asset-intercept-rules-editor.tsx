"use client";

import * as React from "react";

import { PlusIcon, Trash2Icon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import type { AssetInterceptKind, AssetInterceptRuleInput } from "@/lib/types";

// shadcn Select ではなく NativeSelect（ネイティブの <select>）を使う。このエディタは Sheet
// ドロワー内で使われるが、shadcn Select のドロップダウンは body へポータルされるため、外部を
// クリックするとドロワーの「外側クリックで閉じる」が誤発動する。ネイティブのドロップダウンには
// この問題がない。
export const ASSET_INTERCEPT_KIND_OPTIONS: {
  value: AssetInterceptKind;
  label: string;
  placeholder: string;
}[] = [
  { value: "exact_domain", label: "ドメイン（完全一致）", placeholder: "example.gov.cn" },
  { value: "exact_ip", label: "IP（完全一致）", placeholder: "203.0.113.10" },
  { value: "exact_url", label: "URL（完全一致）", placeholder: "https://example.com/login" },
  { value: "fuzzy_domain", label: "ドメイン（あいまい）", placeholder: ".gov.cn" },
  { value: "fuzzy_ip", label: "IP（あいまい）", placeholder: "203.0.113." },
  { value: "fuzzy_url", label: "URL（あいまい）", placeholder: "/admin" },
  { value: "cidr", label: "CIDR ネットワーク", placeholder: "192.168.0.0/16" },
];

// AssetInterceptRulesEditor は「インターセプト/許可ルール」の制御された複数行エディタ
// （block/allow + 種類 + マッチ内容 + メモ）。永続化は行わず、いつ保存するかは親が決める。
export function AssetInterceptRulesEditor({
  value,
  onChange,
}: {
  value: AssetInterceptRuleInput[];
  onChange: (v: AssetInterceptRuleInput[]) => void;
}) {
  function update(i: number, patch: Partial<AssetInterceptRuleInput>) {
    onChange(value.map((r, idx) => (idx === i ? { ...r, ...patch } : r)));
  }
  function remove(i: number) {
    onChange(value.filter((_, idx) => idx !== i));
  }
  function add() {
    onChange([...value, { action: "block", kind: "fuzzy_domain", pattern: "", note: "", enabled: true }]);
  }
  return (
    <div className="grid gap-2">
      {value.map((r, i) => {
        const ph = ASSET_INTERCEPT_KIND_OPTIONS.find((o) => o.value === r.kind)?.placeholder ?? "";
        return (
          // biome-ignore lint/suspicious/noArrayIndexKey: 行に安定した id がないため、インデックスで制御する
          <div key={i} className="flex items-center gap-2">
            <NativeSelect
              size="sm"
              className="w-[84px] shrink-0"
              value={r.action}
              onChange={(e) => update(i, { action: e.target.value as "block" | "allow" })}
            >
              <NativeSelectOption value="block">インターセプト</NativeSelectOption>
              <NativeSelectOption value="allow">許可</NativeSelectOption>
            </NativeSelect>
            <NativeSelect
              size="sm"
              className="w-[120px] shrink-0"
              value={r.kind}
              onChange={(e) => update(i, { kind: e.target.value as AssetInterceptKind })}
            >
              {ASSET_INTERCEPT_KIND_OPTIONS.map((o) => (
                <NativeSelectOption key={o.value} value={o.value}>
                  {o.label}
                </NativeSelectOption>
              ))}
            </NativeSelect>
            <Input
              className="flex-1"
              placeholder={ph}
              value={r.pattern}
              onChange={(e) => update(i, { pattern: e.target.value })}
            />
            <Input
              className="w-[120px] shrink-0"
              placeholder="メモ（任意）"
              value={r.note}
              onChange={(e) => update(i, { note: e.target.value })}
            />
            <Button
              type="button"
              size="icon"
              variant="ghost"
              className="text-destructive hover:text-destructive size-8 shrink-0"
              onClick={() => remove(i)}
            >
              <Trash2Icon className="size-4" />
            </Button>
          </div>
        );
      })}
      <Button type="button" size="sm" variant="outline" className="w-fit" onClick={add}>
        <PlusIcon className="size-4" /> 1 件追加
      </Button>
    </div>
  );
}
