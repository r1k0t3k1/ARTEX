"use client";

import * as React from "react";

import { SearchIcon } from "lucide-react";

import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

// ── DSL autocomplete ──────────────────────────────────────────────────────────
// グローバルの資産ビュー（/function/assets）とタスク別の テスト資産
// 検索で共有し、両方の検索ボックスの挙動と見た目を同じにする。

const DSL_FIELDS: { name: string; desc: string; ops: { op: string; desc: string }[] }[] = [
  {
    name: "domain",
    desc: "ドメイン（ルートドメイン/サブドメイン/サービスドメイン）",
    ops: [
      { op: "=", desc: "あいまい一致" },
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "ip",
    desc: "IPv4/IPv6 アドレス",
    ops: [
      { op: "=", desc: "あいまい一致" },
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "url",
    desc: "完全な URL（サービス/エンドポイント）",
    ops: [
      { op: "=", desc: "あいまい一致" },
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "root_domain",
    desc: "ルートドメイン",
    ops: [
      { op: "=", desc: "あいまい一致" },
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "page_title",
    desc: "ページタイトル（HTTP サービス）",
    ops: [
      { op: "=", desc: "あいまい一致" },
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "icp",
    desc: "ICP 届出番号",
    ops: [
      { op: "=", desc: "あいまい一致" },
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "service_name",
    desc: "サービス名（非 HTTP サービス）",
    ops: [
      { op: "=", desc: "あいまい一致" },
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "app_name",
    desc: "アプリ名",
    ops: [
      { op: "=", desc: "あいまい一致" },
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "bundle_id",
    desc: "アプリ Bundle ID",
    ops: [
      { op: "=", desc: "あいまい一致" },
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "category",
    desc: "アプリ分類",
    ops: [
      { op: "=", desc: "あいまい一致" },
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "app_icp",
    desc: "アプリの ICP 届出",
    ops: [
      { op: "=", desc: "あいまい一致" },
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "method",
    desc: "HTTP メソッド GET/POST/PUT/…",
    ops: [
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "service_type",
    desc: "サービスタイプ: http | other",
    ops: [
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "record_type",
    desc: "DNS レコード種別 A/CNAME/MX/…",
    ops: [
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "technology",
    desc: "技術フィンガープリント（配列フィールド）",
    ops: [
      { op: "=", desc: "あいまい一致" },
      { op: "==", desc: "完全一致" },
      { op: "!=", desc: "除外" },
    ],
  },
  {
    name: "port",
    desc: "ポート番号（整数）",
    ops: [
      { op: "==", desc: "等しい" },
      { op: "!=", desc: "等しくない" },
      { op: ">", desc: "より大きい" },
      { op: ">=", desc: "以上" },
      { op: "<", desc: "より小さい" },
      { op: "<=", desc: "以下" },
    ],
  },
  {
    name: "status_code",
    desc: "HTTP ステータスコード（整数）",
    ops: [
      { op: "==", desc: "等しい" },
      { op: "!=", desc: "等しくない" },
      { op: ">", desc: "より大きい" },
      { op: ">=", desc: "以上" },
      { op: "<", desc: "より小さい" },
      { op: "<=", desc: "以下" },
    ],
  },
  { name: "company_id", desc: "帰属企業 ID（整数）", ops: [{ op: "==", desc: "等しい" }] },
  { name: "task_id", desc: "由来タスク ID（整数）", ops: [{ op: "==", desc: "等しい" }] },
];

const LOGIC_OPS = [
  { label: "AND", desc: "かつ（両方の条件を満たす）" },
  { label: "OR", desc: "または（いずれかを満たす）" },
];

interface DslSuggestion {
  kind: "field" | "operator" | "logic";
  label: string;
  desc: string;
  replaceStart: number;
  replaceEnd: number;
  insertText: string;
}

function getDslSuggestions(text: string, cursor: number): DslSuggestion[] {
  const before = text.slice(0, cursor);
  // Current token: non-whitespace, non-paren run ending at cursor
  const tokenMatch = before.match(/([^\s()]*$)/);
  const currentToken = tokenMatch?.[1] ?? "";
  const tokenStart = cursor - currentToken.length;

  // Token already contains field+operator → typing a value, no suggestions
  if (/^[a-z_]+(==|!=|>=|<=|=|>|<)/.test(currentToken)) return [];

  // Complete known field name → suggest operators for that field
  const exactField = DSL_FIELDS.find((f) => f.name === currentToken.toLowerCase());
  if (exactField) {
    return exactField.ops.map(({ op, desc }) => ({
      kind: "operator",
      label: `${exactField.name}${op}`,
      desc,
      replaceStart: tokenStart,
      replaceEnd: cursor,
      insertText: `${exactField.name}${op}`,
    }));
  }

  // Everything before the current token (trimmed)
  const beforeToken = before.slice(0, tokenStart).trimEnd();
  const afterExpression = beforeToken.length > 0 && !/\b(AND|OR)\s*$/i.test(beforeToken) && !beforeToken.endsWith("(");

  // Current token is a prefix of AND/OR and follows a complete expression
  if (/^(a|an|and|o|or)$/i.test(currentToken) && afterExpression) {
    return LOGIC_OPS.filter((l) => l.label.startsWith(currentToken.toUpperCase())).map(({ label, desc }) => ({
      kind: "logic",
      label,
      desc,
      replaceStart: tokenStart,
      replaceEnd: cursor,
      insertText: `${label} `,
    }));
  }

  // No current token, after a complete expression → suggest AND/OR
  if (!currentToken && afterExpression) {
    return LOGIC_OPS.map(({ label, desc }) => ({
      kind: "logic",
      label,
      desc,
      replaceStart: cursor,
      replaceEnd: cursor,
      insertText: `${label} `,
    }));
  }

  // Default: suggest fields filtered by prefix
  const prefix = currentToken.toLowerCase();
  return DSL_FIELDS.filter((f) => f.name.startsWith(prefix)).map((f) => ({
    kind: "field",
    label: f.name,
    desc: f.desc,
    replaceStart: tokenStart,
    replaceEnd: cursor,
    insertText: f.name,
  }));
}

function applyDslSuggestion(text: string, s: DslSuggestion): { text: string; cursor: number } {
  const newText = text.slice(0, s.replaceStart) + s.insertText + text.slice(s.replaceEnd);
  return { text: newText, cursor: s.replaceStart + s.insertText.length };
}

const KIND_STYLE: Record<string, string> = {
  field: "text-blue-500 dark:text-blue-400",
  operator: "text-amber-500 dark:text-amber-400",
  logic: "text-emerald-500 dark:text-emerald-400",
};

// AssetDslSearch は共有の DSL 検索ボックス。等幅入力欄に
// フィールド/演算子/論理演算子のオートコンプリートポップオーバーとステータス行（「N 件見つかりました」/
// エラー / 読み込み中）を持つ。グローバルの資産ビューとタスク別ビューの両方で使う。
export function AssetDslSearch({
  query,
  onChange,
  loading,
  error,
  count,
}: {
  query: string;
  onChange: (v: string) => void;
  loading: boolean;
  error: string;
  count?: number;
}) {
  const inputRef = React.useRef<HTMLInputElement>(null);
  const [suggestions, setSuggestions] = React.useState<DslSuggestion[]>([]);
  const [selIdx, setSelIdx] = React.useState(0);
  const [open, setOpen] = React.useState(false);

  const refresh = React.useCallback((val: string, pos: number) => {
    const suggs = getDslSuggestions(val, pos);
    setSuggestions(suggs);
    setSelIdx(0);
    setOpen(suggs.length > 0);
  }, []);

  const handleChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const val = e.target.value;
    onChange(val);
    refresh(val, e.target.selectionStart ?? val.length);
  };

  const apply = React.useCallback(
    (s: DslSuggestion) => {
      const cursor = inputRef.current?.selectionStart ?? query.length;
      // use cursor for logic-kind (insert at cursor), replaceStart/End for others
      const adjusted: DslSuggestion =
        s.kind === "logic" && !query.slice(s.replaceStart, s.replaceEnd)
          ? { ...s, replaceStart: cursor, replaceEnd: cursor }
          : s;
      const { text: newText, cursor: newCursor } = applyDslSuggestion(query, adjusted);
      onChange(newText);
      requestAnimationFrame(() => {
        if (!inputRef.current) return;
        inputRef.current.setSelectionRange(newCursor, newCursor);
        inputRef.current.focus();
        refresh(newText, newCursor);
      });
    },
    [query, onChange, refresh],
  );

  const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (!open || suggestions.length === 0) return;
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setSelIdx((i) => Math.min(i + 1, suggestions.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setSelIdx((i) => Math.max(i - 1, 0));
    } else if (e.key === "Tab" || e.key === "Enter") {
      const s = suggestions[selIdx];
      if (s) {
        e.preventDefault();
        apply(s);
      }
    } else if (e.key === "Escape") {
      setOpen(false);
    }
  };

  const cursorPos = () => inputRef.current?.selectionStart ?? query.length;

  return (
    <div className="flex flex-col gap-1">
      <div className="relative max-w-lg">
        <SearchIcon className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
        <Input
          ref={inputRef}
          placeholder="DSL 検索: domain=example AND status_code>=400"
          value={query}
          onChange={handleChange}
          onKeyDown={handleKeyDown}
          onFocus={() => refresh(query, cursorPos())}
          onClick={() => refresh(query, cursorPos())}
          onBlur={() => setTimeout(() => setOpen(false), 120)}
          className="h-8 pl-8 font-mono text-xs"
        />
        {open && suggestions.length > 0 && (
          <div className="absolute top-full left-0 z-50 mt-1 w-max min-w-full max-w-sm rounded-md border bg-popover py-1 shadow-md">
            {suggestions.map((s, i) => (
              <button
                type="button"
                key={i}
                className={cn(
                  "flex w-full cursor-pointer items-center gap-3 px-3 py-1.5 text-left",
                  i === selIdx ? "bg-accent" : "hover:bg-accent/50",
                )}
                onMouseEnter={() => setSelIdx(i)}
                onMouseDown={(e) => {
                  e.preventDefault();
                  apply(s);
                }}
              >
                <span className={cn("shrink-0 font-mono text-xs font-semibold", KIND_STYLE[s.kind])}>{s.label}</span>
                <span className="text-xs text-muted-foreground">{s.desc}</span>
              </button>
            ))}
          </div>
        )}
      </div>
      {query.trim() && !open && (
        <p className="pl-1 text-[11px] text-muted-foreground">
          {loading ? (
            "検索中…"
          ) : error ? (
            <span className="text-destructive">{error}</span>
          ) : (
            `${count ?? 0} 件見つかりました`
          )}
        </p>
      )}
    </div>
  );
}
