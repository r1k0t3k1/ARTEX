"use client";

import * as React from "react";
import {
  RadioIcon,
  SearchIcon,
  ChevronLeftIcon,
  ChevronRightIcon,
  Loader2Icon,
  XIcon,
  Trash2Icon,
  CopyIcon,
  CheckIcon,
} from "lucide-react";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";

import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { cn } from "@/lib/utils";
import { api } from "@/lib/api";
import type { LLMRecordItem, LLMRecordDetail, LLMTask } from "@/lib/types";

function fmtTime(ts: string) {
  return new Date(ts).toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function fmtLatency(ms: number) {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

function fmtTokens(n: number) {
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`;
  return String(n);
}

function tryFormatJSON(s: string): string {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

// 現在の枠内テキストをコピーする小さなボタン。コピー成功後は一時的にチェックマークを表示する。text が空/プレースホルダーのみのときは無効化する。
function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = React.useState(false);
  const timer = React.useRef<ReturnType<typeof setTimeout> | null>(null);
  React.useEffect(
    () => () => {
      if (timer.current) clearTimeout(timer.current);
    },
    [],
  );

  const disabled = !text;
  const copy = async () => {
    if (disabled) return;
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      // navigator.clipboard は安全でないコンテキスト(http の LAN など)では使えないため、execCommand にフォールバックする。
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      try {
        document.execCommand("copy");
      } catch {
        /* 無視:未対応なら黙って諦める */
      }
      document.body.removeChild(ta);
    }
    setCopied(true);
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(false), 1500);
  };

  return (
    <Button
      variant="ghost"
      size="icon"
      className="size-5 shrink-0"
      disabled={disabled}
      title={copied ? "コピーしました" : "内容をコピー"}
      onClick={copy}
    >
      {copied ? <CheckIcon className="size-3 text-emerald-600" /> : <CopyIcon className="size-3" />}
    </Button>
  );
}

const PAGE_SIZES = [25, 50, 100];

export default function LLMRecordsPage() {
  const [page, setPage] = React.useState(0);
  const [size, setSize] = React.useState(50);
  const [session, setSession] = React.useState("");
  const [sessionQ, setSessionQ] = React.useState("");
  const [model, setModel] = React.useState("");

  const [records, setRecords] = React.useState<LLMRecordItem[]>([]);
  const [total, setTotal] = React.useState(0);
  const [loading, setLoading] = React.useState(false);

  // 記録のオン/オフ切り替え(settings.llm_record、既定はオフ)。オフのときバックエンドは何も記録しない。
  const [recEnabled, setRecEnabled] = React.useState(false);
  const [recBusy, setRecBusy] = React.useState(false);

  // インラインの詳細パネル(Burp 風の分割表示で、ダイアログではない)
  const [selected, setSelected] = React.useState<LLMRecordItem | null>(null);
  const [detail, setDetail] = React.useState<LLMRecordDetail | null>(null);
  const [detailLoading, setDetailLoading] = React.useState(false);
  // 正規化ビュー / HTTP 原文ビュー。原文は provider 側の問題を調査する唯一の手がかり:
  // 正規化ビューにはツール schema が含まれず、レスポンスにも tool_use ブロックがない。
  const [rawView, setRawView] = React.useState(false);

  const hasRaw = !!(detail?.raw_request || detail?.raw_response);
  // スイッチはユーザーの選択を保持するが、原文のない古いレコードに切り替えたときは空白ではなく解析ビューに自動で戻す。
  const showRaw = rawView && hasRaw;
  // 原文リクエストボディは JSON で、pretty-print は整形のみで意味を変えないため読みやすくなる。原文レスポンスは SSE
  // フレームで、tryFormatJSON は解析に失敗するとそのまま返すので、両方で同じ関数を共有して構わない。
  const reqText = showRaw
    ? detail?.raw_request && tryFormatJSON(detail.raw_request)
    : detail?.request_body && tryFormatJSON(detail.request_body);
  const respText = showRaw ? detail?.raw_response : detail?.response_body && tryFormatJSON(detail.response_body);

  // タスク単位の削除(タスク選択 + 確認ダイアログ)
  const [tasks, setTasks] = React.useState<LLMTask[]>([]);
  const [pickedTask, setPickedTask] = React.useState("");
  const [deleteOpen, setDeleteOpen] = React.useState(false);
  const [deleting, setDeleting] = React.useState(false);
  const [reloadTick, setReloadTick] = React.useState(0); // 手動再取得のトリガー

  // マウント時に記録スイッチの状態を読み込む。
  React.useEffect(() => {
    let alive = true;
    api
      .settings()
      .then((s) => {
        if (alive) setRecEnabled(!!s.llm_record);
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, []);

  const toggleRecording = async (on: boolean) => {
    setRecBusy(true);
    setRecEnabled(on); // 楽観的更新
    try {
      const s = await api.setSettings({ llm_record: on });
      setRecEnabled(!!s.llm_record);
    } catch {
      setRecEnabled(!on); // 失敗時は元に戻す
    } finally {
      setRecBusy(false);
    }
  };

  // Session フィルタのデバウンス。
  React.useEffect(() => {
    const t = setTimeout(() => setSessionQ(session.trim()), 300);
    return () => clearTimeout(t);
  }, [session]);

  // フィルタ変更時はページをリセットする。
  React.useEffect(() => {
    setPage(0);
  }, [sessionQ, model, size, pickedTask]);

  // 一覧を読み込む。
  React.useEffect(() => {
    let alive = true;
    setLoading(true);
    api
      .llmRecords({
        model: model || undefined,
        session: sessionQ || undefined,
        task: pickedTask || undefined,
        page,
        size,
      })
      .then((r) => {
        if (!alive) return;
        setRecords(r.records ?? []);
        setTotal(r.total ?? 0);
      })
      .catch(() => {})
      .finally(() => alive && setLoading(false));
    api
      .llmTasks()
      .then((r) => {
        if (alive) setTasks(r.tasks ?? []);
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [page, size, sessionQ, model, pickedTask, reloadTick]);

  // 選択したタスクの LLM レコードをすべて削除し、再取得する。
  const confirmDelete = () => {
    setDeleting(true);
    api
      .llmRecordsDeleteTask(pickedTask)
      .then(() => {
        setDeleteOpen(false);
        setSelected(null);
        setDetail(null);
        setPickedTask("");
        setPage(0);
        setReloadTick((t) => t + 1);
      })
      .catch(() => {})
      .finally(() => setDeleting(false));
  };

  // 行を選択したときにリクエスト/レスポンス全体を遅延読み込みする。
  React.useEffect(() => {
    if (!selected) {
      setDetail(null);
      return;
    }
    let alive = true;
    setDetailLoading(true);
    setDetail(null);
    api
      .llmRecordDetail(selected.id)
      .then((d) => {
        if (alive) setDetail(d);
      })
      .catch(() => {})
      .finally(() => {
        if (alive) setDetailLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [selected]);

  const totalPages = Math.max(1, Math.ceil(total / size));
  const rangeStart = total === 0 ? 0 : page * size + 1;
  const rangeEnd = page * size + records.length;

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4">
      {/* ヘッダー */}
      <div className="flex items-center justify-between gap-4">
        <div className="flex items-center gap-2">
          <RadioIcon className="h-5 w-5 text-muted-foreground" />
          <h1 className="text-xl font-semibold tracking-tight">LLM 記録</h1>
          <Badge variant="secondary">{total}</Badge>
        </div>
      </div>

      {/* ツールバー */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative max-w-sm flex-1">
          <SearchIcon className="absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            placeholder="Session ID を検索..."
            value={session}
            onChange={(e) => setSession(e.target.value)}
            className="h-8 pl-8"
          />
        </div>
        <Input placeholder="Model" className="h-8 w-48" value={model} onChange={(e) => setModel(e.target.value)} />
        <Select value={pickedTask} onValueChange={setPickedTask}>
          <SelectTrigger size="sm" className="w-56">
            <SelectValue placeholder="タスクを選択…" />
          </SelectTrigger>
          <SelectContent>
            {tasks.length === 0 ? (
              <SelectItem value="__none__" disabled>
                タスクの記録はまだありません
              </SelectItem>
            ) : (
              tasks.map((t) => (
                <SelectItem key={t.task_id} value={t.task_id}>
                  <span className="font-mono">#{t.task_id}</span>
                  <span className="ml-2 text-muted-foreground">（{t.count}）</span>
                </SelectItem>
              ))
            )}
          </SelectContent>
        </Select>
        <Button
          variant="destructive"
          size="sm"
          className="h-8"
          disabled={!pickedTask || deleting}
          title={pickedTask ? undefined : "先に上のタスクを選択してください"}
          onClick={() => setDeleteOpen(true)}
        >
          <Trash2Icon className="size-3.5" />
          タスクの対話を削除
        </Button>
        <Select value={String(size)} onValueChange={(v) => setSize(Number(v))}>
          <SelectTrigger size="sm" className="w-28">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {PAGE_SIZES.map((n) => (
              <SelectItem key={n} value={String(n)}>
                {n} / ページ
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        {/* 記録のオン/オフ。オフ = LLM 呼び出しを一切記録しない */}
        <div className="flex items-center gap-2 rounded-md border px-2.5 py-1">
          <Switch
            id="llm-rec-toggle"
            size="sm"
            checked={recEnabled}
            onCheckedChange={toggleRecording}
            disabled={recBusy}
          />
          <label
            htmlFor="llm-rec-toggle"
            className={cn(
              "cursor-pointer text-xs font-medium select-none",
              recEnabled ? "text-foreground" : "text-muted-foreground",
            )}
          >
            {recEnabled ? "記録中" : "オフ"}
          </label>
        </div>

        <div className="ml-auto flex items-center gap-2 text-xs text-muted-foreground">
          <span className="tabular-nums">
            {rangeStart}–{rangeEnd} / {total}
          </span>
          <Button
            variant="outline"
            size="icon"
            className="size-8"
            disabled={page <= 0}
            onClick={() => setPage((p) => Math.max(0, p - 1))}
          >
            <ChevronLeftIcon />
          </Button>
          <span className="tabular-nums">
            {page + 1} / {totalPages}
          </span>
          <Button
            variant="outline"
            size="icon"
            className="size-8"
            disabled={page + 1 >= totalPages}
            onClick={() => setPage((p) => Math.min(totalPages - 1, p + 1))}
          >
            <ChevronRightIcon />
          </Button>
        </div>
      </div>

      {/* 履歴テーブル + インライン詳細(Burp 風の分割表示) */}
      <div className="flex h-[calc(100vh-13rem)] min-h-0 flex-col gap-3">
        <Card className="flex min-h-0 flex-1 flex-col overflow-hidden py-0">
          <div className="min-h-0 flex-1 overflow-auto">
            <Table>
              <TableHeader className="sticky top-0 z-10 bg-card">
                <TableRow>
                  <TableHead className="w-[130px]">時刻</TableHead>
                  <TableHead className="w-[60px]">タスク</TableHead>
                  <TableHead className="w-[90px]">Worker</TableHead>
                  <TableHead className="w-[100px]">Profile</TableHead>
                  <TableHead className="w-[140px]">Model</TableHead>
                  <TableHead className="w-[70px]">レイテンシ</TableHead>
                  <TableHead className="w-[90px]">Tokens</TableHead>
                  <TableHead className="w-[60px]">ステータス</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {loading && records.length === 0 ? (
                  <TableRow>
                    <TableCell colSpan={8} className="py-12 text-center">
                      <Loader2Icon className="mx-auto h-5 w-5 animate-spin text-muted-foreground" />
                    </TableCell>
                  </TableRow>
                ) : records.length === 0 ? (
                  <TableRow>
                    <TableCell colSpan={8} className="py-12 text-center text-sm text-muted-foreground">
                      LLM 呼び出しの記録はまだありません
                    </TableCell>
                  </TableRow>
                ) : (
                  records.map((rec) => (
                    <TableRow
                      key={rec.id}
                      className={cn("cursor-pointer", selected?.id === rec.id && "bg-accent hover:bg-accent")}
                      onClick={() => setSelected(rec)}
                    >
                      <TableCell className="text-xs text-muted-foreground tabular-nums">{fmtTime(rec.ts)}</TableCell>
                      <TableCell className="text-xs font-mono text-muted-foreground">
                        {rec.task_id ? `#${rec.task_id}` : "-"}
                      </TableCell>
                      <TableCell>
                        <Badge variant="outline" className="text-xs font-mono">
                          {rec.worker || "-"}
                        </Badge>
                      </TableCell>
                      <TableCell>
                        <span className="text-xs">{rec.profile_name || "-"}</span>
                      </TableCell>
                      <TableCell>
                        <span className="text-xs font-mono">{rec.model || "-"}</span>
                      </TableCell>
                      <TableCell>
                        <span className={cn("text-xs", rec.latency_ms > 30000 && "text-amber-500")}>
                          {fmtLatency(rec.latency_ms)}
                        </span>
                      </TableCell>
                      <TableCell>
                        <span className="text-xs">
                          {fmtTokens(rec.input_tokens)} / {fmtTokens(rec.output_tokens)}
                        </span>
                      </TableCell>
                      <TableCell>
                        {rec.status === "ok" ? (
                          <Badge variant="secondary" className="text-xs text-emerald-600">
                            OK
                          </Badge>
                        ) : (
                          <Badge variant="destructive" className="text-xs">
                            Error
                          </Badge>
                        )}
                      </TableCell>
                    </TableRow>
                  ))
                )}
              </TableBody>
            </Table>
          </div>
        </Card>

        {/* インライン詳細パネル */}
        {selected && (
          <Card className="flex h-[42%] min-h-0 flex-col overflow-hidden py-0">
            {/* 詳細ヘッダー */}
            <div className="flex items-center gap-2 border-b px-3 py-2">
              <Badge variant="outline" className="text-xs font-mono">
                #{selected.id}
              </Badge>
              <Badge variant="outline" className="text-xs font-mono">
                {selected.profile_name || "-"}
              </Badge>
              <Badge variant="outline" className="text-xs font-mono">
                {selected.model || "-"}
              </Badge>
              {selected.task_id && (
                <Badge variant="outline" className="text-xs font-mono">
                  タスク #{selected.task_id}
                </Badge>
              )}
              <span className="text-xs text-muted-foreground">{fmtTime(selected.ts)}</span>
              <span className={cn("text-xs", selected.latency_ms > 30000 && "text-amber-500")}>
                {fmtLatency(selected.latency_ms)}
              </span>
              {selected.status === "ok" ? (
                <Badge variant="secondary" className="text-xs text-emerald-600">
                  OK
                </Badge>
              ) : (
                <Badge variant="destructive" className="text-xs">
                  Error
                </Badge>
              )}
              {/* 原文ビューのスイッチ。古いレコードには原文がないため、その場合は黙って
                  フォールバックせず無効化し、「原文と解析結果が一致している」ように見えるのを防ぐ。 */}
              <Button
                variant={showRaw ? "secondary" : "ghost"}
                size="sm"
                className="ml-auto h-7 shrink-0 text-xs"
                disabled={!hasRaw}
                title={
                  hasRaw
                    ? "provider と実際にやり取りした HTTP 原文を表示"
                    : "このレコードはこの機能の公開前に記録されたため原文がありません"
                }
                onClick={() => setRawView((v) => !v)}
              >
                原文
              </Button>
              <Button variant="ghost" size="icon" className="size-7 shrink-0" onClick={() => setSelected(null)}>
                <XIcon />
              </Button>
            </div>
            {/* Request / Response の分割表示 */}
            <div className="grid min-h-0 flex-1 grid-cols-2 divide-x">
              <div className="flex min-h-0 min-w-0 flex-col">
                <div className="flex items-center gap-2 border-b py-0.5 pr-1.5 pl-3 text-[11px] font-medium text-muted-foreground">
                  <span>Request{showRaw && " · 原文"}</span>
                  <CopyButton text={reqText || ""} />
                </div>
                <div className="min-h-0 flex-1 overflow-auto">
                  {detailLoading ? (
                    <div className="flex items-center gap-2 p-3 text-xs text-muted-foreground">
                      <Loader2Icon className="size-3.5 animate-spin" />
                      読み込み中…
                    </div>
                  ) : (
                    <pre className="p-3 font-mono text-xs break-all whitespace-pre-wrap">{reqText || "（空）"}</pre>
                  )}
                </div>
              </div>
              <div className="flex min-h-0 min-w-0 flex-col">
                <div className="flex items-center gap-2 border-b py-0.5 pr-1.5 pl-3 text-[11px] font-medium text-muted-foreground">
                  <span>Response{showRaw && " · 原文（SSE）"}</span>
                  <CopyButton text={respText || ""} />
                </div>
                <div className="min-h-0 flex-1 overflow-auto">
                  {detailLoading ? (
                    <div className="flex items-center gap-2 p-3 text-xs text-muted-foreground">
                      <Loader2Icon className="size-3.5 animate-spin" />
                      読み込み中…
                    </div>
                  ) : (
                    <pre
                      className={cn(
                        "p-3 font-mono text-xs break-all whitespace-pre-wrap",
                        selected.status !== "ok" && "text-red-600 dark:text-red-400",
                      )}
                    >
                      {respText || "（空）"}
                    </pre>
                  )}
                </div>
              </div>
            </div>
          </Card>
        )}
      </div>

      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>タスク「{pickedTask}」の LLM 対話をすべて削除しますか？</AlertDialogTitle>
            <AlertDialogDescription>
              このタスクのすべての LLM
              呼び出し記録(リクエスト/レスポンスの原文を含む)が完全に削除されます。この操作は元に戻せません。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>キャンセル</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault();
                confirmDelete();
              }}
              disabled={deleting}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {deleting ? "削除中…" : "削除する"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
