"use client";

import * as React from "react";

import {
  ArrowUpIcon,
  BrainIcon,
  ChevronDownIcon,
  CircleCheckIcon,
  CircleSlashIcon,
  CircleXIcon,
  ClockIcon,
  HistoryIcon,
  Loader2Icon,
  PaperclipIcon,
  PauseIcon,
  PlusIcon,
  RadioIcon,
  RotateCwIcon,
  ShieldAlertIcon,
  SquareIcon,
  Trash2Icon,
  UserIcon,
  WifiOffIcon,
  XIcon,
  ZapOffIcon,
} from "lucide-react";
import { toast } from "sonner";

import { ApprovalExecutionFocus, useApprovalFocus, useApprovalHistory } from "@/components/approval-execution-focus";
import { MentionTextarea } from "@/components/mention-textarea";
import { SideQuestionButton, SideQuestionWorkspace } from "@/components/side-question-workspace";
import { TodoPopover } from "@/components/todo-popover";
import { Transcript } from "@/components/transcript";
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
import { InputGroup, InputGroupAddon, InputGroupButton } from "@/components/ui/input-group";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { useSideQuestions } from "@/hooks/use-side-questions";
import { api, sseUrl } from "@/lib/api";
import { shouldSubmitOnKey, useChatSendMode } from "@/lib/chat-send-mode";
import { MOCK } from "@/lib/mock/enabled";
import { isBtwCommand } from "@/lib/side-questions";
import { taskAssetSourceLabel, taskAssetTypeLabel } from "@/lib/task-assets";
import type {
  Activity,
  ChatAttachment,
  IntentAsset,
  InterceptApprovalRow,
  Session,
  SessionStatus,
  SessionTokenUsage,
  TaskLLMResolution,
  TaskLLMResolutions,
  TaskNode,
  TokenTotal,
} from "@/lib/types";
import { cn } from "@/lib/utils";

// resolutionLabel はセッションが動作する LLM の名前を返す。バッジはこれのみを表示し、
// モデル id はツールチップに保持する。env ベースの設定は名前なしで届くことがあるため、
// 空のバッジを描画せずモデル id にフォールバックする。
function resolutionLabel(r: TaskLLMResolution): string {
  return r.name || r.model || "名称未設定の設定";
}

// fmtBytes は添付チップ用に人間が読めるファイルサイズを描画する（transcript.tsx と同じ）。
function fmtBytes(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`;
  return `${n} B`;
}

// ── 信頼性モデル（docs/task-session-history-sse-remediation.md 参照） ──────────
// タスクのアクティビティは、SSE since=0 から再生する無制限の `allActivity` 配列では
// なくなった。代わりに:
//   • 各 UI セッション（main | plan | intent:<id>）が、遅延読み込み・逆方向ページ
//     ネーションの独自キャッシュ（下記 SessionState）を持つ。セッションを開くと最新
//     ページのみを読み込み、上へスクロールすると古い履歴を追加読み込みする。
//   • タスク単位の単一 SSE（最初の履歴ページの since=snapshot_cursor で開く）が全
//     Agent の新規アクティビティを追跡し、フレームは session_key で振り分ける。
//   • 履歴と SSE は snapshot_cursor で隙間なく接続し、seq でマージ（重複排除）される
//     ため、リフレッシュ / タブ切替 / スリープ / 再接続でも最新レコードを失わない。

const PAGE = 200; // 履歴のページサイズ
const SYSTEM_SCAN_PAGE = 500; // 疎なシステム監査イベントを回収するために走査する汎用アクティビティのページサイズ
const MAX_KEEP = 4000; // セッションごとのメモリ上限。古いページは上スクロール時に再取得する
const STREAM_WINDOW_MS = 5000; // 「ライブ」= この時間内に観測されたアクティビティ
const MAX_WORKER_MESSAGE_CHARS = 4000;

function newWorkerMessageRequestID(): string {
  return (
    globalThis.crypto?.randomUUID?.() ??
    `worker-message-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`
  );
}

function workerMessageCharCount(value: string): number {
  return Array.from(value).length;
}

// 1 セッション分の遅延読み込み・逆方向ページネーションキャッシュ。`lastTs`/`unread` は
// 一度も開かれていないセッションでも最新に保たれ、完全な履歴を保持せずに一覧へライブ状態と
// 未読数を表示できる。
type SessionState = {
  items: Activity[];
  loaded: boolean;
  loading: boolean;
  loadingMore: boolean;
  hasMore: boolean; // 読み込み済みウィンドウより上に古い履歴が残っている
  earliestSeq: number; // 読み込み済みの最古 id — 逆方向ページネーションのアンカー
  unread: number;
  lastTs: string; // 最新アクティビティ時刻（ライブバッジの判定に使用。未読み込みでも更新される）
  error?: string;
};
type SessionStore = Record<string, SessionState>;

function emptyState(): SessionState {
  return {
    items: [],
    loaded: false,
    loading: false,
    loadingMore: false,
    hasMore: false,
    earliestSeq: 0,
    unread: 0,
    lastTs: "",
  };
}

// sessionKeyOf はアクティビティを安定したセッションキーへ振り分ける。worker="planner" は
// Goal Agent のラウンド 0 分解とプランナーの両方をカバーする（単一の Plan セッション）。
function sessionKeyOf(a: Activity): string {
  if (a.worker === "system" || a.kind === "llm_switch" || a.kind === "llm_failover") return "system";
  if (a.worker === "mainagent") return `main:${a.main_seg ?? 0}`; // 会話セグメントごとに 1 キー
  if (a.worker === "planner") return "plan";
  if (a.intent_id) return `intent:${a.intent_id}`;
  return "unknown";
}

// mergeBySeq は 2 つのアクティビティリストを seq で統合（重複排除）し、seq 昇順に並べる。
// 最新ページ、古いページ、SSE 補償、ライブテールのすべてがこれを経由するため、履歴応答が
// その間に受信したライブレコードを上書きすることはない。
function mergeBySeq(current: Activity[], incoming: Activity[]): Activity[] {
  if (!incoming.length) return current;
  const bySeq = new Map<number, Activity>();
  for (const a of current) bySeq.set(a.seq, a);
  for (const a of incoming) bySeq.set(a.seq, a);
  return [...bySeq.values()].sort((p, q) => p.seq - q.seq);
}

// statusIcon はセッション状態をアイコンに対応付ける。Worker の終端状態は区別して
// 色分けする: 完了（緑のチェック丸）/ キャンセル停止（琥珀の斜線丸）/ エラー（赤の×丸）/
// ステップ枯渇（紫）。running=青の回転、pending（待機中）=グレーの時計。
function statusIcon(status: SessionStatus) {
  switch (status) {
    case "running": // 実行中
      return <Loader2Icon className="size-3.5 animate-spin text-blue-500" />;
    case "paused":
      return <PauseIcon className="size-3.5 text-amber-500" />;
    case "pending": // 待機中（open の意図）
      return <ClockIcon className="size-3.5 text-muted-foreground" />;
    case "done": // 完了
      return <CircleCheckIcon className="size-3.5 text-emerald-500" />;
    case "stopped": // キャンセル/停止（プランナーにより終了）
      return <CircleSlashIcon className="size-3.5 text-amber-500" />;
    case "blocked": // エラー
      return <CircleXIcon className="size-3.5 text-red-500" />;
    case "exhausted": // ステップ枯渇（max_turns 到達）
      return <ZapOffIcon className="size-3.5 text-violet-500" />;
    case "deleted": // ユーザーによるソフト削除
      return <CircleSlashIcon className="size-3.5 text-muted-foreground" />;
  }
}

// fmtTokens はトークン数を簡潔に描画する（1234 → 1.2k、2_000_000 → 2M）。
function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`;
  if (n >= 1000) return `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}k`;
  return String(n);
}

const TokenMetrics = React.forwardRef<
  HTMLSpanElement,
  React.ComponentPropsWithoutRef<"span"> & {
    input: number;
    cache: number;
    output: number;
    labels?: "short" | "long";
  }
>(({ input, cache, output, labels = "short", className, ...props }, ref) => {
  const names = labels === "short" ? ["入", "キ", "出"] : ["入力", "キャッシュ", "出力"];
  const values = [input, cache, output];
  return (
    <span
      ref={ref}
      className={cn("inline-flex min-w-0 flex-wrap items-center gap-1 text-muted-foreground", className)}
      {...props}
    >
      {values.map((value, index) => (
        <React.Fragment key={names[index]}>
          <span>{names[index]}</span>
          <Badge variant="secondary" className="h-5 px-1.5 font-mono tabular-nums">
            {fmtTokens(value)}
          </Badge>
        </React.Fragment>
      ))}
    </span>
  );
});
TokenMetrics.displayName = "TokenMetrics";

// fmtDuration は経過ミリ秒を簡潔に描画する（90s → 1m30s）。
function fmtDuration(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m${String(s % 60).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  return `${h}h${String(m % 60).padStart(2, "0")}m`;
}

const roleMeta = {
  mainagent: { label: "メイン Agent", icon: UserIcon },
  planner: { label: "プランナー Planner", icon: BrainIcon },
  worker: { label: "Workers", icon: RadioIcon },
  system: { label: "システム監査", icon: HistoryIcon },
} as const;

// メイン Agent セッションはこのタブの対話エントリポイントであり、専用のバックエンド
// 「sessions」エンドポイントは存在しない。トランスクリプトがタスクのメイン Agent
// アクティビティストリーム（worker="mainagent"）になる、固定の UI 要素である。
// メイン Agent セッションはリセット可能な 1 つの会話セグメント。セグメント 0 が元の
// セッションで、「新規セッション」は追加のセグメント（seq 1,2,…）を作成する。これにより
// タスクのグラフ/資産/ゴールを共有したまま、Agent はクリーンなトランスクリプトで開始する。
// 各セグメントは切替可能な UI セッションで、書き込み可能なのは現在（最大）のものだけ。
const mainSessionId = (seg: number) => `s-main-${seg}`;
const mainSessionKey = (seg: number) => `main:${seg}`;
const mainSessionTitle = (seg: number) => `メイン Agent · セッション #${seg + 1}`;
const MAIN_ID = mainSessionId(0);
const MAIN_SESSION: Session = {
  id: MAIN_ID,
  role: "mainagent",
  title: mainSessionTitle(0),
  status: "running",
  live: true,
  last_activity: "",
  seg: 0,
};

// プランナーセッションもメイン Agent セッションと同様、専用のバックエンド「sessions」
// エンドポイントを持たない固定の UI 要素である。トランスクリプトはプランナーが出力する
// すべてのアクティビティステップ（worker === "planner"。Goal Agent のラウンド 0 分解も
// 含む。プランナーは意図を生成する側なので intent_id を持たない）。
const PLANNER_ID = "s-planner";
const PLANNER_SESSION: Session = {
  id: PLANNER_ID,
  role: "planner",
  title: "プランナー Planner · 状況判断",
  status: "running",
  live: true,
  last_activity: "",
};

// LLM プロバイダーの切替はエージェント出力ではなくタスク単位の監査イベントである。
// 履歴エンドポイントに「system」フィルターはないため、この固定セッションは汎用の
// 増分アクティビティエンドポイントを走査して埋め、その後タスク SSE で追跡する。
const SYSTEM_ID = "s-system";
const SYSTEM_SESSION: Session = {
  id: SYSTEM_ID,
  role: "system",
  title: "システムイベント · LLM フェイルオーバー",
  status: "done",
  live: false,
  last_activity: "",
};

// keyForSession は UI の Session → 安定したストアキー（main:<seg> | plan | intent:<id>）を返す。
function keyForSession(s: Session): string {
  if (s.role === "mainagent") return `main:${s.seg ?? 0}`;
  if (s.role === "planner") return "plan";
  if (s.role === "system") return "system";
  return `intent:${s.intent_id}`;
}

// 探索意図（TaskNode）の状態 → UI が描画するセッション状態にマップする。
function intentStatus(state: string): SessionStatus {
  switch (state) {
    case "done":
      return "done";
    case "blocked":
      return "blocked";
    case "exhausted":
      return "exhausted";
    case "stopped":
      return "stopped";
    case "paused":
      return "paused";
    case "open": // 待機中。実行中とは異なる
      return "pending";
    case "deleted": // ユーザーによるソフト削除
      return "deleted";
    default: // running
      return "running";
  }
}

// Worker セッションは実行中/オープンな意図から導出する。バックエンドに sessions
// エンドポイントはないため、意図（≒ Worker の処理単位）が最も近い実データ源である。
function intentToSession(n: TaskNode): Session {
  const label = (n.payload ?? "").trim();
  const state = intentStatus(n.state);
  return {
    id: n.id,
    role: "worker",
    title: label || `Intent ${n.id}`,
    status: state,
    live: !n.inherited && state === "running",
    last_activity: n.ts,
    intent_id: n.id,
    source_task_id: n.source_task_id,
    inherited: n.inherited,
  };
}

function SessionItem({
  s,
  active,
  displayTitle,
  hasPending,
  unread,
  onClick,
  onCancel,
  controlling,
  deleted,
}: {
  s: Session;
  active: boolean;
  displayTitle: string;
  hasPending?: boolean;
  unread?: number;
  onClick: () => void;
  onCancel?: () => void;
  controlling?: boolean;
  deleted?: boolean;
}) {
  const icon = deleted ? (
    <Trash2Icon className="size-3.5 text-destructive" />
  ) : s.role === "worker" ? (
    statusIcon(s.status)
  ) : s.live ? (
    <Loader2Icon className="size-3.5 animate-spin text-blue-500" />
  ) : null;

  const cancellable =
    s.role === "worker" &&
    !s.inherited &&
    !deleted &&
    // pending = 待機中（open）の意図。実行中/一時停止中も削除を許可する。
    (s.status === "running" || s.status === "paused" || s.status === "pending");
  return (
    <div
      className={cn(
        "group/session flex w-full items-center rounded-md transition-colors",
        active ? "bg-accent text-accent-foreground" : "hover:bg-accent/50",
      )}
    >
      <button
        type="button"
        onClick={onClick}
        className="flex min-w-0 flex-1 items-center gap-1 px-2 py-1.5 text-left text-sm"
      >
        {icon ?? <span className="size-3.5 shrink-0" />}
        {s.intent_id && (
          <span className="shrink-0 rounded bg-muted px-1 py-0.5 font-mono text-[10px] tabular-nums text-muted-foreground">
            #{s.intent_id}
          </span>
        )}
        {s.inherited && s.source_task_id && (
          <Badge variant="outline" className="shrink-0">
            ソース #{s.source_task_id}
          </Badge>
        )}
        <span
          className={cn("min-w-0 flex-1 truncate text-sm font-medium", deleted && "text-muted-foreground line-through")}
        >
          {displayTitle}
        </span>
        {deleted && (
          <Badge variant="outline" className="shrink-0 border-destructive/40 text-destructive">
            削除済み
          </Badge>
        )}
        {hasPending && <ShieldAlertIcon className="size-3.5 shrink-0 text-amber-500" />}
        {!active && unread ? (
          <span className="inline-flex min-w-4 items-center justify-center rounded-full bg-blue-500/15 px-1 text-[10px] font-medium tabular-nums text-blue-600 dark:text-blue-400">
            {unread > 99 ? "99+" : unread}
          </span>
        ) : null}
        {s.live && (
          <span className="inline-flex items-center gap-1 rounded bg-blue-500/15 px-1.5 py-0.5 text-[10px] font-medium text-blue-600 dark:text-blue-400">
            <span className="size-1 animate-pulse rounded-full bg-blue-500" />
            リアルタイム
          </span>
        )}
      </button>
      {cancellable && (
        <div className="flex shrink-0 items-center gap-0.5 pr-1 opacity-100 sm:opacity-0 sm:transition-opacity sm:group-hover/session:opacity-100 sm:group-focus-within/session:opacity-100">
          <Button
            type="button"
            variant="ghost"
            size="icon-xs"
            onClick={onCancel}
            disabled={controlling}
            title="この意図を削除（理由が必須。ソフト削除/完全削除を選択可能）"
            aria-label="この意図を削除（理由が必須。ソフト削除/完全削除を選択可能）"
            className="text-destructive hover:text-destructive"
          >
            <Trash2Icon />
          </Button>
        </div>
      )}
    </div>
  );
}

function truncateWorkerAssetLabel(value: string, maxChars = 30): string {
  const chars = Array.from(value.trim());
  if (chars.length <= maxChars) return chars.join("");
  return `${chars.slice(0, Math.max(0, maxChars - 1)).join("")}…`;
}

function WorkerAssetBadge({ assets }: { assets: IntentAsset[] }) {
  const displayAssets = assets.filter(
    (asset) => asset.type === "root_domain" || asset.type === "subdomain" || asset.type === "ip",
  );
  if (displayAssets.length === 0) return null;

  const first = displayAssets[0];
  const firstRawLabel = first.label.trim() || `#${first.asset_id}`;
  const firstLabel = truncateWorkerAssetLabel(firstRawLabel);
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge variant="outline" className="max-w-60 shrink-0 font-normal" title={firstRawLabel}>
          <span className="truncate">現在の資産：{firstLabel}</span>
          {displayAssets.length > 1 && <span className="shrink-0 tabular-nums">+{displayAssets.length - 1}</span>}
        </Badge>
      </TooltipTrigger>
      <TooltipContent side="right" align="start" className="max-w-sm">
        <div className="flex flex-col gap-2">
          {displayAssets.map((asset) => (
            <div key={`${asset.intent_id}-${asset.asset_id}`} className="min-w-0">
              <div className="break-all font-mono text-xs">{asset.label.trim() || `#${asset.asset_id}`}</div>
              <div className="mt-0.5 text-xs text-muted-foreground">
                {taskAssetTypeLabel(asset.type)} · {taskAssetSourceLabel(asset.source)}
                {asset.inherited ? ` · ソースタスク #${asset.source_task_id}` : ""}
              </div>
              <div className="mt-0.5 [overflow-wrap:anywhere] text-xs">{asset.source_summary}</div>
            </div>
          ))}
        </div>
      </TooltipContent>
    </Tooltip>
  );
}

export function SessionsTab({ taskId }: { taskId: string }) {
  const approvalFocus = useApprovalFocus({ taskId });
  const [selectedSessionId, setActiveId] = React.useState(MAIN_ID);
  const focusSession = React.useMemo<Session | undefined>(() => {
    const source = approvalFocus.state?.source;
    if (!source) return undefined;
    if (source.session.startsWith("main:")) {
      const seg = Number(source.session.slice(5));
      return { ...MAIN_SESSION, id: mainSessionId(seg), seg, title: mainSessionTitle(seg), live: false };
    }
    if (source.session === "plan") return { ...PLANNER_SESSION, live: false };
    if (source.session.startsWith("intent:")) {
      const id = source.session.slice(7);
      return {
        id,
        role: "worker",
        intent_id: id,
        title: `Worker #${id}`,
        status: "done",
        live: false,
        last_activity: source.items[0]?.ts ?? "",
      };
    }
    return undefined;
  }, [approvalFocus.state?.source]);
  // 配置済みのアーカイブ/過去セッションを、フォーカスモードを抜けた後も選択可能に保つ。
  const [locatedSession, setLocatedSession] = React.useState<{ taskId: string; session: Session }>();
  React.useEffect(() => {
    if (focusSession) setLocatedSession({ taskId, session: focusSession });
  }, [taskId, focusSession]);
  const retainedSession = locatedSession?.taskId === taskId ? locatedSession.session : undefined;
  const activeId = focusSession?.id ?? selectedSessionId;
  // メイン Agent の会話セグメント（新しい順）。currentSeg が書き込み可能なもの。
  const [mainSegs, setMainSegs] = React.useState<{ seq: number; created_at: string }[]>([{ seq: 0, created_at: "" }]);
  const [currentSeg, setCurrentSeg] = React.useState(0);
  const [creatingMain, setCreatingMain] = React.useState(false);
  const [confirmNewMain, setConfirmNewMain] = React.useState(false);
  // モバイル（<lg）ではセッション一覧を既定で折りたたむ。画面の高さが元々厳しく、
  // 一覧が固定で 10〜15rem を占めると、下のセッション記録がタイトルと入力欄だけに
  // 押し込まれてしまう。折りたたむと記録領域がほぼ全高を使え、タイトルバーをタップ
  // すれば展開してセッションを選べ、選択後に自動で折りたたまれる。デスクトップは
  // 影響を受けない（lg 以上は常に展開）。
  const [listOpen, setListOpen] = React.useState(false);
  // セッションごとの遅延読み込みキャッシュ。session_key（main | plan | intent:<id>）でキー付け。
  const [store, setStore] = React.useState<SessionStore>({});
  // 探索意図から導出した Worker セッション（旧 300 件上限を超えてページング）。
  const [intents, setIntents] = React.useState<TaskNode[]>([]);
  const [intentAssets, setIntentAssets] = React.useState<IntentAsset[]>([]);
  const [olderIntents, setOlderIntents] = React.useState<TaskNode[]>([]);
  const [firstIntentsHasMore, setFirstIntentsHasMore] = React.useState(false);
  const [olderIntentsHasMore, setOlderIntentsHasMore] = React.useState(false);
  const [hasLoadedOlderIntentsPage, setHasLoadedOlderIntentsPage] = React.useState(false);
  const [loadingOlderIntents, setLoadingOlderIntents] = React.useState(false);
  const [input, setInput] = React.useState("");
  const [sending, setSending] = React.useState(false);
  const [stopping, setStopping] = React.useState(false);
  const [mainChatRunning, setMainChatRunning] = React.useState<boolean | null>(null);
  const [controllingIntent, setControllingIntent] = React.useState<string | null>(null);
  const [cancelIntent, setCancelIntent] = React.useState<Session | null>(null);
  const [cancelReason, setCancelReason] = React.useState("");
  // 削除モード: soft=ソフト削除（既定。deleted に設定 + 理由を記録し、データは保持）|
  // hard=完全削除（独占する子孫をカスケード削除）。
  const [deleteMode, setDeleteMode] = React.useState<"soft" | "hard">("soft");
  const [workerMessage, setWorkerMessage] = React.useState("");
  const [workerMessageRequestId, setWorkerMessageRequestId] = React.useState("");
  const [workerMessageSending, setWorkerMessageSending] = React.useState(false);
  // 方法 1 のファイルアップロード:選択済みの添付（タスク作業ディレクトリの uploads/ に
  // 保存済み）を、次のメッセージと一緒に送信する。
  const [attachments, setAttachments] = React.useState<ChatAttachment[]>([]);
  const [uploading, setUploading] = React.useState(false);
  const fileInputRef = React.useRef<HTMLInputElement>(null);

  async function pickFiles(files: FileList | null) {
    if (!files || files.length === 0) return;
    setUploading(true);
    try {
      const r = await api.chatUpload("task", taskId, Array.from(files));
      setAttachments((prev) => [...prev, ...r.attachments]);
    } catch (e) {
      toast.error(`アップロードに失敗しました：${(e as Error).message}`);
    } finally {
      setUploading(false);
      if (fileInputRef.current) fileInputRef.current.value = "";
    }
  }

  // メイン Agent の新しいセッションを開始する。進むのはセグメント番号だけで、タスクの
  // グラフ/資産/ゴールは変わらないため、Agent は同じタスク上をクリーンなコンテキストで
  // 継続する。旧セグメントは読み取り専用の履歴として残り、いつでも切り替えられる。
  async function createMainSession() {
    if (creatingMain) return;
    setCreatingMain(true);
    try {
      const r = await api.newMainSession(taskId);
      setMainSegs((prev) => [{ seq: r.seq, created_at: r.created_at }, ...prev.filter((m) => m.seq !== r.seq)]);
      setCurrentSeg(r.current ?? r.seq);
      // 空の loaded 状態を仕込んで、新しい（空の）セッションを即座に描画できるようにする。
      setStore((prev) => ({ ...prev, [mainSessionKey(r.seq)]: { ...emptyState(), loaded: true } }));
      setActiveId(mainSessionId(r.seq));
      setListOpen(false);
      setInput("");
    } catch (e) {
      toast.error(`新規セッションの作成に失敗しました：${(e as Error).message}`);
    } finally {
      setCreatingMain(false);
      setConfirmNewMain(false);
    }
  }

  const patchIntentState = React.useCallback((intentId: string, state?: string) => {
    const patch = (rows: TaskNode[]) =>
      state
        ? rows.map((row) => (row.id === intentId ? { ...row, state } : row))
        : rows.filter((row) => row.id !== intentId);
    setIntents(patch);
    setOlderIntents(patch);
  }, []);

  const controlWorker = React.useCallback(
    async (session: Session, action: "pause" | "resume" | "cancel", reason?: string, mode?: "soft" | "hard") => {
      if (!session.intent_id || session.inherited || controllingIntent) return;
      if (action === "cancel" && !reason?.trim()) {
        toast.error("削除理由を入力してください");
        return;
      }
      setControllingIntent(session.intent_id);
      try {
        const res = await api.controlIntent(taskId, session.intent_id, action, reason, mode);
        if (action === "pause") {
          patchIntentState(session.intent_id, "paused");
          toast.success(`Worker #${session.intent_id} を一時停止しました`);
        } else if (action === "resume") {
          patchIntentState(session.intent_id, "open");
          toast.success(`Worker #${session.intent_id} を再開しました。再取得を待っています`);
        } else if (mode === "hard") {
          // 完全削除:意図と独占する下流が物理削除されるため、一覧から行を除去する。
          patchIntentState(session.intent_id);
          const d = res.deleted;
          const extra = d ? `（意図 ${d.intents} / 事実 ${d.facts} / 脆弱性 ${d.findings} を含む）` : "";
          toast.success(`Worker #${session.intent_id} とその独占下流を完全に削除しました${extra}`);
          setCancelReason("");
        } else {
          // ソフト削除:意図を deleted にし削除理由を記録。ノードと成果は保持する。
          patchIntentState(session.intent_id, "deleted");
          toast.success(
            `Worker #${session.intent_id} を削除しました（理由を記録済み。プランナーがこれに基づいて再計画します）`,
          );
          setCancelReason("");
        }
      } catch (error) {
        toast.error(`Worker の操作に失敗しました：${(error as Error).message}`);
      } finally {
        setControllingIntent(null);
        setCancelIntent(null);
      }
    },
    [controllingIntent, patchIntentState, taskId],
  );

  // SSE 接続状態。リアルタイムリンクの切断を可視化し、「メッセージなし」と
  // 誤って表示されないようにする。
  const [sseLive, setSseLive] = React.useState(false);
  // タスク全体のトークン合計（全エージェント）。バックエンド集計値からポーリングする。
  const [taskTokens, setTaskTokens] = React.useState<TokenTotal | null>(null);
  const [sessionTokens, setSessionTokens] = React.useState<Record<string, SessionTokenUsage>>({});
  const [llmResolutions, setLLMResolutions] = React.useState<TaskLLMResolutions | null>(null);
  // このタスクの保留中のインターセプト要求 — セッションに警告アイコンを出すために使う。
  const [pendingIntercepts, setPendingIntercepts] = React.useState<InterceptApprovalRow[]>([]);

  // 再レンダリングを誘発せずに SSE/読み込みを支える ref 群。
  const snapshotRef = React.useRef(0); // タスク単位のスナップショットカーソル → SSE since=
  const esRef = React.useRef<EventSource | null>(null);
  const activeKeyRef = React.useRef(mainSessionKey(0)); // 現在のセッションキー（SSE 振り分け/未読用）
  const atBottomRef = React.useRef(true); // トランスクリプトが最下部に固定されているか
  const llmToastSeqRef = React.useRef<Set<number>>(new Set());
  const chatStatusRequestRef = React.useRef(0);
  const firstIntentsRef = React.useRef<TaskNode[]>([]);
  // キーごとのリクエストトークン:あるキーの古い応答は無視する（最新/古いページの高速な
  // 交互読み込みを保護）。書き込みは常にキー付きなので、遅延応答は自分のセッション
  // キャッシュにしか触れず、現在表示中のものには決して影響しない（§7.5 参照）。
  const reqTokenRef = React.useRef<Record<string, number>>({});
  // 最新ページの読み込みが進行中のキー。初回読み込みエフェクトとアクティブセッション
  // エフェクトがマウント時に同時に「main」を要求する二重トリガーを排除する（前者は
  // SSE も開くため、先を越されてはならない）。
  const loadingKeysRef = React.useRef<Set<string>>(new Set());

  // ── ストアヘルパー ────────────────────────────────────────────────────────────
  const patchStore = React.useCallback((key: string, fn: (s: SessionState) => SessionState) => {
    setStore((prev) => ({ ...prev, [key]: fn(prev[key] ?? emptyState()) }));
  }, []);

  // 初回オープン時にセッションの最新ページ（before=0）を読み込む。キー付き +
  // リクエストトークンで保護するため、切替中でも表示が壊れない。
  const loadSession = React.useCallback(
    (key: string) => {
      if (MOCK) return; // MOCK は全データを事前に読み込む
      if (loadingKeysRef.current.has(key)) return; // 既に進行中（例:マウント時の main）
      loadingKeysRef.current.add(key);
      const token = (reqTokenRef.current[key] ?? 0) + 1;
      reqTokenRef.current[key] = token;
      patchStore(key, (s) => ({ ...s, loading: true, error: undefined }));

      if (key === "system") {
        void (async () => {
          let since = 0;
          let systemItems: Activity[] = [];
          for (;;) {
            const page = await api.activity(taskId, { since, limit: SYSTEM_SCAN_PAGE });
            if (reqTokenRef.current[key] !== token) return;
            systemItems = mergeBySeq(
              systemItems,
              page.items.filter(
                (item) => item.worker === "system" || item.kind === "llm_switch" || item.kind === "llm_failover",
              ),
            );
            if (page.items.length < SYSTEM_SCAN_PAGE || page.cursor <= since) break;
            since = page.cursor;
          }
          patchStore(key, (s) => {
            const items = mergeBySeq(systemItems, s.items);
            return {
              ...s,
              items,
              loaded: true,
              loading: false,
              hasMore: false,
              earliestSeq: items.length ? items[0].seq : 0,
              unread: 0,
              lastTs: items.length ? items[items.length - 1].ts : s.lastTs,
              error: undefined,
            };
          });
        })()
          .catch((error) => {
            if (reqTokenRef.current[key] !== token) return;
            patchStore(key, (s) => ({
              ...s,
              loading: false,
              error: (error as Error).message || "読み込みに失敗しました",
            }));
          })
          .finally(() => loadingKeysRef.current.delete(key));
        return;
      }

      api
        .activityHistory(taskId, key, 0, PAGE)
        .then((r) => {
          if (reqTokenRef.current[key] !== token) return; // 上書きされた
          if (r.snapshotCursor > snapshotRef.current) snapshotRef.current = r.snapshotCursor;
          patchStore(key, (s) => {
            const items = mergeBySeq(r.items, s.items); // その間に届いたライブフレームを保持
            return {
              ...s,
              items,
              loaded: true,
              loading: false,
              hasMore: r.hasMore,
              earliestSeq: items.length ? items[0].seq : 0,
              unread: 0,
              error: undefined,
            };
          });
        })
        .catch((e) => {
          if (reqTokenRef.current[key] !== token) return;
          patchStore(key, (s) => ({ ...s, loading: false, error: (e as Error).message || "読み込みに失敗しました" }));
        })
        .finally(() => loadingKeysRef.current.delete(key));
    },
    [taskId, patchStore],
  );

  // セッションの古いページを 1 つ読み込み（上スクロール）、スクロール位置を保持する。
  const loadEarlier = React.useCallback(
    (key: string, viewport: () => HTMLElement | null) => {
      const st = store[key];
      if (!st || st.loadingMore || !st.hasMore || !st.earliestSeq) return;
      const vp = viewport();
      const prevH = vp?.scrollHeight ?? 0;
      const prevTop = vp?.scrollTop ?? 0;
      patchStore(key, (s) => ({ ...s, loadingMore: true }));
      api
        .activityHistory(taskId, key, st.earliestSeq, PAGE)
        .then((r) => {
          patchStore(key, (s) => {
            const items = mergeBySeq(r.items, s.items);
            return {
              ...s,
              items,
              loadingMore: false,
              hasMore: r.hasMore,
              earliestSeq: items.length ? items[0].seq : s.earliestSeq,
            };
          });
          requestAnimationFrame(() => {
            const v = viewport();
            if (v) v.scrollTop = prevTop + (v.scrollHeight - prevH);
          });
        })
        .catch(() => {
          patchStore(key, (s) => ({ ...s, loadingMore: false }));
        });
    },
    [taskId, store, patchStore],
  );

  // ── タスクのトークン合計（タスク全体、全エージェント） ──────────────────────────
  React.useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .tokenStats(taskId)
        .then((r) => {
          if (!alive) return;
          setTaskTokens(r.total);
          setSessionTokens(Object.fromEntries(r.sessions.map((item) => [item.session, item])));
        })
        .catch(() => {
          // ポーリングはベストエフォート。次のインターバルで自動的に再試行する。
        });
    void load();
    const t = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [taskId]);

  React.useEffect(() => {
    let alive = true;
    setMainChatRunning(null);
    const load = () => {
      const request = ++chatStatusRequestRef.current;
      return api
        .chatStatus(taskId)
        .then(({ running }) => {
          if (alive && chatStatusRequestRef.current === request) setMainChatRunning(running);
        })
        .catch(() => {
          // 最後に取得できた信頼できる値を保持する。初回成功前は、直近のアクティビティが
          // 保守的なフォールバックとして機能する。
        });
    };
    void load();
    const timer = setInterval(load, 2000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [taskId]);

  React.useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .taskLLMResolution(taskId)
        .then((value) => {
          if (alive) setLLMResolutions(value);
        })
        .catch(() => {
          // ポーリングはベストエフォート。次のインターバルで自動的に再試行する。
        });
    void load();
    const timer = setInterval(load, 10_000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [taskId]);

  React.useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .interceptTask(taskId)
        .then((rows) => {
          if (alive) setPendingIntercepts(rows.filter((r) => r.status === "pending"));
        })
        .catch(() => {
          // ポーリングはベストエフォート。次のインターバルで自動的に再試行する。
        });
    void load();
    const t = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [taskId]);

  // アクティビティが古くなるにつれて「ストリーミング中」の判定を再計算できるよう、
  // タイマーで再レンダリングする。
  const [, setTick] = React.useState(0);
  React.useEffect(() => {
    const id = setInterval(() => setTick((t) => t + 1), 1500);
    return () => clearInterval(id);
  }, []);

  // ── 初回読み込み + 単一タスク SSE ─────────────────────────────────────────────
  // タスクを開いたら、まず Main の最新ページを読み込み、そのスナップショットカーソルを
  // 取得してから since=snapshot_cursor で 1 本のタスク SSE を開く。履歴が id≤cursor を、
  // SSE が id>cursor を隙間なくカバーする。SSE は全エージェントを追跡し、フレームは
  // session_key で振り分ける。EventSource は自動再接続し（`id:` 行 → Last-Event-ID）、
  // DB から再開するため、リアルタイムリンクが切れても自己修復する。seq マージで重複排除。
  React.useEffect(() => {
    setStore({});
    setIntents([]);
    setIntentAssets([]);
    setOlderIntents([]);
    setFirstIntentsHasMore(false);
    setOlderIntentsHasMore(false);
    setHasLoadedOlderIntentsPage(false);
    setTaskTokens(null);
    setSessionTokens({});
    setLLMResolutions(null);
    setSseLive(false);
    setActiveId(MAIN_ID); // 前のタスクの古い worker id が漏れないようにする
    setMainSegs([{ seq: 0, created_at: "" }]);
    setCurrentSeg(0);
    snapshotRef.current = 0;
    llmToastSeqRef.current = new Set();
    reqTokenRef.current = {};
    // 初期の main キーを予約し、アクティブセッションエフェクト（マウント時に main で
    // 発火する）が二重読み込みして、ここで開く SSE の先を越さないようにする。
    loadingKeysRef.current = new Set([mainSessionKey(0)]);
    let alive = true;

    // MOCK デモ:SSE バックエンドがないため、アクティビティを 1 回取得してセッション別に振り分ける。
    if (MOCK) {
      api
        .activity(taskId)
        .then((r) => {
          if (!alive) return;
          const buckets: SessionStore = {};
          for (const a of r.items) {
            const k = sessionKeyOf(a);
            if (!buckets[k]) buckets[k] = emptyState();
            buckets[k].items.push(a);
          }
          for (const k of Object.keys(buckets)) {
            const st = buckets[k];
            st.items.sort((p, q) => p.seq - q.seq);
            st.loaded = true;
            st.hasMore = false;
            st.earliestSeq = st.items.length ? st.items[0].seq : 0;
            st.lastTs = st.items.length ? st.items[st.items.length - 1].ts : "";
          }
          buckets[mainSessionKey(0)] ??= { ...emptyState(), loaded: true };
          buckets.system ??= { ...emptyState(), loaded: true };
          setStore(buckets);
        })
        .catch(() =>
          setStore({
            [mainSessionKey(0)]: { ...emptyState(), loaded: true },
            system: { ...emptyState(), loaded: true },
          }),
        );
      return () => {
        alive = false;
      };
    }

    const token = (reqTokenRef.current.mainboot ?? 0) + 1;
    reqTokenRef.current.mainboot = token;
    let bootKey = mainSessionKey(0);
    // まずメイン Agent のセグメントを解決し、現在のセグメントの履歴を読み込んで、その
    // スナップショットカーソルから SSE を開く。SSE は全セグメントを追跡し、各フレームを
    // session_key（main:<seg>）で振り分けるため、セグメント切替に新しいストリームは不要。
    api
      .mainSessions(taskId)
      .then((ms) => {
        if (!alive || reqTokenRef.current.mainboot !== token) throw new Error("superseded");
        const segs = ms.sessions.length ? ms.sessions : [{ seq: 0, created_at: "" }];
        setMainSegs(segs);
        setCurrentSeg(ms.current);
        bootKey = mainSessionKey(ms.current);
        if (bootKey !== mainSessionKey(0)) loadingKeysRef.current.delete(mainSessionKey(0));
        loadingKeysRef.current.add(bootKey);
        setActiveId(mainSessionId(ms.current));
        patchStore(bootKey, (s) => ({ ...s, loading: true }));
        return api.activityHistory(taskId, bootKey, 0, PAGE);
      })
      .then((r) => {
        if (!alive || reqTokenRef.current.mainboot !== token) return;
        snapshotRef.current = r.snapshotCursor;
        patchStore(bootKey, (s) => {
          const items = mergeBySeq(r.items, s.items);
          return {
            ...s,
            items,
            loaded: true,
            loading: false,
            hasMore: r.hasMore,
            earliestSeq: items.length ? items[0].seq : 0,
            unread: 0,
          };
        });
        // スナップショットカーソルから単一のタスク SSE を開く。
        const es = new EventSource(
          sseUrl(`/api/exploration/activity/stream?task=${encodeURIComponent(taskId)}&since=${snapshotRef.current}`),
        );
        esRef.current = es;
        es.onopen = () => setSseLive(true);
        es.onerror = () => setSseLive(false); // EventSource は自動再接続し、DB が隙間を補償する
        es.onmessage = (e) => {
          let a: Activity;
          try {
            a = JSON.parse(e.data) as Activity;
          } catch {
            return; // 不正なフレームは無視
          }
          if ((a.kind === "llm_switch" || a.kind === "llm_failover") && !llmToastSeqRef.current.has(a.seq)) {
            llmToastSeqRef.current.add(a.seq);
            const transition = a.metadata?.llm_transition;
            if (transition?.mode === "exhausted" || a.is_error) {
              toast.error(a.summary, { id: `task-${taskId}-llm-${a.seq}` });
            } else if (transition?.mode === "automatic") {
              toast.success(a.summary, { id: `task-${taskId}-llm-${a.seq}` });
            } else {
              toast.info(a.summary, { id: `task-${taskId}-llm-${a.seq}` });
            }
            void api
              .taskLLMResolution(taskId)
              .then((value) => {
                if (alive) setLLMResolutions(value);
              })
              .catch(() => {
                // このイベント起点のリフレッシュが失敗しても、定期的なリゾルバーポーリングが再試行する。
              });
          }
          const k = sessionKeyOf(a);
          setStore((prev) => {
            const cur = prev[k] ?? emptyState();
            const activeK = activeKeyRef.current;
            // 読み込み済み・読み込み中・現在表示中のセッションにマージする（戻ったとき即座に
            // 表示でき、読み込み中に届いたフレームも失わない）。コールドで非アクティブな
            // セッションも、サイドバーが履歴を読み込まずに最新の未完了使用量を加算できるよう
            // 小さな会計用テールだけ保持する。
            if (!cur.loaded && !cur.loading && k !== activeK) {
              const accountingItems =
                a.kind === "usage" || a.kind === "result" ? mergeBySeq(cur.items, [a]).slice(-4) : cur.items;
              return {
                ...prev,
                [k]: { ...cur, items: accountingItems, lastTs: a.ts, unread: cur.unread + 1 },
              };
            }
            let items = mergeBySeq(cur.items, [a]);
            // メモリ上限:上限を超えたら最古を削る（古いものは上スクロール時に再取得）が、
            // ユーザーがこのセッションの履歴を読んでいる間（上へスクロール中）は削らない。
            let hasMore = cur.hasMore;
            let earliestSeq = cur.earliestSeq;
            const trimmable = k !== activeK || atBottomRef.current;
            if (trimmable && items.length > MAX_KEEP) {
              items = items.slice(items.length - MAX_KEEP);
              hasMore = true;
              earliestSeq = items[0].seq;
            }
            const unread = k === activeK ? 0 : cur.unread + 1;
            return { ...prev, [k]: { ...cur, items, lastTs: a.ts, unread, hasMore, earliestSeq } };
          });
        };
      })
      .catch((err) => {
        if (!alive || reqTokenRef.current.mainboot !== token) return;
        if ((err as Error).message === "superseded") return;
        patchStore(bootKey, (s) => ({
          ...s,
          loading: false,
          error: (err as Error).message || "読み込みに失敗しました",
        }));
      })
      .finally(() => loadingKeysRef.current.delete(bootKey));

    return () => {
      alive = false;
      esRef.current?.close();
      esRef.current = null;
    };
  }, [taskId, patchStore]);

  React.useEffect(() => {
    let active = true;
    const load = () =>
      api
        .taskIntentAssets(taskId)
        .then((assets) => {
          if (active) setIntentAssets(assets);
        })
        .catch(() => {
          // 次のポーリングが再試行する。Worker の操作とトランスクリプトは引き続き利用可能。
        });
    void load();
    const timer = setInterval(load, 5000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [taskId]);

  // ── Worker（意図）セッション一覧 — ページングし、先頭ページのみ軽くポーリング ─────
  React.useEffect(() => {
    let active = true;
    firstIntentsRef.current = [];
    setIntents([]);
    setOlderIntents([]);
    setFirstIntentsHasMore(false);
    setOlderIntentsHasMore(false);
    const load = () =>
      api
        .intentsPage(taskId, 0, 300)
        .then((r) => {
          if (!active) return;
          const freshIds = new Set(r.items.map((item) => item.id));
          const oldestFreshId = r.items.reduce((minimum, item) => Math.min(minimum, Number(item.id)), Infinity);
          const displaced = r.hasMore
            ? firstIntentsRef.current.filter((item) => !freshIds.has(item.id) && Number(item.id) < oldestFreshId)
            : [];
          if (displaced.length > 0) {
            setOlderIntents((previous) => {
              const byId = new Map(previous.map((item) => [item.id, item]));
              for (const item of displaced) byId.set(item.id, item);
              return [...byId.values()];
            });
          }
          firstIntentsRef.current = r.items;
          setIntents(r.items);
          setFirstIntentsHasMore(r.hasMore);
        })
        .catch(() => {
          // ポーリングはベストエフォート。次のインターバルで自動的に再試行する。
        });
    void load();
    const t = setInterval(load, 5000);
    return () => {
      active = false;
      clearInterval(t);
    };
  }, [taskId]);

  const loadOlderIntents = React.useCallback(() => {
    if (loadingOlderIntents) return;
    const all = [...intents, ...olderIntents];
    const minId = all.reduce((m, n) => Math.min(m, Number(n.id)), Infinity);
    if (!Number.isFinite(minId)) return;
    setLoadingOlderIntents(true);
    api
      .intentsPage(taskId, minId, 300)
      .then((r) => {
        setOlderIntents((prev) => {
          const seen = new Set([...intents, ...prev].map((n) => n.id));
          return [...prev, ...r.items.filter((n) => !seen.has(n.id))];
        });
        setOlderIntentsHasMore(r.hasMore);
        setHasLoadedOlderIntentsPage(true);
      })
      .catch(() => {
        // 後で手動で再試行すれば、このページをもう一度取得できる。
      })
      .finally(() => setLoadingOlderIntents(false));
  }, [taskId, intents, olderIntents, loadingOlderIntents]);

  // 統合・重複排除済みの Worker 一覧（最新の先頭ページ + 読み込み済みの古いページ）。
  const allIntents = React.useMemo(() => {
    const byId = new Map<string, TaskNode>();
    for (const n of olderIntents) byId.set(n.id, n);
    for (const n of intents) byId.set(n.id, n); // 新しいポーリングが古いスナップショットより優先
    return [...byId.values()].sort((a, b) => Number(b.id) - Number(a.id));
  }, [intents, olderIntents]);
  const intentAssetsByID = React.useMemo(() => {
    const grouped = new Map<string, IntentAsset[]>();
    for (const asset of intentAssets) {
      const key = String(asset.intent_id);
      const current = grouped.get(key);
      if (current) current.push(asset);
      else grouped.set(key, [asset]);
    }
    return grouped;
  }, [intentAssets]);

  const workerSessions = React.useMemo(() => allIntents.map(intentToSession), [allIntents]);
  const intentsHasMore = hasLoadedOlderIntentsPage ? olderIntentsHasMore : firstIntentsHasMore;

  // 各 Worker セッション（意図）の表示タイトルを、意図の payload 要約から導出する。
  // ホバー時の JSON ツールチップ用に、完全な TaskNode も保持する。
  const sessionMeta = React.useMemo(() => {
    const map = new Map<string, { title: string; json: unknown; deleted: boolean; deleteReason: string }>();
    for (const node of allIntents) {
      let title = `Intent ${node.id}`;
      let parsedPayload: unknown = node.payload;
      // ソフト削除:意図の state='deleted'。削除理由は独立フィールド delete_reason にある。
      const deleted = node.state === "deleted";
      const deleteReason = node.delete_reason ?? "";
      if (node.payload) {
        try {
          const p = JSON.parse(node.payload);
          parsedPayload = p;
          if (p?.summary) title = String(p.summary);
        } catch {
          title = node.payload.trim() || title;
        }
      }
      const json = { ...node, payload: parsedPayload };
      map.set(node.id, { title, json, deleted, deleteReason });
    }
    return map;
  }, [allIntents]);

  // このセッションに属する保留中のインターセプトがあれば true を返す。
  // Worker の agent_name 形式: "work#N · #intentID"。Main/planner はロールキーで照合する。
  const hasPendingForSession = React.useCallback(
    (s: Session): boolean => {
      if (s.inherited) return false;
      if (!pendingIntercepts.length) return false;
      if (s.role === "mainagent") return pendingIntercepts.some((r) => r.agent_name === "mainagent");
      if (s.role === "planner") return pendingIntercepts.some((r) => r.agent_name === "planner");
      // Worker: "work#N · #<intentID>" から意図 ID を抽出する
      return pendingIntercepts.some((r) => {
        const m = r.agent_name.match(/·\s*#(\d+)$/);
        return m ? m[1] === s.intent_id : false;
      });
    },
    [pendingIntercepts],
  );

  // 各セッションの最終アクティビティ時刻によるライブ判定（1.5 秒の tick で最新に保たれる）。
  const recentLive = React.useCallback(
    (key: string) => {
      const ts = store[key]?.lastTs;
      if (!ts) return false;
      const t = Date.parse(ts);
      return t > 0 && Date.now() - t < STREAM_WINDOW_MS;
    },
    [store],
  );
  const currentMainKey = mainSessionKey(currentSeg);
  const plannerLive = recentLive("plan");

  // いまストリーミング中のメインセグメントはどれか。メインのターンはタスク単位で直列化
  // されるため、ライブなのは最大 1 セグメント。判定は「直近のアクティビティ」ではなく実際の
  // 実行フラグ（sending / mainChatRunning）で行い、ターンの終端レコード（kind='result' または
  // エラー）が届いた瞬間に落とす。そうしないと Agent が既に完了しているのにバッジが
  // STREAM_WINDOW_MS の間残ってしまう。null = 何も実行中でない。
  const liveMainSeg = React.useMemo<number | null>(() => {
    if (!(sending || mainChatRunning)) return null;
    // ストリーミング中のセグメントは最も新しいアクティビティを持つもの（送信直後のターンを含む）
    let seg = currentSeg;
    let bestTs = -1;
    for (const m of mainSegs) {
      const raw = store[mainSessionKey(m.seq)]?.lastTs;
      const ts = raw ? Date.parse(raw) : -1;
      if (ts > bestTs) {
        bestTs = ts;
        seg = m.seq;
      }
    }
    const items = store[mainSessionKey(seg)]?.items ?? [];
    const last = items[items.length - 1];
    if (last && (last.kind === "result" || (last.kind === "text" && last.is_error))) return null;
    return seg;
  }, [sending, mainChatRunning, mainSegs, store, currentSeg]);

  // メイン Agent の各セグメントは独立した対話セッションである（トップレベルのチャット
  // 会話と同様）。どのセグメントでも発言でき、表示は新しい順になる。
  const mainSessions = React.useMemo<Session[]>(
    () =>
      mainSegs.map((m) => ({
        id: mainSessionId(m.seq),
        role: "mainagent",
        title: mainSessionTitle(m.seq),
        status: "running",
        live: m.seq === liveMainSeg,
        last_activity: m.created_at,
        seg: m.seq,
      })),
    [mainSegs, liveMainSeg],
  );

  const sessions = React.useMemo(() => {
    const items = [...mainSessions, { ...PLANNER_SESSION, live: plannerLive }, ...workerSessions, SYSTEM_SESSION];
    const located = focusSession ?? retainedSession;
    if (located && !items.some((s) => s.id === located.id)) items.push(located);
    return items;
  }, [mainSessions, workerSessions, plannerLive, focusSession, retainedSession]);

  const grouped = {
    mainagent: sessions.filter((s) => s.role === "mainagent"),
    planner: sessions.filter((s) => s.role === "planner"),
    worker: sessions.filter((s) => s.role === "worker"),
    system: sessions.filter((s) => s.role === "system"),
  };

  const active = sessions.find((s) => s.id === activeId) ?? MAIN_SESSION;
  const side = useSideQuestions(
    active.role === "mainagent"
      ? `/api/tasks/${taskId}/chat`
      : active.role === "worker" && !active.inherited && active.intent_id
        ? `/api/tasks/${taskId}/intents/${active.intent_id}`
        : null,
  );
  const isMain = active.role === "mainagent";
  const isPlanner = active.role === "planner";
  const isSystem = active.role === "system";
  const activeKey = keyForSession(active);
  const activeState = store[activeKey];
  // メインのターンはタスク単位で直列化される（チャットロック）ため、「ビジー」はタスク全体に
  // 及ぶ。どのメインセグメントがターン中でも、アクティブな入力欄は無効になる。アクティブ
  // セッションの終端レコード（kind='result' またはエラー）が届いた瞬間に解除し、次の
  // chat-status ポーリングを待たずに入力欄をすぐ元に戻す。
  const activeItems = activeState?.items ?? [];
  const activeLast = activeItems[activeItems.length - 1];
  const activeSettled =
    !!activeLast && (activeLast.kind === "result" || (activeLast.kind === "text" && activeLast.is_error));
  const mainBusy = isMain && (sending || (!activeSettled && (mainChatRunning ?? recentLive(activeKey))));
  // 折りたたみ時（モバイル）のタイトルバーは一覧全体の代わりになる。現在のセッション名と
  // 他のセッションの未読合計を表示する。そうしないと、畳んだ後にどのセッションを見ている
  // のか、他のどこかに新着があるのかが分からなくなる。
  const activeDisplayTitle = (active.role === "worker" ? sessionMeta.get(active.id)?.title : "") || active.title;
  const hiddenUnread = React.useMemo(
    () => Object.entries(store).reduce((sum, [key, s]) => (key === activeKey ? sum : sum + s.unread), 0),
    [store, activeKey],
  );

  // SSE ディスパッチャーが把握するアクティブセッションを最新に保ち、アクティブセッションが
  // 変わるたびに遅延読み込みと未読クリアを行う。
  // biome-ignore lint/correctness/useExhaustiveDependencies: キャッシュ更新で現在のセッションを再アクティブ化してはならない。
  React.useEffect(() => {
    activeKeyRef.current = activeKey;
    const st = store[activeKey];
    if (!st || (!st.loaded && !st.loading)) {
      loadSession(activeKey);
    } else if (st.unread) {
      patchStore(activeKey, (s) => ({ ...s, unread: 0 }));
    }
  }, [activeKey]);

  const focusKey = approvalFocus.state?.source?.session;
  const loadFocusPage = React.useCallback(
    (before: number) => api.activityHistory(taskId, focusKey ?? "main", before, PAGE),
    [taskId, focusKey],
  );
  const mergeFocusPage = React.useCallback(
    (page: { items: Activity[]; hasMore: boolean }) => {
      if (!focusKey) return;
      patchStore(focusKey, (s) => {
        const items = mergeBySeq(page.items, s.items);
        return { ...s, items, hasMore: page.hasMore, earliestSeq: items[0]?.seq ?? s.earliestSeq };
      });
    },
    [focusKey, patchStore],
  );
  const focusHistory = useApprovalHistory(
    approvalFocus.state?.source,
    !!focusKey && !!store[focusKey]?.loaded,
    focusKey ? (store[focusKey]?.items ?? []) : [],
    loadFocusPage,
    mergeFocusPage,
  );
  React.useEffect(() => {
    if (approvalFocus.state) atBottomRef.current = false;
  }, [approvalFocus.state]);

  // メイン Agent は人間↔オーケストレーターのコンソールである。表示するのは会話のみ
  // （ユーザーのメッセージ + メイン Agent 自身の応答/ステップ）。プランナーセッションは
  // プランナーのステップを表示し、Worker セッションはその意図のアクティビティのみを、
  // 意図の目的を先頭にして表示する。
  const activity = React.useMemo(() => {
    const items = activeState?.items ?? [];
    // メイン Agent コンソールはサーバーデータのみから描画する。人間のターンはバックエンドが
    // 返す前に永続化 + ブロードキャストするため、他のあらゆる Agent ステップと同じ SSE
    // ストリーム（worker="mainagent"）で届く。クライアント側の楽観的エコーはなく、実際の
    // DB id と衝突しうる偽の seq も生成しない。
    if (isMain) return items;
    if (isPlanner || isSystem) return items;
    // Worker セッション:意図を右寄せの「ユーザー」風メッセージとしてトランスクリプトの先頭に
    // 置き（この Worker に渡されたタスク）、その後に実行ステップを続ける。
    const intentTitle = sessionMeta.get(active.id)?.title ?? active.title;
    const intentMsg: Activity = {
      seq: -1, // 実際のステップより前に並ぶ（実 seq は ≥ 0）
      worker: items[0]?.worker ?? active.id, // 同じレーンを再利用して Worker チップを出さない
      ts: active.last_activity || "",
      kind: "intent", // LLM 生成の目的 — 人間でないことが分かる専用バブルとして描画
      summary: intentTitle,
      source_task_id: active.source_task_id,
      inherited: active.inherited,
    };
    return [intentMsg, ...items];
  }, [
    isMain,
    isPlanner,
    isSystem,
    activeState,
    active.id,
    active.title,
    active.last_activity,
    active.source_task_id,
    active.inherited,
    sessionMeta,
  ]);

  // このセッションで最新の TodoWrite 呼び出しの seq — Todo ポップオーバー用。
  const latestTodoSeq = React.useMemo(() => {
    for (let i = activity.length - 1; i >= 0; i--) {
      const a = activity[i];
      if (a.kind === "tool_use" && a.tool === "TodoWrite") return a.seq;
    }
    return null;
  }, [activity]);

  // 確定済みの結果合計はバックエンドの完全なアクティビティ履歴から取得する。クライアント側で
  // 加算するのは最新の未完了 usage フレームのみ。これにより、部分的な履歴ページでも完了済み
  // 実行を過少に数えたり、実行中のものを二重計上したりしない。
  const tokenForSession = React.useCallback(
    (key: string): TokenTotal => {
      const saved = sessionTokens[key];
      const total: TokenTotal = {
        input_tokens: saved?.input_tokens ?? 0,
        output_tokens: saved?.output_tokens ?? 0,
        cache_read_tokens: saved?.cache_read_tokens ?? 0,
        cache_write_tokens: saved?.cache_write_tokens ?? 0,
      };
      const items = store[key]?.items ?? [];
      for (let index = items.length - 1; index >= 0; index--) {
        const item = items[index];
        if (item.kind === "result") break;
        if (item.kind !== "usage") continue;
        total.input_tokens += item.input_tokens ?? 0;
        total.output_tokens += item.output_tokens ?? 0;
        total.cache_read_tokens += item.cache_read_tokens ?? 0;
        total.cache_write_tokens += item.cache_write_tokens ?? 0;
        break;
      }
      return total;
    },
    [sessionTokens, store],
  );

  const activeTokens = tokenForSession(activeKey);
  const tokenTotal = {
    i: activeTokens.input_tokens,
    o: activeTokens.output_tokens,
    cr: activeTokens.cache_read_tokens,
    any:
      activeTokens.input_tokens +
        activeTokens.output_tokens +
        activeTokens.cache_read_tokens +
        activeTokens.cache_write_tokens >
      0,
  };

  // 実行時間 = このセッションの最初のステップから最後のステップまでのスパン（ライブ
  // セッションでは「現在」まで伸び続ける — 上の 1.5 秒 setTick が再レンダリングする）。
  const runDuration = React.useMemo(() => {
    let min = Infinity,
      max = 0;
    for (const a of activity) {
      const t = Date.parse(a.ts);
      if (!Number.isFinite(t)) continue;
      if (t < min) min = t;
      if (t > max) max = t;
    }
    if (!Number.isFinite(min) || max === 0) return null;
    const end = active.live ? Date.now() : max;
    return Math.max(0, end - min);
  }, [activity, active.live]);

  // ---- トランスクリプトの自動スクロール（開く → 最下部。上へスクロールしない限り最下部に固定）----
  const contentRef = React.useRef<HTMLDivElement | null>(null);
  const viewport = React.useCallback(
    () => (contentRef.current?.closest('[data-slot="scroll-area-viewport"]') as HTMLElement | null) ?? null,
    [],
  );
  // biome-ignore lint/correctness/useExhaustiveDependencies: activeId は意図的にスクロールリスナーを再バインドする。
  React.useEffect(() => {
    const vp = viewport();
    if (!vp) return;
    const onScroll = () => {
      if (approvalFocus.state && !focusHistory.ready) return;
      atBottomRef.current = vp.scrollTop + vp.clientHeight >= vp.scrollHeight - 60;
      if (vp.scrollTop <= 80) loadEarlier(activeKeyRef.current, viewport); // 上部付近 → 古いページ
    };
    vp.addEventListener("scroll", onScroll, { passive: true });
    return () => vp.removeEventListener("scroll", onScroll);
  }, [viewport, activeId, loadEarlier, approvalFocus.state, focusHistory.ready]);
  // セッションを開く/切り替える → 最新（最下部）へジャンプ
  // biome-ignore lint/correctness/useExhaustiveDependencies: activeId は意図的に新しく選択されたセッションへスクロールする。
  React.useLayoutEffect(() => {
    const vp = viewport();
    if (vp && !approvalFocus.state) {
      vp.scrollTop = vp.scrollHeight;
      atBottomRef.current = true;
    }
  }, [activeId, viewport, approvalFocus.state]);
  // 新規アクティビティ → ユーザーが既に最下部に固定している場合のみ追従
  // biome-ignore lint/correctness/useExhaustiveDependencies: activity の増加は意図的にライブエッジへのスクロールを駆動する。
  React.useLayoutEffect(() => {
    if (approvalFocus.state || !atBottomRef.current) return;
    const vp = viewport();
    if (vp) vp.scrollTop = vp.scrollHeight;
  }, [activity, viewport, approvalFocus.state]);
  // AnswerBlock / ToolBlock / Markdown の遅延詳細読み込みは、アクティビティ配列が確定した
  // 後に内容を増やすが、その参照は変えない。そのため上のレイアウトエフェクトは再発火せず、
  // 開いたばかりのセッションで最後のメッセージが部分的に画面外へスクロールされてしまう
  // （最終回答が 1 行要約から折り返し下の完全な Markdown へ展開される）。ResizeObserver は、
  // ユーザーがまだ最下部にいる間、高さの変化のたびに最下部へ再固定する。これによりメイン
  // Agent を開くと最後のメッセージが完全に表示された状態で着地する。contentRef の div は
  // 常にマウントされているので、オブザーバーはトランスクリプトのマウントと各詳細の展開を
  // 捉えられる。
  // biome-ignore lint/correctness/useExhaustiveDependencies: activeId は意図的にオブザーバーを新しいセッションの内容へ再バインドする。
  React.useEffect(() => {
    const el = contentRef.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(() => {
      if (approvalFocus.state || !atBottomRef.current) return;
      const vp = viewport();
      if (vp) vp.scrollTop = vp.scrollHeight;
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [activeId, viewport, approvalFocus.state]);

  function stop() {
    if (stopping) return;
    setStopping(true);
    void api.stopChat(taskId).finally(() => setStopping(false));
  }

  function send() {
    const text = input.trim();
    const atts = attachments;
    if (side.handleCommand(text, () => setInput(""))) return;
    if ((!text && atts.length === 0) || sending || mainBusy) return;
    // 楽観的エコーはしない。バックエンドが人間のターンを返す前に永続化 + ブロードキャストする
    // ため、実際の DB seq を持つ SSE（worker="mainagent"）として返ってくる。トランスクリプトは
    // 他のあらゆるステップと同様にサーバーデータから描画する。応答性のため入力欄は先に
    // クリアし、送信に失敗したら元に戻す。
    setInput("");
    setAttachments([]);
    setSending(true);
    chatStatusRequestRef.current++;
    api
      .chat(text, taskId, atts.length > 0 ? atts : undefined, active.seg ?? 0)
      .then(({ mode }) => {
        chatStatusRequestRef.current++;
        setMainChatRunning(mode === "llm");
      })
      .catch((e) => {
        setInput(text); // ユーザーがテキスト/添付を失わないよう復元する
        setAttachments(atts);
        toast.error(`送信に失敗しました：${(e as Error).message || "しばらくしてから再試行してください"}`);
      })
      .finally(() => setSending(false));
  }

  function sendWorkerChat() {
    const intentId = active.intent_id;
    const message = workerMessage.trim();
    if (side.handleCommand(message, () => setWorkerMessage(""))) return;
    if (!intentId || active.inherited || active.status !== "paused" || workerMessageSending || !message) return;
    if (workerMessageCharCount(message) > MAX_WORKER_MESSAGE_CHARS) {
      toast.error(`メッセージは ${MAX_WORKER_MESSAGE_CHARS} 文字以内で入力してください`);
      return;
    }
    const requestId = workerMessageRequestId || newWorkerMessageRequestID();
    if (!workerMessageRequestId) setWorkerMessageRequestId(requestId);
    setWorkerMessageSending(true);
    api
      .sendWorkerMessage(taskId, intentId, message, requestId)
      .then((result) => {
        // サーバーがユーザーのターンを記録し、実行を SSE でストリーミングするため、楽観的な
        // 挿入は不要。メッセージと継続実行はライブで届く。
        patchIntentState(intentId, result.state);
        setWorkerMessage("");
        setWorkerMessageRequestId("");
        toast.success(`Worker #${intentId} にメッセージを送信しました。即座に実行を再開します`);
      })
      .catch((error) => {
        toast.error(`送信に失敗しました：${(error as Error).message || "しばらくしてから再試行してください"}`);
      })
      .finally(() => setWorkerMessageSending(false));
  }

  const mainLoaded = !!store[currentMainKey]?.loaded;
  // 送信キーはシステム設定（localStorage）で決まり、既定は Enter 送信。
  const sendMode = useChatSendMode();
  // アクティブなセッションでトランスクリプトペインに表示すべき内容。
  const showLoader = !activeState || (activeState.loading && !activeState.loaded);
  const resolutionForSession = (session: Session): TaskLLMResolution | undefined => {
    if (!llmResolutions || session.inherited || session.role === "system") return undefined;
    if (session.role === "mainagent") return llmResolutions.mainagent;
    if (session.role === "planner") return llmResolutions.planner;
    return llmResolutions.worker;
  };
  const activeResolution = resolutionForSession(active);
  const activeAssets =
    active.role === "worker" && active.intent_id ? intentAssetsByID.get(active.intent_id) : undefined;

  return (
    <TooltipProvider delayDuration={300}>
      {/* 高さの予約:ページヘッダー（タイトル行 + ゴール + Tabs ≈ 7.5rem）+ コンテンツの内側余白。
        モバイルは p-4、デスクトップは lg:p-6 で、デスクトップはスクロール余白も確保するため、
        2 段階で 10rem / 13rem を予約する。モバイルで 13rem を流用すると 3rem が記録領域を
        無駄に食ってしまう。 */}
      <div
        className={cn(
          "grid h-[calc(100svh-10rem)] min-h-0 grid-cols-1 gap-4",
          listOpen ? "grid-rows-[minmax(0,40svh)_minmax(0,1fr)]" : "grid-rows-[auto_minmax(0,1fr)]",
          "lg:h-[calc(100svh-13rem)] lg:grid-cols-[18rem_1fr] lg:grid-rows-[minmax(0,1fr)]",
        )}
      >
        {/* 左:セッション一覧 */}
        <div className="flex min-h-0 flex-col overflow-hidden rounded-lg border bg-card">
          <div className="border-b px-3 py-2">
            <div className="flex items-center justify-between gap-2">
              <button
                type="button"
                onClick={() => setListOpen((open) => !open)}
                aria-expanded={listOpen}
                aria-controls="session-list-panel"
                className="flex min-w-0 flex-1 items-center gap-1 text-left text-xs font-medium text-muted-foreground lg:pointer-events-none"
              >
                <ChevronDownIcon
                  className={cn("size-3.5 shrink-0 transition-transform lg:hidden", !listOpen && "-rotate-90")}
                />
                <span className="shrink-0">セッション一覧</span>
                {!listOpen && (
                  <>
                    <span className="min-w-0 truncate text-foreground lg:hidden" title={activeDisplayTitle}>
                      · {activeDisplayTitle}
                    </span>
                    {hiddenUnread > 0 && (
                      <span className="inline-flex min-w-4 shrink-0 items-center justify-center rounded-full bg-blue-500/15 px-1 text-[10px] font-medium tabular-nums text-blue-600 lg:hidden dark:text-blue-400">
                        {hiddenUnread > 99 ? "99+" : hiddenUnread}
                      </span>
                    )}
                  </>
                )}
              </button>
              {!MOCK && (
                <span
                  className={cn(
                    "inline-flex items-center gap-1 text-[10px]",
                    sseLive ? "text-emerald-500" : "text-amber-500",
                  )}
                  title={
                    sseLive
                      ? "リアルタイム接続は正常です"
                      : "リアルタイム接続が切断されました。自動再接続中（履歴は引き続き表示可能）"
                  }
                >
                  {sseLive ? (
                    <>
                      <span className="size-1 animate-pulse rounded-full bg-emerald-500" />
                      リアルタイム
                    </>
                  ) : (
                    <>
                      <WifiOffIcon className="size-3" />
                      再接続中
                    </>
                  )}
                </span>
              )}
            </div>
            {taskTokens && (
              <Tooltip>
                <TooltipTrigger asChild>
                  <div className="mt-2 flex min-w-0 flex-wrap items-center gap-1 text-xs text-muted-foreground">
                    <span>タスク合計</span>
                    <span>·</span>
                    <TokenMetrics
                      input={taskTokens.input_tokens}
                      cache={taskTokens.cache_read_tokens}
                      output={taskTokens.output_tokens}
                    />
                  </div>
                </TooltipTrigger>
                <TooltipContent>
                  入力 {taskTokens.input_tokens.toLocaleString()} · 出力 {taskTokens.output_tokens.toLocaleString()} ·
                  キャッシュ読取 {taskTokens.cache_read_tokens.toLocaleString()} · キャッシュ書込{" "}
                  {taskTokens.cache_write_tokens.toLocaleString()}
                </TooltipContent>
              </Tooltip>
            )}
          </div>
          <ScrollArea
            id="session-list-panel"
            type="auto"
            className={cn(
              "min-h-0 flex-1 [&_[data-slot=scroll-area-viewport]>div]:block!",
              !listOpen && "max-lg:hidden",
            )}
          >
            <div className="flex w-full flex-col gap-3 p-2">
              {(["mainagent", "planner", "system", "worker"] as const).map((role) => {
                const items = grouped[role];
                if (!items.length) return null;
                const Meta = roleMeta[role];
                return (
                  <div key={role} className="flex flex-col gap-0.5">
                    <div className="flex items-center gap-1.5 px-2 py-1 text-xs font-medium text-muted-foreground">
                      <Meta.icon className="size-3.5" />
                      {Meta.label}
                      {role === "mainagent" && (
                        <button
                          type="button"
                          onClick={() => setConfirmNewMain(true)}
                          disabled={creatingMain}
                          title="新しいメイン Agent セッション（コンテキストをクリア、タスク状態は保持）"
                          aria-label="新しいメイン Agent セッション"
                          className="ml-auto flex items-center gap-1 rounded px-1.5 py-0.5 text-[11px] text-muted-foreground hover:bg-accent/60 hover:text-foreground disabled:opacity-50"
                        >
                          {creatingMain ? (
                            <Loader2Icon className="size-3.5 animate-spin" />
                          ) : (
                            <PlusIcon className="size-3.5" />
                          )}
                          新規
                        </button>
                      )}
                    </div>
                    {items.map((s) => {
                      const meta = s.role === "worker" ? sessionMeta.get(s.id) : undefined;
                      return (
                        <SessionItem
                          key={s.id}
                          s={s}
                          active={s.id === activeId}
                          displayTitle={meta?.title ?? s.title}
                          hasPending={hasPendingForSession(s)}
                          unread={store[keyForSession(s)]?.unread}
                          onClick={() => {
                            approvalFocus.close();
                            setActiveId(s.id);
                            setListOpen(false); // モバイルでは選択後に折りたたみ、高さを記録領域へ戻す
                            setWorkerMessage("");
                            setWorkerMessageRequestId("");
                          }}
                          controlling={controllingIntent === s.intent_id}
                          deleted={meta?.deleted}
                          onCancel={() => {
                            setCancelIntent(s);
                          }}
                        />
                      );
                    })}
                    {role === "worker" && intentsHasMore && (
                      <button
                        type="button"
                        onClick={loadOlderIntents}
                        disabled={loadingOlderIntents}
                        className="mt-0.5 flex items-center justify-center gap-1 rounded-md px-2 py-1 text-xs text-muted-foreground hover:bg-accent/50"
                      >
                        {loadingOlderIntents ? (
                          <Loader2Icon className="size-3.5 animate-spin" />
                        ) : (
                          <RotateCwIcon className="size-3.5" />
                        )}
                        さらに古い Worker を読み込む
                      </button>
                    )}
                  </div>
                );
              })}
              {mainLoaded && !workerSessions.length && (
                <div className="px-2 py-1 text-xs text-muted-foreground">実行中の Worker セッションはありません。</div>
              )}
            </div>
          </ScrollArea>
        </div>

        {/* 右:トランスクリプト */}
        <SideQuestionWorkspace
          side={side}
          label={active.role === "worker" ? `Worker #${active.intent_id} · ${activeDisplayTitle}` : activeDisplayTitle}
        >
          <div className="flex min-h-0 flex-1 min-w-0 flex-col overflow-hidden rounded-lg border bg-card">
            <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 border-b px-3 py-2 sm:px-4 sm:py-2.5">
              {(() => {
                const isWorker = active.role === "worker";
                const meta = isWorker ? sessionMeta.get(active.id) : undefined;
                // Worker:意図はメッセージとしてトランスクリプトへ移ったため、ヘッダーには
                // 安定した汎用ラベルを表示する（意図の JSON はホバーで見られる）。
                const title = isWorker ? "Worker 実行セッション" : active.title;
                const titleEl = <span className="min-w-0 truncate text-sm font-medium">{title}</span>;
                return meta?.json ? (
                  <Tooltip>
                    <TooltipTrigger asChild>{titleEl}</TooltipTrigger>
                    <TooltipContent side="bottom" align="start" className="max-h-80 max-w-sm overflow-auto p-0">
                      <pre className="p-2 text-[10px] leading-relaxed">{JSON.stringify(meta.json, null, 2)}</pre>
                    </TooltipContent>
                  </Tooltip>
                ) : (
                  titleEl
                );
              })()}
              <SideQuestionButton side={side} />
              {activeResolution && (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <Badge
                      variant="outline"
                      className="max-w-28 shrink-0 font-normal"
                      aria-label={
                        activeResolution.available
                          ? `現在の設定：${resolutionLabel(activeResolution)}`
                          : "モデル利用不可"
                      }
                    >
                      <span className="truncate">
                        {activeResolution.available ? resolutionLabel(activeResolution) : "モデル利用不可"}
                      </span>
                    </Badge>
                  </TooltipTrigger>
                  <TooltipContent side="bottom" className="max-w-xs [overflow-wrap:anywhere]">
                    {activeResolution.available
                      ? [resolutionLabel(activeResolution), activeResolution.model].filter(Boolean).join(" / ")
                      : activeResolution.reason || "利用可能な LLM 設定がありません"}
                  </TooltipContent>
                </Tooltip>
              )}
              {activeAssets && activeAssets.length > 0 && <WorkerAssetBadge assets={activeAssets} />}
              {active.inherited && active.source_task_id && (
                <Badge variant="outline">ソースタスク #{active.source_task_id} · 読み取り専用の履歴</Badge>
              )}
              {active.live && (
                <span className="inline-flex items-center gap-1 rounded bg-blue-500/15 px-1.5 py-0.5 text-[10px] font-medium text-blue-600 dark:text-blue-400">
                  <span className="size-1 animate-pulse rounded-full bg-blue-500" />
                  リアルタイム
                </span>
              )}
              {activeState?.hasMore && (
                <span className="text-[10px] text-muted-foreground" title="上へスクロールすると古い履歴を読み込みます">
                  ↑ 過去の履歴
                </span>
              )}
              <div className="ml-auto flex min-w-0 max-w-full items-center justify-end gap-x-3 gap-y-1 text-xs text-muted-foreground max-sm:w-full max-sm:flex-wrap">
                {tokenTotal.any && (
                  <Tooltip>
                    {/* モバイルでは短いラベル（入/キ/出）を使う。長いラベルだとこの行が 2 行になり、
                    記録領域がさらに圧迫されるため。 */}
                    <TooltipTrigger asChild>
                      <span className="inline-flex min-w-0 items-center">
                        <TokenMetrics
                          input={tokenTotal.i}
                          cache={tokenTotal.cr}
                          output={tokenTotal.o}
                          labels="short"
                          className="sm:hidden"
                        />
                        <TokenMetrics
                          input={tokenTotal.i}
                          cache={tokenTotal.cr}
                          output={tokenTotal.o}
                          labels="long"
                          className="max-sm:hidden"
                        />
                      </span>
                    </TooltipTrigger>
                    <TooltipContent>
                      入力 {activeTokens.input_tokens.toLocaleString()} · 出力{" "}
                      {activeTokens.output_tokens.toLocaleString()} · キャッシュ読取{" "}
                      {activeTokens.cache_read_tokens.toLocaleString()} · キャッシュ書込{" "}
                      {activeTokens.cache_write_tokens.toLocaleString()}
                    </TooltipContent>
                  </Tooltip>
                )}
                {runDuration != null && (
                  <span className="inline-flex items-center gap-1" title="実行時間（最初のステップ → 最後のステップ）">
                    <ClockIcon className="size-3" />
                    {fmtDuration(runDuration)}
                  </span>
                )}
                {isMain && <span>対話可能</span>}
              </div>
            </div>
            {(() => {
              const dm = active.role === "worker" ? sessionMeta.get(active.id) : undefined;
              if (!dm?.deleted) return null;
              return (
                <div className="flex items-start gap-2 border-b border-destructive/30 bg-destructive/5 px-4 py-2.5 text-xs">
                  <Trash2Icon className="mt-0.5 size-3.5 shrink-0 text-destructive" />
                  <div className="min-w-0">
                    <span className="font-medium text-destructive">この意図はユーザーにより削除されました</span>
                    <span className="text-muted-foreground">
                      （実行は停止済みで、プランナーに通知済み。意図と成果は保持されており、下で履歴を確認できます）
                    </span>
                    {dm.deleteReason && (
                      <p className="mt-1 break-words text-foreground">
                        <span className="text-muted-foreground">削除理由：</span>
                        {dm.deleteReason}
                      </p>
                    )}
                  </div>
                </div>
              );
            })()}
            <ApprovalExecutionFocus
              focus={{
                ...approvalFocus,
                close: () => {
                  setActiveId(activeId);
                  approvalFocus.close();
                },
              }}
              history={focusHistory}
            />
            {/* Radix 内部の viewport ラッパー（display:table で内容に合わせてサイズ調整）を
            強制的に block にする。これにより幅が広く改行不能なステップ（長いコマンド、コード、
            URL）でも幅を押し広げて下の切り詰めを無効化できない — トランスクリプトは水平に
            はみ出さずパネル幅で折り返す。 */}
            <ScrollArea type="auto" className="min-h-0 min-w-0 flex-1 [&_[data-slot=scroll-area-viewport]>div]:block!">
              <div className="min-w-0 max-w-full p-4" ref={contentRef}>
                {activeState?.loadingMore && (
                  <div className="flex items-center justify-center gap-2 pb-2 text-xs text-muted-foreground">
                    <Loader2Icon className="size-3.5 animate-spin" />
                    過去の履歴を読み込み中…
                  </div>
                )}
                {showLoader ? (
                  <div className="flex items-center gap-2 pl-9 text-xs text-muted-foreground">
                    <Loader2Icon className="size-3.5 animate-spin" />
                    アクティビティストリームを読み込み中…
                  </div>
                ) : activeState?.error ? (
                  <div className="flex items-center gap-2 pl-9 text-xs text-red-500">
                    <CircleXIcon className="size-3.5" />
                    読み込みに失敗しました：{activeState.error}
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-6 px-2 text-xs"
                      onClick={() => loadSession(activeKey)}
                    >
                      再試行
                    </Button>
                  </div>
                ) : activity.length ? (
                  <Transcript
                    activity={activity}
                    live={active.live}
                    taskId={taskId}
                    chat={isMain}
                    focusedSeq={focusHistory.ready ? approvalFocus.state?.source?.seq : undefined}
                  />
                ) : (
                  <div className="pl-9 text-xs text-muted-foreground">
                    {isMain
                      ? "まだ会話がありません。下でメイン Agent にメッセージを送り、探索の方向を導いたり、フローに介入したりできます。"
                      : "アクティビティ記録はまだありません。"}
                  </div>
                )}
              </div>
            </ScrollArea>
            {isMain ? (
              <div className="border-t p-3">
                {attachments.length > 0 && (
                  <div className="mb-2 flex flex-wrap gap-1.5">
                    {attachments.map((a) => (
                      <div
                        key={a.path}
                        className="flex items-center gap-1.5 rounded-md border bg-muted/50 px-2 py-1 text-xs"
                        title={a.path}
                      >
                        <PaperclipIcon className="size-3 shrink-0 text-primary" />
                        <span className="max-w-[160px] truncate">{a.name}</span>
                        <span className="text-muted-foreground">{fmtBytes(a.size)}</span>
                        <button
                          type="button"
                          className="ml-0.5 text-muted-foreground hover:text-foreground"
                          onClick={() => setAttachments((p) => p.filter((x) => x.path !== a.path))}
                          title="削除"
                        >
                          <XIcon className="size-3" />
                        </button>
                      </div>
                    ))}
                  </div>
                )}
                <input
                  ref={fileInputRef}
                  type="file"
                  multiple
                  className="hidden"
                  onChange={(e) => void pickFiles(e.target.files)}
                />
                <InputGroup className="min-h-9 has-disabled:opacity-100">
                  <MentionTextarea
                    inputGroup
                    rows={1}
                    aria-label="メイン Agent にメッセージを送信"
                    placeholder={
                      mainBusy
                        ? "メイン Agent が実行中です。/btw で質問できます…"
                        : "メイン Agent にメッセージを送信。@ で脆弱性や資産などを参照できます…"
                    }
                    value={input}
                    disabled={sending}
                    onValueChange={setInput}
                    onKeyDown={(e) => {
                      if (!shouldSubmitOnKey(e, sendMode)) return;
                      e.preventDefault();
                      send();
                    }}
                    className="max-h-36 min-h-9 overflow-y-auto"
                  />
                  <InputGroupAddon align="block-end">
                    <InputGroupButton
                      size="icon-xs"
                      variant="ghost"
                      onClick={() => fileInputRef.current?.click()}
                      disabled={mainBusy || uploading}
                      title="ファイルをアップロード"
                      aria-label="ファイルをアップロード"
                    >
                      {uploading ? <Loader2Icon className="animate-spin" /> : <PaperclipIcon />}
                    </InputGroupButton>
                    {mainBusy && isBtwCommand(input) && (
                      <InputGroupButton size="icon-xs" onClick={send} aria-label="サイド質問を送信">
                        <ArrowUpIcon />
                      </InputGroupButton>
                    )}
                    {mainBusy ? (
                      <InputGroupButton
                        className="ml-auto"
                        size="icon-xs"
                        variant="destructive"
                        onClick={stop}
                        disabled={stopping}
                        title="現在の実行を停止"
                        aria-label="現在の実行を停止"
                      >
                        {stopping ? <Loader2Icon className="animate-spin" /> : <SquareIcon />}
                      </InputGroupButton>
                    ) : (
                      <InputGroupButton
                        className="ml-auto"
                        size="icon-xs"
                        variant="default"
                        onClick={send}
                        disabled={(!input.trim() && attachments.length === 0) || sending}
                        title="メッセージを送信"
                        aria-label="メッセージを送信"
                      >
                        {sending ? <Loader2Icon className="animate-spin" /> : <ArrowUpIcon />}
                      </InputGroupButton>
                    )}
                  </InputGroupAddon>
                </InputGroup>
              </div>
            ) : active.role === "worker" &&
              !active.inherited &&
              (active.status === "running" || active.status === "paused") ? (
              <div className="border-t p-3">
                <InputGroup className="min-h-9 has-disabled:opacity-100">
                  <MentionTextarea
                    inputGroup
                    rows={1}
                    aria-label={`Worker #${active.intent_id} にメッセージを送信`}
                    placeholder={`Worker #${active.intent_id} にメッセージを送信。@ で記録を参照し、実行方針を調整…`}
                    value={workerMessage}
                    onValueChange={(value) => {
                      setWorkerMessage(value);
                      setWorkerMessageRequestId("");
                    }}
                    onKeyDown={(event) => {
                      if (!shouldSubmitOnKey(event, sendMode)) return;
                      event.preventDefault();
                      sendWorkerChat();
                    }}
                    disabled={workerMessageSending}
                    aria-invalid={workerMessageCharCount(workerMessage) > MAX_WORKER_MESSAGE_CHARS}
                    className="max-h-36 min-h-9 overflow-y-auto"
                  />
                  <InputGroupAddon align="block-end">
                    <span
                      className={cn(
                        "px-1 text-[10px] tabular-nums text-muted-foreground",
                        workerMessageCharCount(workerMessage) > MAX_WORKER_MESSAGE_CHARS && "text-destructive",
                      )}
                    >
                      {workerMessageCharCount(workerMessage)}/{MAX_WORKER_MESSAGE_CHARS}
                    </span>
                    {active.status === "running" && isBtwCommand(workerMessage) && (
                      <InputGroupButton size="icon-xs" onClick={sendWorkerChat} aria-label="サイド質問を送信">
                        <ArrowUpIcon />
                      </InputGroupButton>
                    )}
                    {active.status === "running" ? (
                      <InputGroupButton
                        className="ml-auto"
                        size="icon-xs"
                        variant="destructive"
                        onClick={() => void controlWorker(active, "pause")}
                        disabled={controllingIntent === active.intent_id}
                        title="現在の Worker を一時停止"
                        aria-label="現在の Worker を一時停止"
                      >
                        {controllingIntent === active.intent_id ? (
                          <Loader2Icon className="animate-spin" />
                        ) : (
                          <SquareIcon />
                        )}
                      </InputGroupButton>
                    ) : (
                      <>
                        <InputGroupButton
                          className="ml-auto"
                          size="xs"
                          variant="ghost"
                          onClick={() => void controlWorker(active, "resume")}
                          disabled={controllingIntent === active.intent_id || workerMessageSending}
                          title="メッセージなしで実行を継続"
                          aria-label="メッセージなしで実行を継続"
                        >
                          {controllingIntent === active.intent_id ? (
                            <Loader2Icon className="animate-spin" />
                          ) : (
                            "そのまま継続"
                          )}
                        </InputGroupButton>
                        <InputGroupButton
                          size="icon-xs"
                          variant="default"
                          onClick={sendWorkerChat}
                          disabled={
                            workerMessageSending ||
                            !workerMessage.trim() ||
                            workerMessageCharCount(workerMessage) > MAX_WORKER_MESSAGE_CHARS
                          }
                          title="メッセージを送信"
                          aria-label="メッセージを送信"
                        >
                          {workerMessageSending ? <Spinner /> : <ArrowUpIcon />}
                        </InputGroupButton>
                      </>
                    )}
                  </InputGroupAddon>
                </InputGroup>
              </div>
            ) : (
              <div className="flex items-center border-t px-4 py-2">
                <TodoPopover
                  seq={latestTodoSeq}
                  fetchDetail={(seq) => api.activityDetail(seq, taskId).then((r) => r.detail ?? "")}
                />
              </div>
            )}
          </div>
        </SideQuestionWorkspace>
        <AlertDialog
          open={cancelIntent !== null}
          onOpenChange={(open) => {
            if (!open) {
              setCancelIntent(null);
              setCancelReason("");
              setDeleteMode("soft");
            }
          }}
        >
          <AlertDialogContent className="max-w-[min(32rem,calc(100vw-2rem))]">
            <AlertDialogHeader>
              <AlertDialogTitle>Worker #{cancelIntent?.intent_id} を削除しますか？</AlertDialogTitle>
              <AlertDialogDescription className="break-words whitespace-normal">
                {deleteMode === "hard" ? (
                  <>
                    <strong>完全削除</strong>は、この意図と、<strong>それだけに支えられた</strong>
                    下流ノードを物理的に削除します（リーフまでカスケードし、孤立データを残しません）。
                    共有ノード、ゴール、タスクのルート事実は保持されます。
                    <strong>この操作は元に戻せません。</strong>
                    プランナーは削除通知を受け取り、それに基づいて再計画します。
                  </>
                ) : (
                  <>
                    <strong>ソフト削除</strong>
                    はこの意図を「削除済み」にし、削除理由を記録します。意図ノード、実行記録、 登録済みの事実と脆弱性は
                    <strong>すべて保持されます</strong>。プランナーは「この意図はユーザーにより削除 +
                    理由」を受け取り、それに基づいて再計画します。
                  </>
                )}
              </AlertDialogDescription>
            </AlertDialogHeader>
            <div className="grid gap-3 py-1">
              <div className="grid grid-cols-2 gap-2">
                <button
                  type="button"
                  onClick={() => setDeleteMode("soft")}
                  className={cn(
                    "rounded-md border px-3 py-2 text-left text-sm transition-colors",
                    deleteMode === "soft" ? "border-primary bg-primary/5" : "hover:bg-accent",
                  )}
                >
                  <div className="font-medium">ソフト削除</div>
                  <div className="text-xs text-muted-foreground">データを保持し、追跡可能</div>
                </button>
                <button
                  type="button"
                  onClick={() => setDeleteMode("hard")}
                  className={cn(
                    "rounded-md border px-3 py-2 text-left text-sm transition-colors",
                    deleteMode === "hard" ? "border-destructive bg-destructive/5" : "hover:bg-accent",
                  )}
                >
                  <div className="font-medium">完全削除</div>
                  <div className="text-xs text-muted-foreground">カスケード削除、復元不可</div>
                </button>
              </div>
              <div className="grid gap-2">
                <label htmlFor="cancel-reason" className="text-sm font-medium">
                  削除理由（必須）
                </label>
                <Textarea
                  id="cancel-reason"
                  value={cancelReason}
                  onChange={(e) => setCancelReason(e.target.value)}
                  placeholder="この意図を削除する理由を記入してください。例:方向判断の誤り / ゴールが無効になった / 他の意図と重複…"
                  rows={3}
                  autoFocus
                />
              </div>
            </div>
            <AlertDialogFooter>
              <AlertDialogCancel>戻る</AlertDialogCancel>
              <AlertDialogAction
                variant="destructive"
                disabled={!cancelIntent || controllingIntent !== null || !cancelReason.trim()}
                onClick={() => cancelIntent && void controlWorker(cancelIntent, "cancel", cancelReason, deleteMode)}
              >
                {controllingIntent ? <Loader2Icon className="animate-spin" /> : <Trash2Icon />}
                {deleteMode === "hard" ? "完全削除する" : "削除を確認"}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>

        <AlertDialog open={confirmNewMain} onOpenChange={(open) => !open && setConfirmNewMain(false)}>
          <AlertDialogContent className="max-w-[min(32rem,calc(100vw-2rem))]">
            <AlertDialogHeader>
              <AlertDialogTitle>新しいセッションを開始しますか？</AlertDialogTitle>
              <AlertDialogDescription className="break-words whitespace-normal">
                現在のセッションはアーカイブされ（いつでも切り替え可能）、メイン Agent
                はクリーンなコンテキストで続行します。タスクのグラフ、資産、ゴールは影響を受けません。
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel disabled={creatingMain}>キャンセル</AlertDialogCancel>
              <AlertDialogAction disabled={creatingMain} onClick={() => void createMainSession()}>
                {creatingMain ? "開始中…" : "新しいセッションを開始"}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </div>
    </TooltipProvider>
  );
}
