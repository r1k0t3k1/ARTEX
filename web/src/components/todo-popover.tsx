"use client";

import * as React from "react";
import { ListTodo } from "lucide-react";

import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/lib/utils";

// TodoPopover はセッション（チャット会話、またはタスクのワーカー/プランナーのリプレイ）の
// 最新の TodoWrite 状態を表示する。完全な Todo リストはアクティビティ一覧には含まれず
// （詳細は遅延取得）、開いたときに seq で最新の TodoWrite 呼び出しの詳細を取得し、
// その {todos:[…]} JSON を解析する。完全にクライアントサイドで動作し、呼び出し側が
// seq と詳細取得関数（会話または探索のエンドポイント）を渡す。
export function TodoPopover({
  seq,
  fetchDetail,
}: {
  seq: number | null;
  fetchDetail: (seq: number) => Promise<string>;
}) {
  const [open, setOpen] = React.useState(false);
  const [todos, setTodos] = React.useState<{ content: string; status: string }[] | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [err, setErr] = React.useState("");

  const load = React.useCallback(async () => {
    if (seq == null) return;
    setLoading(true);
    setErr("");
    try {
      const detail = await fetchDetail(seq);
      const start = detail.indexOf("{"); // detail には "TodoWrite " のプレフィックスが付く場合がある
      const parsed = JSON.parse(start >= 0 ? detail.slice(start) : detail);
      setTodos(Array.isArray(parsed?.todos) ? parsed.todos : []);
    } catch {
      setErr("Todo の解析に失敗しました");
      setTodos(null);
    } finally {
      setLoading(false);
    }
  }, [seq, fetchDetail]);

  // 開くたびに再取得する — Todo は実行の進行に伴って変化するため。
  React.useEffect(() => {
    if (open) load();
  }, [open, load]);

  const disabled = seq == null;
  const MARK: Record<string, string> = { pending: "☐", in_progress: "▶", completed: "✔" };
  return (
    <Popover open={open} onOpenChange={disabled ? undefined : setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          disabled={disabled}
          title={disabled ? "このセッションには Todo がない" : "最近の Todo を表示"}
          className="text-muted-foreground/70 hover:text-primary flex items-center gap-0.5 text-xs disabled:pointer-events-none disabled:opacity-40"
        >
          <ListTodo className="size-3" />
          Todo
        </button>
      </PopoverTrigger>
      <PopoverContent align="end" className="max-h-80 w-80 overflow-auto p-2">
        <p className="text-muted-foreground px-1 pb-1 text-[11px] font-medium">
          最近の Todo{loading ? " · 読み込み中…" : ""}
        </p>
        {err && <p className="text-destructive px-1 text-xs">{err}</p>}
        {todos && todos.length === 0 && !loading && <p className="text-muted-foreground px-1 text-xs">（空）</p>}
        <ul className="space-y-0.5">
          {(todos ?? []).map((t, i) => (
            <li
              key={`${i}:${t.content}`}
              className={cn(
                "flex gap-1.5 px-1 text-xs",
                t.status === "completed" && "text-muted-foreground line-through",
              )}
            >
              <span className="shrink-0">{MARK[t.status] ?? "☐"}</span>
              <span className="break-words">{t.content}</span>
            </li>
          ))}
        </ul>
      </PopoverContent>
    </Popover>
  );
}
