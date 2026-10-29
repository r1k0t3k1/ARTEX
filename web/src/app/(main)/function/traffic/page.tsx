"use client";

import * as React from "react";

import {
  ArrowDownWideNarrowIcon,
  ArrowUpNarrowWideIcon,
  ChevronLeftIcon,
  ChevronRightIcon,
  EraserIcon,
  FilterXIcon,
  ListChecksIcon,
  Loader2Icon,
  RadioTowerIcon,
  SearchIcon,
  Trash2Icon,
} from "lucide-react";
import { toast } from "sonner";

import { HttpCodeBlock } from "@/components/http-code-block";
import { LinkTrafficDialog } from "@/components/link-traffic-dialog";
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
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { SortableHead } from "@/components/ui/sortable-head";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { api } from "@/lib/api";
import { useStoredSortPreference } from "@/lib/sort-preference";
import type { TrafficDetail, TrafficExchange, TrafficHost, TrafficResp } from "@/lib/types";
import { cn } from "@/lib/utils";

function fmtTime(ts: string) {
  return new Date(ts).toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function fmtBytes(n: number) {
  if (n <= 0) return "0 B";
  // GB は全削除時に報告される解放容量で重要になる。キャプチャの多いインスタンスでは
  // 数 GB が返ることもある。
  const units = ["B", "KB", "MB", "GB"];
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), units.length - 1);
  const v = n / 1024 ** i;
  return `${i === 0 ? v : v.toFixed(1)} ${units[i]}`;
}

function statusTone(status: number) {
  if (status >= 500) return "text-red-500";
  if (status >= 400) return "text-amber-500";
  if (status >= 300) return "text-blue-500";
  if (status >= 200) return "text-emerald-500";
  return "text-muted-foreground";
}

function MethodBadge({ method }: { method: string }) {
  return <Badge className="shrink-0 font-mono">{method}</Badge>;
}

// 古いキャプチャは Host の永続化より前のもので、net/http は Host を Request.Header の外に
// 保持する。表示用に補完する。新しく記録されるトラフィックはレコーダー層でも修正済み。
function requestWithHost(raw: string, exchange: TrafficExchange): string {
  if (!raw.trim() || /^host\s*:/im.test(raw)) return raw;
  let host = exchange.host;
  try {
    host = new URL(exchange.url).host || host;
  } catch {
    // 相対 URL や旧形式の URL はインデックス済みの host にフォールバックする。
  }
  const newline = raw.includes("\r\n") ? "\r\n" : "\n";
  const firstLineEnd = raw.indexOf(newline);
  if (firstLineEnd < 0) return `${raw}${newline}Host: ${host}`;
  return `${raw.slice(0, firstLineEnd + newline.length)}Host: ${host}${newline}${raw.slice(firstLineEnd + newline.length)}`;
}

// 固定のメソッド集合(サーバー側は完全一致でフィルタする)。1 ページから選択肢を導出すると
// そのページに現れるメソッドしか一覧に載らないため、固定集合を使う。
const METHODS = ["GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"];
const PAGE_SIZES = [25, 50, 100, 200];
type HostCountSortDirection = "asc" | "desc";

// サーバー側でソート可能な列。リストは `sort` としてそのままバックエンドに送られ、
// バックエンドは同じ名前のホワイトリストを使うため、traffic.Page と同期を保つこと。
const SORT_FIELDS = ["ts", "status", "resp_len"] as const;
type SortField = (typeof SORT_FIELDS)[number];
const SORT_STORAGE_KEY = "traffic-sort";

// フィルタドロップダウン用のステータスクラス区分。値は `status` として送られ、
// バックエンドは完全なコードまたは "Nxx" クラス帯として解釈する。
const STATUS_BUCKETS = ["2xx", "3xx", "4xx", "5xx"];

export default function TrafficPage() {
  const [selectedFlows, setSelectedFlows] = React.useState<Set<string>>(() => new Set());
  const [linking, setLinking] = React.useState(false);
  const [page, setPage] = React.useState(0);
  const [size, setSize] = React.useState(50);
  const [host, setHost] = React.useState(""); // 生の host 入力
  const [hostQ, setHostQ] = React.useState(""); // デバウンス済み → サーバー
  const [query, setQuery] = React.useState(""); // 生のフリーテキスト入力
  const [queryQ, setQueryQ] = React.useState(""); // デバウンス済み → サーバー
  const [method, setMethod] = React.useState("all");

  // 高度なフィルタ(issue #177):レスポンスボディの内容、パス、ステータスクラス、
  // レスポンスサイズの範囲。テキスト入力は host/query と同様にデバウンスし、
  // ステータスの選択は即時適用する。
  const [body, setBody] = React.useState("");
  const [bodyQ, setBodyQ] = React.useState("");
  const [path, setPath] = React.useState("");
  const [pathQ, setPathQ] = React.useState("");
  const [statusFilter, setStatusFilter] = React.useState("all");
  const [respMin, setRespMin] = React.useState("");
  const [respMinQ, setRespMinQ] = React.useState("");
  const [respMax, setRespMax] = React.useState("");
  const [respMaxQ, setRespMaxQ] = React.useState("");
  const [sort, setSort] = useStoredSortPreference<SortField>(SORT_STORAGE_KEY, SORT_FIELDS, "ts", "desc");

  const [traffic, setTraffic] = React.useState<TrafficResp | null>(null);
  const [selected, setSelected] = React.useState<TrafficExchange | null>(null);
  const [detail, setDetail] = React.useState<TrafficDetail | null>(null);
  const [detailLoading, setDetailLoading] = React.useState(false);

  const [hosts, setHosts] = React.useState<TrafficHost[]>([]); // ターゲットピッカー
  const [selectedHosts, setSelectedHosts] = React.useState<string[]>([]); // ピッカーでチェック済み
  const [pickerOpen, setPickerOpen] = React.useState(false);
  const [hostCountSortDirection, setHostCountSortDirection] = React.useState<HostCountSortDirection>("desc");

  const [deleteMode, setDeleteMode] = React.useState<"filter" | "selected" | "all" | null>(null); // null = ダイアログを閉じている
  const [deleting, setDeleting] = React.useState(false);
  const [reloadTick, setReloadTick] = React.useState(0); // 手動再取得のトリガー

  // 入力のたびに再取得しないよう、両方のフィルタをデバウンスする。
  React.useEffect(() => {
    const t = setTimeout(() => setHostQ(host.trim()), 300);
    return () => clearTimeout(t);
  }, [host]);
  React.useEffect(() => {
    const t = setTimeout(() => setQueryQ(query.trim()), 300);
    return () => clearTimeout(t);
  }, [query]);
  React.useEffect(() => {
    const t = setTimeout(() => setBodyQ(body.trim()), 300);
    return () => clearTimeout(t);
  }, [body]);
  React.useEffect(() => {
    const t = setTimeout(() => setPathQ(path.trim()), 300);
    return () => clearTimeout(t);
  }, [path]);
  React.useEffect(() => {
    const t = setTimeout(() => setRespMinQ(respMin.trim()), 300);
    return () => clearTimeout(t);
  }, [respMin]);
  React.useEffect(() => {
    const t = setTimeout(() => setRespMaxQ(respMax.trim()), 300);
    return () => clearTimeout(t);
  }, [respMax]);

  const hasAdvancedFilter = Boolean(bodyQ || pathQ || respMinQ || respMaxQ) || statusFilter !== "all";
  const resetAdvancedFilters = () => {
    setBody("");
    setBodyQ("");
    setPath("");
    setPathQ("");
    setStatusFilter("all");
    setRespMin("");
    setRespMinQ("");
    setRespMax("");
    setRespMaxQ("");
  };

  // フィルタ/件数/ソートの変更で先頭ページに戻る。
  // biome-ignore lint/correctness/useExhaustiveDependencies: これらの値は意図的にページリセットを起こす。
  React.useEffect(() => {
    setPage(0);
  }, [hostQ, queryQ, method, size, bodyQ, pathQ, statusFilter, respMinQ, respMaxQ, sort]);

  // 現在のページを読み込む。自動更新は先頭ページ(最新)のみ行い、履歴を遡っている最中に
  // 足元をすくわれないようにする。
  // biome-ignore lint/correctness/useExhaustiveDependencies: reloadTick は明示的な手動再取得のトリガー。
  React.useEffect(() => {
    let alive = true;
    const load = () => {
      api
        .traffic(page, size, hostQ, method, queryQ, {
          body: bodyQ,
          path: pathQ,
          status: statusFilter,
          respMin: respMinQ,
          respMax: respMaxQ,
          sort: sort.field,
          order: sort.direction,
        })
        .then((r) => {
          if (alive) setTraffic(r);
        })
        .catch(() => {
          // 一時的な更新失敗の間は最後に成功したスナップショットを保持する。
        });
      api
        .trafficHosts()
        .then((r) => {
          if (alive) setHosts(r.hosts ?? []);
        })
        .catch(() => {
          // 一時的な更新失敗の間は最後に成功した host 一覧を保持する。
        });
    };
    load();
    const t = setInterval(() => {
      if (page !== 0) return; // 最新ページのみ自動更新する
      load();
    }, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [page, size, hostQ, method, queryQ, bodyQ, pathQ, statusFilter, respMinQ, respMaxQ, sort, reloadTick]);

  // 現在の host フィルタ(部分一致)またはチェックした host(完全一致のバッチ)の
  // トラフィックを削除し、再取得する。
  const allSelected = hosts.length > 0 && hosts.every((h) => selectedHosts.includes(h.host));
  const sortedHosts = React.useMemo(
    () =>
      [...hosts].sort((a, b) => {
        const countOrder = hostCountSortDirection === "asc" ? a.count - b.count : b.count - a.count;
        return countOrder || a.host.localeCompare(b.host);
      }),
    [hosts, hostCountSortDirection],
  );

  // フィルタ無視の全削除は「クリア」、host 単位の削除は「削除」を使う。ダイアログの
  // タイトルと確認ボタンは、どちらが有効かによって決まる。
  const deleteVerb = deleteMode === "all" ? "クリア" : "削除";
  const deleteTitle = deleteMode
    ? {
        all: "すべてのトラフィック記録をクリアしますか？",
        selected: `選択した ${selectedHosts.length} 件のターゲットのトラフィックをすべて削除しますか？`,
        filter: "このターゲットのトラフィックをすべて削除しますか？",
      }[deleteMode]
    : "";

  // `reclaimed` は全削除のときだけ返る。host 単位の削除は行数のみを報告する。
  const requestDelete = (mode: "filter" | "selected" | "all"): Promise<{ deleted: number; reclaimed?: number }> => {
    if (mode === "all") return api.trafficDeleteAll();
    if (mode === "selected") return api.trafficDeleteHosts(selectedHosts);
    return api.trafficDeleteHost(hostQ);
  };

  const confirmDelete = () => {
    if (!deleteMode) return;
    setDeleting(true);
    const mode = deleteMode;
    requestDelete(mode)
      .then((r) => {
        setDeleteMode(null);
        setSelected(null);
        setDetail(null);
        if (mode !== "filter") {
          setSelectedHosts([]);
          setPickerOpen(false);
        }
        if (mode === "all") {
          // 空になったインデックスを圧縮する目的は解放容量なので、必ず伝える。
          const reclaimed = r.reclaimed ?? 0;
          const freed = reclaimed > 0 ? `、${fmtBytes(reclaimed)} のストレージを解放` : "";
          toast.success(`トラフィック ${r.deleted} 件をクリアしました${freed}`);
        }
        setPage(0);
        setReloadTick((t) => t + 1);
      })
      .catch((e) => {
        // 失敗した削除を再試行できるよう、確認ダイアログは開いたままにする。
        if (mode === "all") toast.error(`クリアに失敗しました：${(e as Error).message}`);
      })
      .finally(() => setDeleting(false));
  };

  // 選択した交換の生リクエスト/レスポンスを遅延読み込みする。
  React.useEffect(() => {
    if (!selected) {
      setDetail(null);
      return;
    }
    let alive = true;
    setDetailLoading(true);
    setDetail(null);
    api
      .trafficExchange(selected.id)
      .then((d) => {
        if (alive) setDetail(d);
      })
      .catch(() => {
        if (alive) setDetail({ req: "（メッセージを読み込めません）", resp: "" });
      })
      .finally(() => {
        if (alive) setDetailLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [selected]);

  // アクティブな列を再クリックすると方向を切り替え、それ以外は新しい列を
  // 新しい順/大きい順でソートする。
  const toggleSort = (field: SortField) =>
    setSort((prev) =>
      prev.field === field
        ? { field, direction: prev.direction === "asc" ? "desc" : "asc" }
        : { field, direction: "desc" },
    );

  const exchanges = React.useMemo(() => traffic?.exchanges ?? [], [traffic]);
  const total = traffic?.total ?? exchanges.length;
  const pageCount = Math.max(1, Math.ceil(total / size));
  const rangeStart = total === 0 ? 0 : page * size + 1;
  const rangeEnd = page * size + exchanges.length;

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4">
      <div className="flex items-center justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">トラフィック</h1>
          <p className="text-muted-foreground text-sm">グローバル録画プロキシ · すべての HTTP 通信</p>
        </div>
        <div className="flex items-center gap-4 text-sm">
          <span
            className={cn(
              "inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-xs font-medium",
              traffic?.enabled
                ? "border-emerald-500/20 bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
                : "border-transparent bg-muted text-muted-foreground",
            )}
          >
            <RadioTowerIcon className="size-3.5" />
            {traffic?.enabled ? "記録中" : "停止中"}
          </span>
          {traffic?.proxy && <span className="font-mono text-xs text-muted-foreground">{traffic.proxy}</span>}
          <span className="text-xs text-muted-foreground">
            合計 <span className="tabular-nums">{traffic?.count ?? 0}</span> 件
          </span>
        </div>
      </div>

      {/* ツールバー */}
      <div className="flex flex-wrap items-center gap-2">
        <Popover open={pickerOpen} onOpenChange={setPickerOpen}>
          <PopoverTrigger asChild>
            <Button variant="outline" size="sm" className="h-8">
              <ListChecksIcon className="size-3.5" />
              {selectedHosts.length > 0 ? `ターゲットを選択（${selectedHosts.length}）` : "ターゲットを選択…"}
            </Button>
          </PopoverTrigger>
          <PopoverContent
            className="w-[calc(100vw-2rem)] p-0 data-open:animate-none data-closed:animate-none sm:w-80"
            align="start"
            collisionPadding={16}
          >
            <div className="flex items-center justify-between border-b px-3 py-2">
              <span className="text-xs font-medium text-muted-foreground">ターゲット単位で一括削除</span>
              <div className="flex items-center gap-1">
                {hosts.length > 0 && (
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <Button
                        variant="ghost"
                        size="icon-xs"
                        onClick={() => setHostCountSortDirection((current) => (current === "desc" ? "asc" : "desc"))}
                        aria-label={
                          hostCountSortDirection === "desc"
                            ? "パケット数は現在降順です。クリックで昇順に切り替え"
                            : "パケット数は現在昇順です。クリックで降順に切り替え"
                        }
                      >
                        {hostCountSortDirection === "desc" ? <ArrowDownWideNarrowIcon /> : <ArrowUpNarrowWideIcon />}
                      </Button>
                    </TooltipTrigger>
                    <TooltipContent>パケット数で{hostCountSortDirection === "desc" ? "降順" : "昇順"}</TooltipContent>
                  </Tooltip>
                )}
                {hosts.length > 0 && (
                  <Button
                    variant="ghost"
                    size="sm"
                    className="h-6 px-2 text-xs"
                    onClick={() => setSelectedHosts(allSelected ? [] : hosts.map((h) => h.host))}
                  >
                    {allSelected ? "すべて解除" : "すべて選択"}
                  </Button>
                )}
              </div>
            </div>
            <div className="max-h-64 overflow-y-auto">
              {hosts.length === 0 ? (
                <div className="px-3 py-6 text-center text-xs text-muted-foreground">
                  トラフィックの記録はまだありません
                </div>
              ) : (
                sortedHosts.map((h, index) => (
                  <label
                    key={h.host}
                    htmlFor={`traffic-host-${index}`}
                    className="flex cursor-pointer items-center gap-2 px-3 py-1.5 text-xs hover:bg-accent"
                  >
                    <Checkbox
                      id={`traffic-host-${index}`}
                      checked={selectedHosts.includes(h.host)}
                      onCheckedChange={() =>
                        setSelectedHosts((prev) =>
                          prev.includes(h.host) ? prev.filter((x) => x !== h.host) : [...prev, h.host],
                        )
                      }
                    />
                    <span className="truncate font-mono">{h.host}</span>
                    <span className="ml-auto shrink-0 tabular-nums text-muted-foreground">{h.count}</span>
                  </label>
                ))
              )}
            </div>
            <div className="border-t p-2">
              <Button
                variant="destructive"
                size="sm"
                className="w-full"
                disabled={selectedHosts.length === 0}
                onClick={() => {
                  setDeleteMode("selected");
                  setPickerOpen(false);
                }}
              >
                選択項目を削除（{selectedHosts.length}）
              </Button>
            </div>
          </PopoverContent>
        </Popover>
        <div className="relative w-48">
          <Input placeholder="host…" value={host} onChange={(e) => setHost(e.target.value)} className="h-8" />
        </div>
        <Button
          variant="destructive"
          size="sm"
          className="h-8"
          disabled={!hostQ || deleting}
          title={hostQ ? undefined : "先に左側でターゲットを選択するか host を入力してください"}
          onClick={() => setDeleteMode("filter")}
        >
          <Trash2Icon className="size-3.5" />
          このターゲットを削除
        </Button>
        {/* 2 つ目の破壊的ボタンではなくアウトラインを使う。こちらはすべてのフィルタを無視するため、
            「このターゲットを削除」と誤クリック 1 回の距離に見えてはいけない。 */}
        <Button
          variant="outline"
          size="sm"
          className="h-8 text-destructive hover:bg-destructive/10 hover:text-destructive"
          disabled={!traffic?.count || deleting}
          title={
            traffic?.count ? "すべてのトラフィックを削除してストレージを圧縮" : "現在トラフィックの記録はありません"
          }
          onClick={() => setDeleteMode("all")}
        >
          <EraserIcon className="size-3.5" />
          すべてクリア
        </Button>
        <div className="relative max-w-sm flex-1">
          <SearchIcon className="absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            placeholder="すべて検索（URL / メソッド / タイプ / ステータスコード…）"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            className="h-8 pl-8"
          />
        </div>
        <Select value={method} onValueChange={setMethod}>
          <SelectTrigger size="sm" className="w-32">
            <SelectValue placeholder="メソッド" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">すべてのメソッド</SelectItem>
            {METHODS.map((m) => (
              <SelectItem key={m} value={m}>
                {m}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
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
            {page + 1} / {pageCount}
          </span>
          <Button
            variant="outline"
            size="icon"
            className="size-8"
            disabled={page + 1 >= pageCount}
            onClick={() => setPage((p) => Math.min(pageCount - 1, p + 1))}
          >
            <ChevronRightIcon />
          </Button>
        </div>
      </div>

      {/* 高度なフィルタ(issue #177):66 万件を超える交換から目的のパケット 1 件まで絞り込む。 */}
      <div className="flex flex-wrap items-center gap-2 rounded-md border bg-muted/30 px-2 py-1.5">
        <span className="pl-1 text-xs font-medium text-muted-foreground">高度なフィルタ</span>
        <div className="relative w-56">
          <Input
            placeholder="レスポンス内容（本文キーワード、3 文字以上）"
            value={body}
            onChange={(e) => setBody(e.target.value)}
            className="h-8"
          />
        </div>
        <div className="relative w-52">
          <Input
            placeholder="パス（例:/api/user/…）"
            value={path}
            onChange={(e) => setPath(e.target.value)}
            className="h-8"
          />
        </div>
        <Select value={statusFilter} onValueChange={setStatusFilter}>
          <SelectTrigger size="sm" className="w-28">
            <SelectValue placeholder="ステータスコード" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">すべてのステータスコード</SelectItem>
            {STATUS_BUCKETS.map((s) => (
              <SelectItem key={s} value={s}>
                {s}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <div className="flex items-center gap-1 text-xs text-muted-foreground">
          <span>レスポンス長</span>
          <Input
            type="number"
            min={0}
            placeholder="最小(B)"
            value={respMin}
            onChange={(e) => setRespMin(e.target.value)}
            className="h-8 w-24"
          />
          <span>–</span>
          <Input
            type="number"
            min={0}
            placeholder="最大(B)"
            value={respMax}
            onChange={(e) => setRespMax(e.target.value)}
            className="h-8 w-24"
          />
        </div>
        {hasAdvancedFilter ? (
          <Button variant="ghost" size="sm" className="h-8" onClick={resetAdvancedFilters}>
            <FilterXIcon className="size-3.5" />
            フィルタをクリア
          </Button>
        ) : null}
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm text-muted-foreground">選択中 {selectedFlows.size} 件のトラフィック</span>
        <Button variant="outline" size="sm" disabled={selectedFlows.size === 0} onClick={() => setLinking(true)}>
          脆弱性に関連付け
        </Button>
        {selectedFlows.size > 0 ? (
          <Button variant="ghost" size="sm" onClick={() => setSelectedFlows(new Set())}>
            選択をクリア
          </Button>
        ) : null}
      </div>
      {/* 履歴テーブル */}
      <div className="flex h-[calc(100vh-15rem)] min-h-0 flex-col">
        <Card className="flex min-h-0 flex-1 flex-col overflow-hidden py-0">
          <div className="min-h-0 flex-1 overflow-auto">
            <Table>
              <TableHeader className="sticky top-0 z-10 bg-card">
                <TableRow>
                  <TableHead className="w-10">
                    <Checkbox
                      aria-label="このページのトラフィックを選択"
                      checked={exchanges.length > 0 && exchanges.every((e) => selectedFlows.has(e.id))}
                      onCheckedChange={(checked) =>
                        setSelectedFlows((previous) => {
                          const next = new Set(previous);
                          for (const e of exchanges) {
                            if (checked === true) next.add(e.id);
                            else next.delete(e.id);
                          }
                          return next;
                        })
                      }
                    />
                  </TableHead>
                  <SortableHead
                    field="ts"
                    label="時刻"
                    activeField={sort.field}
                    direction={sort.direction}
                    onSort={toggleSort}
                    className="w-36"
                  />
                  <TableHead className="w-44">host</TableHead>
                  <TableHead className="w-20">メソッド</TableHead>
                  <TableHead>URL</TableHead>
                  <SortableHead
                    field="status"
                    label="ステータスコード"
                    activeField={sort.field}
                    direction={sort.direction}
                    onSort={toggleSort}
                    className="w-20"
                  />
                  <TableHead className="w-36">content-type</TableHead>
                  <SortableHead
                    field="resp_len"
                    label="レスポンス長"
                    activeField={sort.field}
                    direction={sort.direction}
                    onSort={toggleSort}
                    align="right"
                    className="w-24 text-right"
                  />
                </TableRow>
              </TableHeader>
              <TableBody>
                {exchanges.map((e) => (
                  <TableRow
                    key={e.id}
                    className={cn("cursor-pointer", selected?.id === e.id && "bg-accent hover:bg-accent")}
                    onClick={() => setSelected(e)}
                  >
                    <TableCell>
                      <Checkbox
                        aria-label={`トラフィック ${e.id} を選択`}
                        checked={selectedFlows.has(e.id)}
                        onClick={(event) => event.stopPropagation()}
                        onCheckedChange={(checked) =>
                          setSelectedFlows((previous) => {
                            const next = new Set(previous);
                            if (checked === true) next.add(e.id);
                            else next.delete(e.id);
                            return next;
                          })
                        }
                      />
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground tabular-nums">{fmtTime(e.ts)}</TableCell>
                    <TableCell className="font-mono text-xs">{e.host}</TableCell>
                    <TableCell>
                      <MethodBadge method={e.method} />
                    </TableCell>
                    <TableCell className="max-w-0">
                      <span className="block truncate font-mono text-xs">{e.url}</span>
                    </TableCell>
                    <TableCell>
                      <span className={cn("font-mono text-xs font-semibold tabular-nums", statusTone(e.status))}>
                        {e.status}
                      </span>
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">{e.content_type}</TableCell>
                    <TableCell className="text-right text-xs tabular-nums">{fmtBytes(e.resp_len)}</TableCell>
                  </TableRow>
                ))}
                {exchanges.length === 0 && (
                  <TableRow>
                    <TableCell colSpan={8} className="py-12 text-center text-sm text-muted-foreground">
                      {traffic === null ? "読み込み中…" : "一致するトラフィックはありません。"}
                    </TableCell>
                  </TableRow>
                )}
              </TableBody>
            </Table>
          </div>
        </Card>
      </div>

      {linking ? (
        <LinkTrafficDialog
          trafficIds={[...selectedFlows]}
          onClose={() => setLinking(false)}
          onBound={() => setSelectedFlows(new Set())}
        />
      ) : null}
      <Sheet open={selected !== null} onOpenChange={(open) => !open && setSelected(null)}>
        <SheetContent className="w-full! max-w-none! gap-0 p-0 sm:w-[48rem]! sm:max-w-[48rem]!">
          {selected && (
            <>
              <SheetHeader className="border-b px-5 py-4">
                <div className="flex items-center gap-2 pr-8">
                  <MethodBadge method={selected.method} />
                  <Badge variant="secondary" className={cn("font-mono tabular-nums", statusTone(selected.status))}>
                    {selected.status}
                  </Badge>
                  <span className="ml-auto text-xs text-muted-foreground tabular-nums">{fmtTime(selected.ts)}</span>
                </div>
                <SheetTitle className="break-all font-mono">{selected.host}</SheetTitle>
                <SheetDescription className="break-all font-mono">{selected.url}</SheetDescription>
              </SheetHeader>
              <Tabs defaultValue="request" className="min-h-0 flex-1 gap-0">
                <TabsList className="mx-5 mt-4 grid w-auto grid-cols-2">
                  <TabsTrigger value="request">リクエスト Request</TabsTrigger>
                  <TabsTrigger value="response">レスポンス Response</TabsTrigger>
                </TabsList>
                <TabsContent value="request" className="min-h-0 overflow-auto">
                  {detailLoading ? (
                    <div className="flex items-center gap-2 p-5 text-xs text-muted-foreground">
                      <Loader2Icon className="size-3.5 animate-spin" />
                      メッセージを読み込み中…
                    </div>
                  ) : (
                    <HttpCodeBlock raw={requestWithHost(detail?.req ?? "", selected)} />
                  )}
                </TabsContent>
                <TabsContent value="response" className="min-h-0 overflow-auto">
                  {detailLoading ? (
                    <div className="flex items-center gap-2 p-5 text-xs text-muted-foreground">
                      <Loader2Icon className="size-3.5 animate-spin" />
                      メッセージを読み込み中…
                    </div>
                  ) : (
                    <HttpCodeBlock raw={detail?.resp ?? ""} />
                  )}
                </TabsContent>
              </Tabs>
            </>
          )}
        </SheetContent>
      </Sheet>

      <AlertDialog
        open={deleteMode !== null}
        onOpenChange={(o) => {
          if (!o) setDeleteMode(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{deleteTitle}</AlertDialogTitle>
            <AlertDialogDescription>
              {deleteMode === "all" && (
                <>
                  <span className="font-semibold tabular-nums">{traffic?.count ?? 0}</span>{" "}
                  件のトラフィック記録(リクエスト/レスポンスの原文を含む)をすべて完全に削除します。現在のフィルタ条件は無視され、この操作は元に戻せません。脆弱性に紐付け済みのトラフィック証拠は独立した証拠ライブラリに保存されているため影響を受けません。
                  <br />
                  <span className="text-muted-foreground">
                    クリア後はストレージも圧縮され、インデックスが占有していたディスク領域がシステムに返されます。その間、トラフィックの記録は一時的に停止します。
                  </span>
                </>
              )}
              {deleteMode === "selected" && (
                <>
                  <span className="font-semibold tabular-nums">{selectedHosts.length}</span> 件のターゲット（
                  <span className="font-mono">
                    {selectedHosts.slice(0, 3).join("、")}
                    {selectedHosts.length > 3 ? "…" : ""}
                  </span>
                  ）のすべてのトラフィック記録(リクエスト/レスポンスの原文を含む)を完全に削除します。この操作は元に戻せません。
                </>
              )}
              {deleteMode === "filter" && (
                <>
                  host に <span className="font-mono font-semibold">{hostQ}</span>{" "}
                  を含むすべてのトラフィック記録(リクエスト/レスポンスの原文を含む)を完全に削除します。この操作は元に戻せません。
                </>
              )}
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
              {deleting ? `${deleteVerb}中…` : `${deleteVerb}する`}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
