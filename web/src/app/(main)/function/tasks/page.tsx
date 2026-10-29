"use client";

import * as React from "react";

import Link from "next/link";

import {
  DndContext,
  type DragEndEvent,
  DragOverlay,
  type DragStartEvent,
  KeyboardSensor,
  PointerSensor,
  useDraggable,
  useDroppable,
  useSensor,
  useSensors,
} from "@dnd-kit/core";
import {
  ArchiveIcon,
  ArrowDownIcon,
  ArrowUpDownIcon,
  ArrowUpIcon,
  ChevronRightIcon,
  EyeIcon,
  FolderInputIcon,
  GripVerticalIcon,
  Loader2Icon,
  PaperclipIcon,
  PauseIcon,
  PencilIcon,
  PinIcon,
  PinOffIcon,
  PlayIcon,
  PlusIcon,
  SaveIcon,
  SearchIcon,
  SlidersHorizontalIcon,
  StarIcon,
  TagsIcon,
  Trash2Icon,
  Undo2Icon,
  XIcon,
} from "lucide-react";
import { toast } from "sonner";

import { AssetInterceptRulesEditor } from "@/components/asset-intercept-rules-editor";
import { StatusBadge } from "@/components/status-badge";
import { TablePagination } from "@/components/table-pagination";
import { TaskLLMProfileChain } from "@/components/task-llm-profile-chain";
import { TaskTemplateControls } from "@/components/task-template-controls";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import {
  Combobox,
  ComboboxChip,
  ComboboxChips,
  ComboboxChipsInput,
  ComboboxContent,
  ComboboxEmpty,
  ComboboxItem,
  ComboboxList,
  ComboboxValue,
} from "@/components/ui/combobox";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import {
  Field,
  FieldContent,
  FieldDescription,
  FieldGroup,
  FieldLabel,
  FieldLegend,
  FieldSet,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Item, ItemActions, ItemContent, ItemDescription, ItemGroup, ItemMedia, ItemTitle } from "@/components/ui/item";
import { Label } from "@/components/ui/label";
import { Progress } from "@/components/ui/progress";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import {
  Sheet,
  SheetClose,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
  SheetTrigger,
} from "@/components/ui/sheet";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { api } from "@/lib/api";
import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";
import { type SortDirection, useStoredSortPreference } from "@/lib/sort-preference";
import type {
  AssetInterceptRuleInput,
  ChatAttachment,
  Company,
  DeleteTaskOptions,
  DeleteTaskResult,
  LLMProfile,
  Task,
  TaskArchive,
  TaskArchiveState,
  TaskCategory,
  TaskStatus,
} from "@/lib/types";
import { cn } from "@/lib/utils";

// fmtBytes はアップロードマニフェスト用に人が読めるファイルサイズを返す。
function fmtBytes(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`;
  return `${n} B`;
}

// UPLOAD_MARKER はタスク説明内に自動追記されるアップロードファイルパスのブロックを識別する。
// 再アップロード時は新しいヘッダーを追加せず、同じブロックの下に追記する。
// 互換性のため中国語のまま: 既存タスクの説明にはこの中国語マーカーが保存済みで、
// 日本語化すると desc.includes(UPLOAD_MARKER) が一致しなくなり、再アップロード時に
// 二重ヘッダーが付いてしまう。
const UPLOAD_MARKER = "【上传文件（绝对路径）】";

// appendUploads は新たにアップロードされたファイルの絶対パスを、Read/Bash で扱いやすい
// マニフェストとして説明文に折り込む。worker はパスで開く。マーク付きブロックは 1 つだけに保つ:
// 最初のアップロードでヘッダーを追加し、以降はその下に箇条書きを追記する。
function appendUploads(desc: string, atts: ChatAttachment[]): string {
  const bullets = atts.map((a) => `- ${a.abs ?? a.path}（${fmtBytes(a.size)}）`).join("\n");
  if (desc.includes(UPLOAD_MARKER)) {
    return `${desc.replace(/\s*$/, "")}\n${bullets}\n`;
  }
  const head = desc.trim() ? `${desc.replace(/\s*$/, "")}\n\n` : "";
  return `${head}${UPLOAD_MARKER} worker は Read/Bash でパスを開けます：\n${bullets}\n`;
}

// POLL_MS はタスク一覧の更新間隔。タスク状態はサーバー側(プランナー /
// worker)で進むため一覧をポーリングする必要がある。ステータス / 進捗 / token の変化には 10 秒で十分。
const POLL_MS = 10_000;
const MAX_SOURCE_TASKS = 8;

// fmtTokens はコンパクトなトークン数を返す(1234 → 1.2k、2_000_000 → 2M)。
function fmtTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1) + "M";
  if (n >= 1000) return (n / 1000).toFixed(n >= 10000 ? 0 : 1) + "k";
  return String(n);
}

// fmtDuration は実行時間(秒)をコンパクトで読みやすい文字列にする。
function fmtDuration(sec: number): string {
  if (sec <= 0) return "—";
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

// taskDuration はタスクの実行時間(秒)を返す:実行中は作成→現在、終了済みなら
// 作成→完了、それ以外は作成→最終アクティビティ。一度も実行していない場合(アクティビティなし)は 0。
function taskDuration(task: Task, nowSec: number): number {
  const start = task.created_unix ?? 0;
  if (!start) return 0;
  const end =
    task.status === "running"
      ? nowSec
      : task.completed_unix && task.completed_unix > 0
        ? task.completed_unix
        : (task.last_activity_unix ?? 0);
  return end > start ? end - start : 0;
}

// deleteDetails は結果のうち件数部分だけを取り出す。呼び出し側が一括削除で集計した値を渡せるようにするため。
type DeleteCounts = Omit<DeleteTaskResult, "deleted" | "cleanup_warning">;

function deleteDetails(result: DeleteCounts): string[] {
  const details: string[] = [];
  if (result.assets_deleted > 0) details.push(`資産を ${result.assets_deleted} 件削除`);
  if (result.assets_detached > 0) details.push(`共有資産の関連付けを ${result.assets_detached} 件解除`);
  if (result.traffic_deleted > 0) details.push(`トラフィックを ${result.traffic_deleted} 件削除`);
  if (result.files_deleted) details.push("タスクファイルを削除");
  if (result.findings_deleted > 0) details.push(`脆弱性を ${result.findings_deleted} 件削除`);
  if (result.llm_records_deleted > 0)
    details.push(`LLM リクエスト/レスポンス記録を ${result.llm_records_deleted} 件削除`);
  return details;
}

function deleteSummary(result: DeleteTaskResult): string {
  const details = deleteDetails(result);
  return details.length > 0 ? `タスクを削除しました（${details.join("、")}）` : "タスクを削除しました";
}

// fmtDateTime は unix 秒のタイムスタンプをコンパクトなローカル日時
// (MM-DD HH:mm)にする。未設定なら "—"。
function fmtDateTime(unix?: number): string {
  if (!unix || unix <= 0) return "—";
  const d = new Date(unix * 1000);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

const STATUS_OPTIONS: { value: TaskStatus; label: string }[] = [
  { value: "created", label: "作成済み" },
  { value: "queued", label: "待機中" },
  { value: "running", label: "実行中" },
  { value: "paused", label: "一時停止" },
  { value: "done", label: "完了" },
  { value: "failed", label: "失敗" },
  { value: "timeout", label: "タイムアウト" },
];

// Select は空文字の value を受け付けないため、「分類なし」はフィルタ、新規作成フォーム、一括移動で
// このセンチネル値に統一し、送信時にバックエンドの null へ変換する。
const UNCATEGORIZED_VALUE = "uncategorized";

// 一時停止可能 = 非終端かつ未停止。バックエンドの applyTaskControlWithCause のゲートと一致する
// (done/failed/timeout が終端)。paused のときのみ再開できる。行内ボタンと一括制御がこの判定を共有し、
// 片方だけ押せる・押せないという食い違いが起きないようにする。
const PAUSABLE_STATUSES = new Set<TaskStatus>(["created", "queued", "running"]);
const ARCHIVABLE_STATUSES = new Set<TaskStatus>(["paused", "done", "failed", "timeout"]);

function taskControlAction(status: TaskStatus): "pause" | "resume" | null {
  if (status === "paused") return "resume";
  if (PAUSABLE_STATUSES.has(status)) return "pause";
  return null;
}

type TaskSortField = "id" | "created" | "duration" | "status";

const TASK_SORT_FIELDS: readonly TaskSortField[] = ["id", "created", "duration", "status"];
const TASK_SORT_PREFERENCE_KEY = "artex_task_list_sort";
const TASK_FILTER_PREFERENCE_KEY = "artex_task_list_filters";

const TASK_STATUS_RANK = new Map(STATUS_OPTIONS.map((option, index) => [option.value, index]));

function taskIsPinned(task: Task): boolean {
  return task.pinned ?? Boolean(task.pinned_at);
}

function compareTaskIDs(left: string, right: string): number {
  return left.localeCompare(right, undefined, { numeric: true, sensitivity: "base" });
}

function taskCreatedUnix(task: Task): number {
  if (task.created_unix) return task.created_unix;
  const parsed = Date.parse(task.created_at);
  return Number.isNaN(parsed) ? 0 : Math.floor(parsed / 1000);
}

function compareTasks(left: Task, right: Task, field: TaskSortField, direction: SortDirection, nowSec: number): number {
  const leftPinned = taskIsPinned(left);
  const rightPinned = taskIsPinned(right);
  if (leftPinned !== rightPinned) return leftPinned ? -1 : 1;
  if (leftPinned && left.pinned_at !== right.pinned_at) {
    return (right.pinned_at ?? "").localeCompare(left.pinned_at ?? "");
  }

  let compared = 0;
  switch (field) {
    case "created":
      compared = taskCreatedUnix(left) - taskCreatedUnix(right);
      break;
    case "duration":
      compared = taskDuration(left, nowSec) - taskDuration(right, nowSec);
      break;
    case "status":
      compared = (TASK_STATUS_RANK.get(left.status) ?? 0) - (TASK_STATUS_RANK.get(right.status) ?? 0);
      break;
    default:
      compared = compareTaskIDs(left.id, right.id);
  }
  if (compared !== 0) return direction === "asc" ? compared : -compared;
  return -compareTaskIDs(left.id, right.id);
}

export default function TasksPage() {
  const [activeTab, setActiveTab] = React.useState("current");
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const [categories, setCategories] = React.useState<TaskCategory[]>([]);
  const [categoriesLoaded, setCategoriesLoaded] = React.useState(false);
  const [query, setQuery] = React.useState("");
  const [statusFilter, setStatusFilter] = React.useState<TaskStatus | "all">("all");
  const [categoryFilter, setCategoryFilter] = React.useState("all");
  const [filtersHydrated, setFiltersHydrated] = React.useState(false);
  const [sortPreference, setSortPreference] = useStoredSortPreference(
    TASK_SORT_PREFERENCE_KEY,
    TASK_SORT_FIELDS,
    "id",
    "desc",
  );
  const { field: sortField, direction: sortDirection } = sortPreference;
  const [page, setPage] = React.useState(1);
  const [pageSize, setPageSize] = React.useState(20);
  const [nowSec, setNowSec] = React.useState(() => Math.floor(Date.now() / 1000));
  const [batchControlling, setBatchControlling] = React.useState<"pause" | "resume" | null>(null);
  const [movingCategory, setMovingCategory] = React.useState(false);

  React.useEffect(() => {
    const raw = getLocalStorageValue(TASK_FILTER_PREFERENCE_KEY);
    if (raw) {
      try {
        const parsed = JSON.parse(raw) as { status?: unknown; category?: unknown };
        if (parsed.status === "all" || STATUS_OPTIONS.some((option) => option.value === parsed.status)) {
          setStatusFilter(parsed.status as TaskStatus | "all");
        }
        if (
          typeof parsed.category === "string" &&
          (parsed.category === "all" || parsed.category === UNCATEGORIZED_VALUE || /^\d+$/.test(parsed.category))
        ) {
          setCategoryFilter(parsed.category);
        }
      } catch {
        // 不正または旧形式の設定を無視し、既定値を保持する。
      }
    }
    setFiltersHydrated(true);
  }, []);

  React.useEffect(() => {
    if (!filtersHydrated) return;
    setLocalStorageValue(
      TASK_FILTER_PREFERENCE_KEY,
      JSON.stringify({ status: statusFilter, category: categoryFilter }),
    );
  }, [categoryFilter, filtersHydrated, statusFilter]);

  const filtered = React.useMemo(() => {
    const q = query.trim().toLowerCase();
    return tasks.filter((t) => {
      if (statusFilter !== "all" && t.status !== statusFilter) return false;
      if (categoryFilter === UNCATEGORIZED_VALUE && t.category_id != null) return false;
      if (
        categoryFilter !== "all" &&
        categoryFilter !== UNCATEGORIZED_VALUE &&
        String(t.category_id) !== categoryFilter
      )
        return false;
      if (!q) return true;
      return (
        (t.name ?? "").toLowerCase().includes(q) ||
        (t.category_name ?? "").toLowerCase().includes(q) ||
        t.description.toLowerCase().includes(q) ||
        t.goal.toLowerCase().includes(q) ||
        t.id.toLowerCase().includes(q)
      );
    });
  }, [tasks, query, statusFilter, categoryFilter]);

  const sortNowSec = sortField === "duration" ? nowSec : 0;
  const ordered = React.useMemo(() => {
    return [...filtered].sort((left, right) => compareTasks(left, right, sortField, sortDirection, sortNowSec));
  }, [filtered, sortDirection, sortField, sortNowSec]);

  const sortTasksBy = React.useCallback(
    (field: TaskSortField) => {
      setSortPreference((current) => {
        if (current.field !== field) return { field, direction: "desc" };
        return { field, direction: current.direction === "asc" ? "desc" : "asc" };
      });
    },
    [setSortPreference],
  );

  // フィルタまたは並び順が変わるたびにページ 1 に戻す
  // biome-ignore lint/correctness/useExhaustiveDependencies: 両方のフィルタが意図的にページネーションをリセットする。
  React.useEffect(() => {
    setPage(1);
  }, [query, statusFilter, categoryFilter, sortField, sortDirection]);

  const paginated = React.useMemo(
    () => ordered.slice((page - 1) * pageSize, page * pageSize),
    [ordered, page, pageSize],
  );

  // 複数選択削除:選択はページ送り/フィルタをまたいで保持し、タスクが実際に消えた(削除された、または
  // バックエンドが返さなくなった)ときだけ絞り込む。
  const [selectedIds, setSelectedIds] = React.useState<Set<string>>(() => new Set());

  React.useEffect(() => {
    setSelectedIds((prev) => {
      if (prev.size === 0) return prev;
      const live = new Set(tasks.map((t) => t.id));
      const next = new Set([...prev].filter((id) => live.has(id)));
      return next.size === prev.size ? prev : next;
    });
  }, [tasks]);

  const toggleSelected = React.useCallback((id: string, checked: boolean) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (checked) next.add(id);
      else next.delete(id);
      return next;
    });
  }, []);

  const pageIds = React.useMemo(() => paginated.map((t) => t.id), [paginated]);
  const pageSelectedCount = React.useMemo(
    () => pageIds.filter((id) => selectedIds.has(id)).length,
    [pageIds, selectedIds],
  );
  let headerChecked: boolean | "indeterminate" = false;
  if (pageIds.length > 0 && pageSelectedCount === pageIds.length) {
    headerChecked = true;
  } else if (pageSelectedCount > 0) {
    headerChecked = "indeterminate";
  }

  const toggleSelectedPage = React.useCallback(
    (checked: boolean) => {
      setSelectedIds((prev) => {
        const next = new Set(prev);
        for (const id of pageIds) {
          if (checked) next.add(id);
          else next.delete(id);
        }
        return next;
      });
    },
    [pageIds],
  );

  // lastRef は前回ポーリング時のシリアライズ済みペイロードを保持する:一覧は POLL_MS ごとに再取得するが
  // 通常は変化がない。同一ペイロードで setTasks するとページ全体が無駄に再描画されるため、
  // 一致したら bail out する。
  const lastRef = React.useRef<string>("");

  const load = React.useCallback(() => {
    api
      .tasks()
      .then((r) => {
        const next = r.tasks.map((t) => (t.id === r.active ? { ...t, active: true } : t));
        const sig = JSON.stringify(next);
        if (sig === lastRef.current) return;
        lastRef.current = sig;
        setTasks(next);
      })
      .catch(() => {
        // ポーリングはベストエフォート。次のインターバルで自動的に再試行する。
      });
  }, []);

  const loadCategories = React.useCallback(() => {
    api
      .taskCategories()
      .then((next) => {
        setCategories(next);
        setCategoriesLoaded(true);
      })
      .catch(() => {
        // 分類管理はタスク一覧をブロックせず再試行可能なままにする。
      });
  }, []);

  React.useEffect(() => {
    load();
    loadCategories();
    const i = setInterval(load, POLL_MS);
    return () => clearInterval(i);
  }, [load, loadCategories]);

  const refreshCategoriesAndTasks = React.useCallback(() => {
    lastRef.current = "";
    loadCategories();
    load();
  }, [load, loadCategories]);

  const applyTaskCategoryMove = React.useCallback(
    (taskID: string, category: TaskCategory | null) => {
      setTasks((current) => {
        const next = current.map((task) =>
          task.id === taskID ? { ...task, category_id: category?.id, category_name: category?.name } : task,
        );
        lastRef.current = JSON.stringify(next);
        return next;
      });
      loadCategories();
    },
    [loadCategories],
  );

  React.useEffect(() => {
    if (!categoriesLoaded) return;
    if (categoryFilter === "all" || categoryFilter === UNCATEGORIZED_VALUE) return;
    if (!categories.some((category) => String(category.id) === categoryFilter)) setCategoryFilter("all");
  }, [categories, categoriesLoaded, categoryFilter]);

  // running のタスクが実際にあるときだけ毎秒 tick する。それ以外は「実行時間」が静的な値なので、
  // 空回りの tick はテーブル全体の無駄な再描画になる。
  const hasRunning = React.useMemo(() => tasks.some((t) => t.status === "running"), [tasks]);

  // 毎秒 tick して、実行中タスクの「実行時間」がライブで進むようにする。
  React.useEffect(() => {
    if (!hasRunning) return;
    setNowSec(Math.floor(Date.now() / 1000));
    const i = setInterval(() => setNowSec(Math.floor(Date.now() / 1000)), 1000);
    return () => clearInterval(i);
  }, [hasRunning]);

  const deleteTask = React.useCallback(
    async (id: string, options: DeleteTaskOptions) => {
      try {
        const result = await api.deleteTask(id, options);
        if (result.cleanup_warning) {
          toast.warning(
            `${deleteSummary(result)}；一部の外部データのクリーンアップが完了していません：${result.cleanup_warning}`,
          );
        } else {
          toast.success(deleteSummary(result));
        }
        load();
      } catch (e) {
        toast.error("削除に失敗しました：" + (e as Error).message);
        throw e;
      }
    },
    [load],
  );

  // controlTask は行内の一時停止/再開:一括は controlTasksBatch、単一行は単一タスク API を叩き、
  // 「先にチェックしてから一括ボタン」の手間を省く。lastRef をクリアすると次回ポーリングは
  // ペイロードが同じでもそのまま受け入れる。そうしないと状態の書き戻しが重複排除で弾かれ、
  // ボタンが反応しないように見える。
  const controlTask = React.useCallback(
    async (id: string, action: "pause" | "resume") => {
      try {
        const result = await api.controlTask(id, action);
        toast.success(
          action === "pause"
            ? `タスク #${id} を一時停止しました`
            : `タスク #${id} を再開しました${result.queued ? "（キューに追加済み）" : ""}`,
        );
      } catch (e) {
        toast.error(`${action === "pause" ? "一時停止" : "再開"}に失敗しました：${(e as Error).message}`);
      } finally {
        // 成否にかかわらず更新する。失敗の多くは状態が既に変わっているためで、再取得して初めて
        // ボタンが正しい形に戻る。
        lastRef.current = "";
        load();
      }
    },
    [load],
  );

  const renameTask = React.useCallback(
    async (task: Task, name: string) => {
      try {
        await api.renameTask(task.id, name);
        toast.success(`タスク #${task.id} の名前を変更しました`);
        lastRef.current = "";
        load();
      } catch (error) {
        toast.error(`名前の変更に失敗しました：${(error as Error).message}`);
        throw error;
      }
    },
    [load],
  );

  const toggleTaskPinned = React.useCallback(
    async (task: Task) => {
      const pinned = taskIsPinned(task);
      try {
        await api.pinTask(task.id, !pinned);
        toast.success(pinned ? `タスク #${task.id} のピン留めを解除しました` : `タスク #${task.id} をピン留めしました`);
        lastRef.current = "";
        load();
      } catch (error) {
        toast.error(`${pinned ? "ピン留め解除" : "ピン留め"}に失敗しました：${(error as Error).message}`);
        throw error;
      }
    },
    [load],
  );

  const queueTaskArchive = React.useCallback(
    async (task: Task) => {
      try {
        await api.archiveTask(task.id);
        toast.success(`タスク #${task.id} をアーカイブキューに追加しました`);
        setActiveTab("archived");
        lastRef.current = "";
        load();
      } catch (error) {
        toast.error(`アーカイブに失敗しました：${(error as Error).message}`);
        throw error;
      }
    },
    [load],
  );

  // deleteTasks は選択したタスクを 1 件ずつ削除する:バックエンドに一括 API がなく、1 回の削除で
  // 資産/トラフィック/ファイルもまとめてクリーンアップされるため、直列に実行してバックエンドを
  // 一度に叩き潰さないようにする。成功分は選択セットから外し、失敗分は再試行できるよう残す。
  const deleteTasks = React.useCallback(
    async (ids: string[], options: DeleteTaskOptions, onProgress: (done: number) => void) => {
      const total: DeleteCounts = {
        assets_deleted: 0,
        assets_detached: 0,
        traffic_deleted: 0,
        files_deleted: false,
        findings_deleted: 0,
        llm_records_deleted: 0,
      };
      const deleted: string[] = [];
      const failed: { id: string; message: string }[] = [];
      const warnings: string[] = [];

      for (const id of ids) {
        try {
          const r = await api.deleteTask(id, options);
          total.assets_deleted += r.assets_deleted;
          total.assets_detached += r.assets_detached;
          total.traffic_deleted += r.traffic_deleted;
          total.files_deleted = total.files_deleted || r.files_deleted;
          total.findings_deleted += r.findings_deleted;
          total.llm_records_deleted += r.llm_records_deleted;
          if (r.cleanup_warning) warnings.push(`#${id}：${r.cleanup_warning}`);
          deleted.push(id);
        } catch (e) {
          failed.push({ id, message: (e as Error).message });
        }
        onProgress(deleted.length + failed.length);
      }

      if (deleted.length > 0) {
        setSelectedIds((prev) => {
          const next = new Set(prev);
          for (const id of deleted) next.delete(id);
          return next;
        });
        const details = deleteDetails(total);
        const summary =
          `${deleted.length} 件のタスクを削除しました` + (details.length > 0 ? `（${details.join("、")}）` : "");
        if (warnings.length > 0) {
          toast.warning(`${summary}；一部の外部データのクリーンアップが完了していません：${warnings.join("；")}`);
        } else {
          toast.success(summary);
        }
      }
      if (failed.length > 0) {
        const head = failed
          .slice(0, 3)
          .map((f) => `#${f.id}（${f.message}）`)
          .join("；");
        toast.error(`${failed.length} 件のタスクの削除に失敗しました：${head}${failed.length > 3 ? " ほか" : ""}`);
      }
      load();
    },
    [load],
  );

  const selectedTasks = React.useMemo(() => tasks.filter((task) => selectedIds.has(task.id)), [tasks, selectedIds]);
  const pausableTaskIDs = React.useMemo(
    () => selectedTasks.filter((task) => taskControlAction(task.status) === "pause").map((task) => task.id),
    [selectedTasks],
  );
  const resumableTaskIDs = React.useMemo(
    () => selectedTasks.filter((task) => taskControlAction(task.status) === "resume").map((task) => task.id),
    [selectedTasks],
  );
  const archivableTaskIDs = React.useMemo(
    () => selectedTasks.filter((task) => ARCHIVABLE_STATUSES.has(task.status) && !task.queued).map((task) => task.id),
    [selectedTasks],
  );

  const archiveSelectedTasks = React.useCallback(async () => {
    if (archivableTaskIDs.length === 0) return;
    const result = await api.archiveTasks(archivableTaskIDs);
    const succeeded = result.items.filter((item) => item.ok);
    const failed = result.items.filter((item) => !item.ok);
    if (succeeded.length > 0) toast.success(`${succeeded.length} 件のタスクをアーカイブキューに追加しました`);
    if (failed.length > 0) {
      toast.error(
        `${failed.length} 件のタスクをアーカイブできませんでした：${failed
          .slice(0, 3)
          .map((item) => `#${item.id}（${item.error || "状態が変更済み"}）`)
          .join("；")}`,
      );
    }
    setSelectedIds(new Set());
    setActiveTab("archived");
    lastRef.current = "";
    load();
  }, [archivableTaskIDs, load]);

  const controlSelectedTasks = React.useCallback(
    async (action: "pause" | "resume", ids: string[]) => {
      if (ids.length === 0 || batchControlling) return;
      if (ids.length > 100) {
        toast.error("一度に制御できるタスクは最大 100 件です");
        return;
      }
      setBatchControlling(action);
      try {
        const result = await api.controlTasksBatch(ids, action);
        const succeeded = result.items.filter((item) => item.ok);
        const failed = result.items.filter((item) => !item.ok);
        if (succeeded.length > 0) {
          toast.success(
            action === "pause"
              ? `${succeeded.length} 件のタスクを一時停止しました`
              : `${succeeded.length} 件のタスクを再開しました${succeeded.some((item) => item.queued) ? "（一部はキューに追加済み）" : ""}`,
          );
        }
        if (failed.length > 0) {
          const details = failed
            .slice(0, 3)
            .map((item) => `#${item.id}（${item.error || "状態が変更済み"}）`)
            .join("；");
          toast.error(`${failed.length} 件のタスク操作に失敗しました：${details}${failed.length > 3 ? " ほか" : ""}`);
        }
        lastRef.current = "";
        load();
      } catch (error) {
        toast.error(`${action === "pause" ? "一括一時停止" : "一括再開"}に失敗しました：${(error as Error).message}`);
      } finally {
        setBatchControlling(null);
      }
    },
    [batchControlling, load],
  );

  // バックエンドはバッチの分類書き込みを 1 つのトランザクションで行うため、失敗項目は
  // チェック後に削除されたタスクしかあり得ない。
  const moveSelectedTasksCategory = React.useCallback(
    async (categoryID?: number) => {
      const ids = [...selectedIds];
      if (ids.length === 0 || movingCategory) return;
      if (ids.length > 100) {
        toast.error("一度に分類を変更できるタスクは最大 100 件です");
        return;
      }
      setMovingCategory(true);
      try {
        const result = await api.updateTasksCategory(ids, categoryID);
        const succeeded = result.items.filter((item) => item.ok);
        const failed = result.items.filter((item) => !item.ok);
        const target = result.category?.name ?? "未分類";
        if (succeeded.length > 0) {
          toast.success(`${succeeded.length} 件のタスクを「${target}」に移動しました`);
          setSelectedIds(new Set());
        }
        if (failed.length > 0) {
          const details = failed
            .slice(0, 3)
            .map((item) => `#${item.id}（${item.error || "タスクが存在しません"}）`)
            .join("；");
          toast.error(
            `${failed.length} 件のタスクを移動できませんでした：${details}${failed.length > 3 ? " ほか" : ""}`,
          );
        }
        refreshCategoriesAndTasks();
      } catch (error) {
        toast.error(`分類の変更に失敗しました：${(error as Error).message}`);
      } finally {
        setMovingCategory(false);
      }
    },
    [movingCategory, refreshCategoriesAndTasks, selectedIds],
  );

  return (
    <Tabs value={activeTab} onValueChange={setActiveTab} className="gap-4">
      <TabsList className="mx-4 lg:mx-6">
        <TabsTrigger value="current">現在のタスク</TabsTrigger>
        <TabsTrigger value="archived">アーカイブ済み</TabsTrigger>
      </TabsList>
      <TabsContent value="current">
        <Card>
          <CardContent className="flex flex-col gap-4 px-0 pt-6">
            <div className="flex flex-wrap items-center gap-2 px-4 lg:px-6">
              <div className="relative w-full sm:max-w-xs">
                <SearchIcon className="text-muted-foreground pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2" />
                <Input
                  placeholder="説明 / ゴール / ID を検索"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  className="pl-8"
                />
                {query && (
                  <button
                    type="button"
                    onClick={() => setQuery("")}
                    aria-label="検索をクリア"
                    className="text-muted-foreground hover:text-foreground absolute top-1/2 right-2 -translate-y-1/2"
                  >
                    <XIcon className="size-4" />
                  </button>
                )}
              </div>
              <Select value={statusFilter} onValueChange={(v) => setStatusFilter(v as TaskStatus | "all")}>
                <SelectTrigger className="w-36">
                  <SelectValue placeholder="ステータス" />
                </SelectTrigger>
                <SelectContent>
                  <SelectGroup>
                    <SelectItem value="all">すべてのステータス</SelectItem>
                    {STATUS_OPTIONS.map((s) => (
                      <SelectItem key={s.value} value={s.value}>
                        {s.label}
                      </SelectItem>
                    ))}
                  </SelectGroup>
                </SelectContent>
              </Select>
              <Select value={categoryFilter} onValueChange={setCategoryFilter}>
                <SelectTrigger className="w-40">
                  <SelectValue placeholder="タスク分類" />
                </SelectTrigger>
                <SelectContent>
                  <SelectGroup>
                    <SelectItem value="all">すべての分類</SelectItem>
                    <SelectItem value={UNCATEGORIZED_VALUE}>未分類</SelectItem>
                    {categories.map((category) => (
                      <SelectItem key={category.id} value={String(category.id)}>
                        {category.name}
                      </SelectItem>
                    ))}
                  </SelectGroup>
                </SelectContent>
              </Select>
              <span className="text-muted-foreground text-xs tabular-nums">
                {filtered.length}/{tasks.length} 件
              </span>
              {selectedIds.size > 0 && (
                <>
                  <span className="text-xs tabular-nums">{selectedIds.size} 件選択中</span>
                  <Button size="sm" variant="ghost" onClick={() => setSelectedIds(new Set())}>
                    選択を解除
                  </Button>
                  {pausableTaskIDs.length > 0 && (
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={batchControlling !== null}
                      onClick={() => void controlSelectedTasks("pause", pausableTaskIDs)}
                    >
                      {batchControlling === "pause" ? (
                        <Spinner data-icon="inline-start" />
                      ) : (
                        <PauseIcon data-icon="inline-start" />
                      )}
                      一時停止 {pausableTaskIDs.length}
                    </Button>
                  )}
                  {resumableTaskIDs.length > 0 && (
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={batchControlling !== null}
                      onClick={() => void controlSelectedTasks("resume", resumableTaskIDs)}
                    >
                      {batchControlling === "resume" ? (
                        <Spinner data-icon="inline-start" />
                      ) : (
                        <PlayIcon data-icon="inline-start" />
                      )}
                      再開 {resumableTaskIDs.length}
                    </Button>
                  )}
                  {archivableTaskIDs.length > 0 && (
                    <ArchiveConfirmDialog
                      count={archivableTaskIDs.length}
                      onConfirm={archiveSelectedTasks}
                      trigger={
                        <Button size="sm" variant="outline">
                          <ArchiveIcon data-icon="inline-start" />
                          アーカイブ {archivableTaskIDs.length}
                        </Button>
                      }
                    />
                  )}
                  <MoveTasksCategoryDialog
                    categories={categories}
                    count={selectedIds.size}
                    moving={movingCategory}
                    onMove={moveSelectedTasksCategory}
                  />
                  <BulkDeleteTasksDialog ids={[...selectedIds]} onDelete={deleteTasks} />
                </>
              )}
              <ConcurrencySettingsDialog />
              <CategoryManagementSheet
                categories={categories}
                tasks={tasks}
                onChanged={refreshCategoriesAndTasks}
                onTaskMoved={applyTaskCategoryMove}
              />
              <CreateTaskSheet
                tasks={tasks}
                categories={categories}
                onCreated={refreshCategoriesAndTasks}
                onCategoriesChanged={refreshCategoriesAndTasks}
              />
            </div>

            {tasks.length === 0 ? (
              <div className="text-muted-foreground mx-4 flex items-center justify-center rounded-lg border border-dashed py-20 text-sm lg:mx-6">
                タスクがありません。右上の「タスクを作成」から開始してください。
              </div>
            ) : filtered.length === 0 ? (
              <div className="text-muted-foreground mx-4 flex items-center justify-center rounded-lg border border-dashed py-20 text-sm lg:mx-6">
                一致するタスクはありません。
              </div>
            ) : (
              <Table className="**:data-[slot='table-cell']:px-4 **:data-[slot='table-head']:px-4">
                <TableHeader className="[&_tr]:border-t">
                  <TableRow>
                    <TableHead className="w-10">
                      <Checkbox
                        checked={headerChecked}
                        onCheckedChange={(checked) => toggleSelectedPage(checked === true)}
                        aria-label="このページのタスクをすべて選択"
                      />
                    </TableHead>
                    <SortableTaskHead
                      field="id"
                      label="ID"
                      activeField={sortField}
                      direction={sortDirection}
                      className="font-mono"
                      onSort={sortTasksBy}
                    />
                    <TableHead>名前</TableHead>
                    <TableHead>説明</TableHead>
                    <TableHead>ゴール</TableHead>
                    <SortableTaskHead
                      field="status"
                      label="ステータス"
                      activeField={sortField}
                      direction={sortDirection}
                      onSort={sortTasksBy}
                    />
                    <TableHead className="text-center">ゴール進捗</TableHead>
                    <TableHead className="text-center" title="深刻 / 高 / 中 / 低">
                      脆弱性 <span className="text-muted-foreground font-normal">深/高/中/低</span>
                    </TableHead>
                    <TableHead className="text-center">実行中 Worker</TableHead>
                    <SortableTaskHead
                      field="created"
                      label="作成日時"
                      activeField={sortField}
                      direction={sortDirection}
                      align="right"
                      onSort={sortTasksBy}
                    />
                    <SortableTaskHead
                      field="duration"
                      label="実行時間"
                      activeField={sortField}
                      direction={sortDirection}
                      align="right"
                      onSort={sortTasksBy}
                    />
                    <TableHead className="text-right">Token</TableHead>
                    <TableHead className="sticky right-0 z-10 bg-card text-right shadow-[-1px_0_0_0_hsl(var(--border))]">
                      操作
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {paginated.map((task) => (
                    <TaskRow
                      key={task.id}
                      task={task}
                      // running のタスクだけ nowSec を使う。それ以外の行は 0 を渡す。props が変わらなければ
                      // memo が毎秒 tick によるテーブル全体の再描画を抑え、実行中の数行だけが時間を進める。
                      nowSec={task.status === "running" ? nowSec : 0}
                      onDelete={deleteTask}
                      onControl={controlTask}
                      onRename={renameTask}
                      onTogglePinned={toggleTaskPinned}
                      onArchive={queueTaskArchive}
                      selected={selectedIds.has(task.id)}
                      onSelectedChange={toggleSelected}
                    />
                  ))}
                </TableBody>
              </Table>
            )}
            <TablePagination
              page={page}
              pageSize={pageSize}
              total={filtered.length}
              onPageChange={setPage}
              onPageSizeChange={(nextPageSize) => {
                setPageSize(nextPageSize);
                setPage(1);
              }}
            />
          </CardContent>
        </Card>
      </TabsContent>
      <TabsContent value="archived">
        <TaskArchivesPanel onChanged={load} />
      </TabsContent>
    </Tabs>
  );
}

function taskSortIcon(active: boolean, direction: SortDirection) {
  if (!active) {
    return <ArrowUpDownIcon className="size-3.5 opacity-40 transition-opacity group-hover/sort:opacity-100" />;
  }
  if (direction === "asc") return <ArrowUpIcon className="size-3.5" />;
  return <ArrowDownIcon className="size-3.5" />;
}

function SortableTaskHead({
  field,
  label,
  activeField,
  direction,
  align = "left",
  className,
  onSort,
}: {
  field: TaskSortField;
  label: string;
  activeField: TaskSortField;
  direction: SortDirection;
  align?: "left" | "right";
  className?: string;
  onSort: (field: TaskSortField) => void;
}) {
  const active = activeField === field;
  let ariaSort: React.AriaAttributes["aria-sort"] = "none";
  if (active) ariaSort = direction === "asc" ? "ascending" : "descending";

  let actionLabel = `「${label}」の降順で並べ替え`;
  if (active) actionLabel = `「${label}」は現在${direction === "asc" ? "昇順" : "降順"}。クリックで並び順を切り替え`;

  return (
    <TableHead className={className} aria-sort={ariaSort}>
      <button
        type="button"
        className={cn(
          "group/sort inline-flex h-full w-full items-center gap-1 outline-none focus-visible:underline",
          align === "right" && "justify-end",
        )}
        aria-label={actionLabel}
        onClick={() => onSort(field)}
      >
        <span>{label}</span>
        {taskSortIcon(active, direction)}
      </button>
    </TableHead>
  );
}

function ConcurrencySettingsDialog() {
  const [open, setOpen] = React.useState(false);
  const [enabled, setEnabled] = React.useState(false);
  const [limit, setLimit] = React.useState("5");
  const [loading, setLoading] = React.useState(false);
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (!open) return;
    setLoading(true);
    api
      .settings()
      .then((settings) => {
        setEnabled(!!settings.task_concurrency_enabled);
        setLimit(String(settings.task_concurrency_limit ?? 5));
      })
      .catch(() => {
        // 既定値のままダイアログを使えるようにしておく。再度開くとリクエストを再試行する。
      })
      .finally(() => setLoading(false));
  }, [open]);

  async function save() {
    const nextLimit = Math.max(1, Math.floor(Number(limit) || 5));
    setSaving(true);
    try {
      await api.setSettings({ task_concurrency_enabled: enabled, task_concurrency_limit: nextLimit });
      toast.success(
        enabled
          ? `同時実行制限を有効にしました：最大 ${nextLimit} タスクまで同時実行`
          : "タスクの同時実行制限を無効にしました",
      );
      setOpen(false);
    } catch (error) {
      toast.error(`保存に失敗しました：${(error as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm" variant="outline" className="ml-auto" aria-label="タスクの同時実行設定">
          <SlidersHorizontalIcon /> 同時実行設定
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>タスクの同時実行制限</DialogTitle>
          <DialogDescription>
            同時に実行するタスク数を制限します。上限に達すると、新しいタスクは作成順にキューに並び、空きが出ると自動的に開始されます。
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-5 py-2">
          <div className="flex items-center justify-between gap-4">
            <div className="grid gap-1">
              <Label htmlFor="task-concurrency-enabled">タスクの同時実行制限を有効化</Label>
              <span className="text-muted-foreground text-xs">既定では無効</span>
            </div>
            <Switch id="task-concurrency-enabled" checked={enabled} onCheckedChange={setEnabled} disabled={loading} />
          </div>
          {enabled && (
            <div className="grid gap-2">
              <Label htmlFor="task-concurrency-limit">同時実行の上限</Label>
              <Input
                id="task-concurrency-limit"
                type="number"
                min={1}
                className="w-32"
                value={limit}
                onChange={(event) => setLimit(event.target.value)}
                disabled={loading}
              />
            </div>
          )}
        </div>
        <DialogFooter>
          <DialogClose asChild>
            <Button variant="outline">キャンセル</Button>
          </DialogClose>
          <Button onClick={save} disabled={loading || saving}>
            {saving && <Loader2Icon className="animate-spin" />} 保存
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// TaskRow はタスクテーブルの 1 行を描画する。毎秒の「実行時間」tick と
// POLL_MS ごとの一覧更新で、実際にデータが動いた行だけを再描画できるよう memo 化している。
// 1 ページ 20 行 × (StatusBadge + Link + Radix AlertDialog ツリー) は重く、親の再描画ごとに
// 丸ごと作り直すのは現実的でない。
const TaskRow = React.memo(function TaskRow({
  task,
  nowSec,
  onDelete,
  onControl,
  onRename,
  onTogglePinned,
  onArchive,
  selected,
  onSelectedChange,
}: {
  task: Task;
  nowSec: number;
  onDelete: (id: string, options: DeleteTaskOptions) => Promise<void>;
  onControl: (id: string, action: "pause" | "resume") => Promise<void>;
  onRename: (task: Task, name: string) => Promise<void>;
  onTogglePinned: (task: Task) => Promise<void>;
  onArchive: (task: Task) => Promise<void>;
  selected: boolean;
  onSelectedChange: (id: string, checked: boolean) => void;
}) {
  return (
    <TableRow className="group border-border/60" data-state={selected ? "selected" : undefined}>
      <TableCell>
        <Checkbox
          checked={selected}
          onCheckedChange={(checked) => onSelectedChange(task.id, checked === true)}
          aria-label={`タスク ${task.id} を選択`}
        />
      </TableCell>
      <TableCell>
        <code className="bg-muted rounded px-1.5 py-0.5 font-mono text-xs">{task.id}</code>
      </TableCell>
      <TableCell className="font-medium">
        <div className="flex max-w-xs items-center gap-2">
          <TaskNameEditor task={task} onRename={onRename} />
          {task.active && <StarIcon className="size-4 shrink-0 fill-amber-400 text-amber-400" />}
          {taskIsPinned(task) && <PinIcon className="text-primary size-4 shrink-0" aria-label="ピン留め済み" />}
        </div>
      </TableCell>
      <TableCell className="text-muted-foreground max-w-40">
        <Link
          href={`/function/tasks/detail?id=${encodeURIComponent(task.id)}`}
          className="block truncate rounded-sm hover:text-foreground hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          title={task.description}
        >
          {task.description}
        </Link>
      </TableCell>
      <TableCell className="text-muted-foreground max-w-40 truncate" title={task.goal}>
        {task.goal}
      </TableCell>
      <TableCell>
        <StatusBadge domain="task" value={task.status} dot />
      </TableCell>
      <TableCell className="text-muted-foreground text-center text-xs tabular-nums">
        {typeof task.goals_total === "number" && task.goals_total > 0 ? (
          `${task.goals_met}/${task.goals_total}`
        ) : (
          <span className="text-muted-foreground">—</span>
        )}
      </TableCell>
      <TableCell className="text-center text-xs tabular-nums">
        {(() => {
          const f = task.findings;
          const total = f ? f.critical + f.high + f.medium + f.low : 0;
          if (!f || total === 0) return <span className="text-muted-foreground">0</span>;
          const seg = (n: number, cls: string) => <span className={n > 0 ? cls : "text-muted-foreground"}>{n}</span>;
          return (
            <span className="font-medium whitespace-nowrap" title="深刻 / 高 / 中 / 低">
              {seg(f.critical, "text-rose-600 dark:text-rose-400")}
              <span className="text-muted-foreground">/</span>
              {seg(f.high, "text-red-600 dark:text-red-400")}
              <span className="text-muted-foreground">/</span>
              {seg(f.medium, "text-amber-600 dark:text-amber-400")}
              <span className="text-muted-foreground">/</span>
              {seg(f.low, "text-slate-600 dark:text-slate-400")}
            </span>
          );
        })()}
      </TableCell>
      <TableCell className="text-center text-xs tabular-nums">
        {task.in_flight && task.in_flight > 0 ? (
          <span className="text-foreground font-medium">{task.in_flight}</span>
        ) : (
          <span className="text-muted-foreground">0</span>
        )}
      </TableCell>
      <TableCell className="text-muted-foreground text-right text-xs whitespace-nowrap tabular-nums">
        {fmtDateTime(task.created_unix)}
      </TableCell>
      <TableCell className="text-right text-xs whitespace-nowrap tabular-nums">
        {(() => {
          const secs = taskDuration(task, nowSec);
          if (secs <= 0) return <span className="text-muted-foreground">—</span>;
          return (
            <span className={task.status === "running" ? "text-foreground" : "text-muted-foreground"}>
              {fmtDuration(secs)}
            </span>
          );
        })()}
      </TableCell>
      <TableCell
        className="text-right text-xs whitespace-nowrap tabular-nums"
        title={
          task.tokens
            ? `入力 ${task.tokens.input_tokens} · キャッシュ ${task.tokens.cache_read_tokens} · 出力 ${task.tokens.output_tokens}`
            : undefined
        }
      >
        {task.tokens ? (
          <span className="text-muted-foreground">
            入 <span className="text-foreground">{fmtTokens(task.tokens.input_tokens)}</span>
            {" · キ "}
            <span className="text-foreground">{fmtTokens(task.tokens.cache_read_tokens)}</span>
            {" · 出 "}
            <span className="text-foreground">{fmtTokens(task.tokens.output_tokens)}</span>
          </span>
        ) : (
          "—"
        )}
      </TableCell>
      <TableCell className="sticky right-0 z-10 bg-card text-right shadow-[-1px_0_0_0_hsl(var(--border))] group-hover:bg-muted/50">
        <div className="flex items-center justify-end gap-0.5">
          <Button size="icon" variant="ghost" asChild aria-label="タスク詳細を表示" title="タスク詳細を表示">
            <Link href={`/function/tasks/detail?id=${encodeURIComponent(task.id)}`}>
              <EyeIcon />
            </Link>
          </Button>
          <TaskControlButton task={task} onControl={onControl} />
          <TaskPinAction task={task} onTogglePinned={onTogglePinned} />
          <TaskArchiveAction task={task} onArchive={onArchive} />
          <DeleteTaskDialog task={task} onDelete={onDelete} />
        </div>
      </TableCell>
    </TableRow>
  );
});

function TaskNameEditor({ task, onRename }: { task: Task; onRename: (task: Task, name: string) => Promise<void> }) {
  const [editing, setEditing] = React.useState(false);
  const [name, setName] = React.useState(task.name ?? "");
  const [saving, setSaving] = React.useState(false);
  const inputRef = React.useRef<HTMLInputElement>(null);
  const cancelOnBlurRef = React.useRef(false);

  React.useEffect(() => {
    if (!editing) setName(task.name ?? "");
  }, [editing, task.name]);

  async function finishEditing() {
    if (cancelOnBlurRef.current) {
      cancelOnBlurRef.current = false;
      setName(task.name ?? "");
      setEditing(false);
      return;
    }

    const next = name.trim();
    if (!next || next === task.name?.trim()) {
      setName(task.name ?? "");
      setEditing(false);
      return;
    }

    setSaving(true);
    try {
      await onRename(task, next);
      setEditing(false);
    } catch {
      requestAnimationFrame(() => inputRef.current?.focus());
    } finally {
      setSaving(false);
    }
  }

  if (editing) {
    return (
      <div className="flex min-w-0 items-center gap-1">
        <Input
          ref={inputRef}
          value={name}
          maxLength={200}
          autoFocus
          disabled={saving}
          aria-label={`タスク #${task.id} の名前`}
          className="h-7 min-w-28 max-w-48 px-2 font-medium"
          onFocus={(event) => event.currentTarget.select()}
          onChange={(event) => setName(event.target.value)}
          onBlur={() => void finishEditing()}
          onKeyDown={(event) => {
            if (event.key === "Enter") {
              event.preventDefault();
              event.currentTarget.blur();
            } else if (event.key === "Escape") {
              event.preventDefault();
              cancelOnBlurRef.current = true;
              event.currentTarget.blur();
            }
          }}
        />
        {saving && <Spinner className="shrink-0" />}
      </div>
    );
  }

  return (
    <div className="flex min-w-0 items-center gap-1">
      <Link
        href={`/function/tasks/detail?id=${encodeURIComponent(task.id)}`}
        className="min-w-0 truncate rounded-sm hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        title={task.name?.trim() ? task.name : task.description}
      >
        {task.name?.trim() ? task.name : <span className="text-muted-foreground">無題</span>}
      </Link>
      <Button
        type="button"
        variant="ghost"
        size="icon-xs"
        className="shrink-0 opacity-100 transition-opacity [@media(hover:hover)]:opacity-0 [@media(hover:hover)]:group-focus-within:opacity-100 [@media(hover:hover)]:group-hover:opacity-100"
        aria-label={`タスク #${task.id} の名前を変更`}
        title="名前を変更"
        onClick={() => {
          setName(task.name ?? "");
          setEditing(true);
        }}
      >
        <PencilIcon />
      </Button>
    </div>
  );
}

function taskPinIcon(pinning: boolean, pinned: boolean) {
  if (pinning) return <Spinner />;
  if (pinned) return <PinOffIcon />;
  return <PinIcon />;
}

function TaskPinAction({ task, onTogglePinned }: { task: Task; onTogglePinned: (task: Task) => Promise<void> }) {
  const [pinning, setPinning] = React.useState(false);
  const pinned = taskIsPinned(task);

  return (
    <Button
      type="button"
      variant="ghost"
      size="icon"
      disabled={pinning}
      aria-label={pinned ? `タスク #${task.id} のピン留めを解除` : `タスク #${task.id} をピン留め`}
      title={pinned ? "ピン留めを解除" : "ピン留め"}
      onClick={async () => {
        setPinning(true);
        try {
          await onTogglePinned(task);
        } finally {
          setPinning(false);
        }
      }}
    >
      {taskPinIcon(pinning, pinned)}
    </Button>
  );
}

// 三態アイコンはネストした三項演算子ではなく個別に書く:リポジトリの Biome ベースラインが
// noNestedTernary を禁止しているため。
function taskControlIcon(pending: boolean, action: "pause" | "resume" | null) {
  if (pending) return <Loader2Icon className="animate-spin" />;
  if (action === "resume") return <PlayIcon />;
  return <PauseIcon />;
}

// TaskControlButton は行内の一時停止/再開スイッチ。終端状態のタスクは非表示ではなく disabled で描画し、
// 各行の操作列の幅を揃えてボタン位置が状態で動かないようにする。
function TaskControlButton({
  task,
  onControl,
}: {
  task: Task;
  onControl: (id: string, action: "pause" | "resume") => Promise<void>;
}) {
  const [pending, setPending] = React.useState(false);
  const action = taskControlAction(task.status);
  const label = action === "resume" ? "タスクを再開" : "タスクを一時停止";
  return (
    <Button
      size="icon"
      variant="ghost"
      aria-label={label}
      title={action ? label : "この状態では一時停止も再開もできません"}
      disabled={!action || pending}
      onClick={async () => {
        if (!action) return;
        setPending(true);
        try {
          await onControl(task.id, action);
        } finally {
          setPending(false);
        }
      }}
    >
      {taskControlIcon(pending, action)}
    </Button>
  );
}

function archiveBlockReason(task: Task): string {
  if (task.queued) return "キューのタスクは先に一時停止する必要があります";
  if (!ARCHIVABLE_STATUSES.has(task.status)) return "実行中または未終了のタスクは先に一時停止する必要があります";
  if (task.archive_blocked_by_task_id) {
    return `未アーカイブのタスク #${task.archive_blocked_by_task_id} に直接継承されているため、先に依存タスクをアーカイブしてください`;
  }
  return "";
}

function ArchiveConfirmDialog({
  count,
  trigger,
  onConfirm,
}: {
  count: number;
  trigger: React.ReactNode;
  onConfirm: () => Promise<void>;
}) {
  const [open, setOpen] = React.useState(false);
  const [pending, setPending] = React.useState(false);
  return (
    <AlertDialog open={open} onOpenChange={(next) => !pending && setOpen(next)}>
      <AlertDialogTrigger asChild>{trigger}</AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{count === 1 ? "タスクをアーカイブ" : `${count} 件のタスクをアーカイブ`}</AlertDialogTitle>
          <AlertDialogDescription className="[overflow-wrap:anywhere]">
            アーカイブするとタスクのスケジューリングが停止し、グラフ、LLM
            履歴、ファイル、および専有資産とトラフィックがローカルのコールドストレージに圧縮されます。完了後は「アーカイブ済み」から復元できます。
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={pending}>キャンセル</AlertDialogCancel>
          <AlertDialogAction
            disabled={pending}
            onClick={async (event) => {
              event.preventDefault();
              setPending(true);
              try {
                await onConfirm();
                setOpen(false);
              } finally {
                setPending(false);
              }
            }}
          >
            {pending && <Spinner data-icon="inline-start" />}
            アーカイブを確認
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

function TaskArchiveAction({ task, onArchive }: { task: Task; onArchive: (task: Task) => Promise<void> }) {
  const reason = archiveBlockReason(task);
  const trigger = (
    <Button size="icon" variant="ghost" disabled={Boolean(reason)} aria-label={`タスク #${task.id} をアーカイブ`}>
      <ArchiveIcon />
    </Button>
  );
  if (reason) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="inline-flex">{trigger}</span>
        </TooltipTrigger>
        <TooltipContent>{reason}</TooltipContent>
      </Tooltip>
    );
  }
  return <ArchiveConfirmDialog count={1} trigger={trigger} onConfirm={() => onArchive(task)} />;
}

const ARCHIVE_PROCESSING_STATES = new Set<TaskArchiveState>([
  "archive_queued",
  "archiving",
  "restore_queued",
  "restoring",
  "delete_queued",
  "deleting",
]);

const ARCHIVE_FAILED_STATES = new Set<TaskArchiveState>(["archive_failed", "restore_failed", "delete_failed"]);

function archiveStateLabel(state: TaskArchiveState): string {
  const labels: Record<TaskArchiveState, string> = {
    archive_queued: "アーカイブ待ち",
    archiving: "アーカイブ中",
    archive_failed: "アーカイブ失敗",
    ready: "復元可能",
    restore_queued: "復元待ち",
    restoring: "復元中",
    restore_failed: "復元失敗",
    delete_queued: "削除待ち",
    deleting: "削除中",
    delete_failed: "削除失敗",
  };
  return labels[state];
}

function ArchiveStateBadge({ state }: { state: TaskArchiveState }) {
  let variant: "default" | "secondary" | "destructive" | "outline" = "outline";
  if (state === "ready") variant = "secondary";
  if (ARCHIVE_PROCESSING_STATES.has(state)) variant = "default";
  if (ARCHIVE_FAILED_STATES.has(state)) variant = "destructive";
  return <Badge variant={variant}>{archiveStateLabel(state)}</Badge>;
}

function formatArchiveBytes(bytes: number): string {
  if (bytes <= 0) return "—";
  if (bytes >= 1024 ** 3) return `${(bytes / 1024 ** 3).toFixed(1)} GB`;
  if (bytes >= 1024 ** 2) return `${(bytes / 1024 ** 2).toFixed(1)} MB`;
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${bytes} B`;
}

function archiveCompressionLabel(archive: TaskArchive): string {
  if (archive.original_size <= 0 || archive.compressed_size <= 0) return "—";
  const saved = Math.max(0, 100 - (archive.compressed_size / archive.original_size) * 100);
  return `${formatArchiveBytes(archive.compressed_size)} · ${saved.toFixed(0)}% 節約`;
}

function archiveDataTotal(archive: TaskArchive): number {
  return Object.values(archive.data_counts).reduce((sum, value) => sum + (Number(value) || 0), 0);
}

function archiveDate(value?: string): string {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleString();
}

function ArchiveDeleteDialog({
  archives,
  onConfirm,
  trigger,
}: {
  archives: TaskArchive[];
  onConfirm: () => Promise<void>;
  trigger: React.ReactNode;
}) {
  const [open, setOpen] = React.useState(false);
  const [pending, setPending] = React.useState(false);
  const rows = archives.reduce((sum, archive) => sum + archiveDataTotal(archive), 0);
  const bytes = archives.reduce((sum, archive) => sum + archive.compressed_size, 0);
  return (
    <AlertDialog open={open} onOpenChange={(next) => !pending && setOpen(next)}>
      <AlertDialogTrigger asChild>{trigger}</AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{archives.length} 件のタスクアーカイブを完全削除しますか？</AlertDialogTitle>
          <AlertDialogDescription className="[overflow-wrap:anywhere]">
            約 {formatArchiveBytes(bytes)} のアーカイブパッケージと {rows.toLocaleString()}{" "}
            件の関連データスナップショットを完全に削除します。この操作は元に戻せません。
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={pending}>キャンセル</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={pending}
            onClick={async (event) => {
              event.preventDefault();
              setPending(true);
              try {
                await onConfirm();
                setOpen(false);
              } finally {
                setPending(false);
              }
            }}
          >
            {pending && <Spinner data-icon="inline-start" />}
            完全削除
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

function TaskArchivesPanel({ onChanged }: { onChanged: () => void }) {
  const [archives, setArchives] = React.useState<TaskArchive[]>([]);
  const [query, setQuery] = React.useState("");
  const [stateFilter, setStateFilter] = React.useState("all");
  const [page, setPage] = React.useState(1);
  const [pageSize, setPageSize] = React.useState(20);
  const [total, setTotal] = React.useState(0);
  const [selected, setSelected] = React.useState<Set<number>>(() => new Set());
  const [loading, setLoading] = React.useState(true);
  const pendingRestoreIDs = React.useRef(new Set<number>());

  const load = React.useCallback(async () => {
    try {
      const result = await api.taskArchives({
        page,
        size: pageSize,
        q: query.trim(),
        state: stateFilter === "all" ? undefined : stateFilter,
      });
      setArchives(result.items);
      setTotal(result.total);
      setSelected((current) => {
        const visible = new Set(result.items.map((archive) => archive.id));
        return new Set([...current].filter((id) => visible.has(id)));
      });
      const pending = [...pendingRestoreIDs.current];
      if (pending.length > 0) {
        const states = await Promise.allSettled(pending.map((id) => api.taskArchive(id)));
        let restored = false;
        states.forEach((state, index) => {
          if (state.status !== "rejected" || !(state.reason instanceof Error)) return;
          // 互換性のため中国語のまま: バックエンド (server/task_archives.go) が返す
          // 「归档不存在」というエラーメッセージと照合するため、文字列を変更してはならない。
          if (!state.reason.message.includes("归档不存在")) return;
          pendingRestoreIDs.current.delete(pending[index]);
          restored = true;
        });
        if (restored) onChanged();
      }
    } catch (error) {
      toast.error(`アーカイブ一覧の読み込みに失敗しました：${(error as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, [onChanged, page, pageSize, query, stateFilter]);

  React.useEffect(() => {
    void load();
    const timer = setInterval(() => void load(), 2_000);
    return () => clearInterval(timer);
  }, [load]);

  const selectable = React.useMemo(
    () => archives.filter((archive) => !ARCHIVE_PROCESSING_STATES.has(archive.state)),
    [archives],
  );
  const selectedArchives = React.useMemo(
    () => archives.filter((archive) => selected.has(archive.id)),
    [archives, selected],
  );
  const restorable = selectedArchives.filter(
    (archive) => archive.state === "ready" || archive.state === "restore_failed",
  );
  const deletable = selectedArchives.filter(
    (archive) => archive.state === "ready" || archive.state === "delete_failed",
  );
  const selectedAll = selectable.length > 0 && selectable.every((archive) => selected.has(archive.id));
  const selectedSome = selectable.some((archive) => selected.has(archive.id));

  const afterAction = React.useCallback(() => {
    setSelected(new Set());
    void load();
    onChanged();
  }, [load, onChanged]);

  async function restoreMany(items: TaskArchive[]) {
    try {
      const result =
        items.length === 1
          ? {
              items: [
                {
                  id: String(items[0].id),
                  ok: true,
                  queued: true,
                  archive_id: (await api.restoreTaskArchive(items[0].id)).id,
                },
              ],
            }
          : await api.restoreTaskArchives(items.map((archive) => archive.id));
      const succeeded = result.items.filter((item) => item.ok).length;
      const failed = result.items.length - succeeded;
      for (const item of result.items) {
        const archiveID = Number(item.archive_id ?? item.id);
        if (item.ok && Number.isSafeInteger(archiveID) && archiveID > 0) pendingRestoreIDs.current.add(archiveID);
      }
      if (succeeded > 0) toast.success(`${succeeded} 件のタスクを復元キューに追加しました`);
      if (failed > 0) toast.error(`${failed} 件のタスクを復元できません`);
      afterAction();
    } catch (error) {
      toast.error(`復元に失敗しました：${(error as Error).message}`);
    }
  }

  async function deleteMany(items: TaskArchive[]) {
    try {
      const result =
        items.length === 1
          ? {
              items: [
                {
                  id: String(items[0].id),
                  ok: true,
                  queued: true,
                  archive_id: (await api.deleteTaskArchive(items[0].id)).id,
                },
              ],
            }
          : await api.deleteTaskArchives(items.map((archive) => archive.id));
      const succeeded = result.items.filter((item) => item.ok).length;
      const failed = result.items.length - succeeded;
      if (succeeded > 0) toast.success(`${succeeded} 件のアーカイブを完全削除キューに追加しました`);
      if (failed > 0) toast.error(`${failed} 件のアーカイブを削除できません`);
      afterAction();
    } catch (error) {
      toast.error(`完全削除に失敗しました：${(error as Error).message}`);
      throw error;
    }
  }

  async function retry(archive: TaskArchive) {
    try {
      if (archive.state === "archive_failed") await api.archiveTask(String(archive.task_id));
      if (archive.state === "restore_failed") await api.restoreTaskArchive(archive.id);
      if (archive.state === "delete_failed") await api.deleteTaskArchive(archive.id);
      toast.success("処理キューに再追加しました");
      afterAction();
    } catch (error) {
      toast.error(`再試行に失敗しました：${(error as Error).message}`);
    }
  }

  return (
    <Card>
      <CardContent className="flex flex-col gap-4 px-0 pt-6">
        <div className="flex flex-wrap items-center gap-2 px-4 lg:px-6">
          <div className="relative w-full sm:max-w-xs">
            <SearchIcon className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              value={query}
              onChange={(event) => {
                setQuery(event.target.value);
                setPage(1);
              }}
              placeholder="タスク名 / 説明 / ID を検索"
              className="pl-8"
            />
          </div>
          <Select
            value={stateFilter}
            onValueChange={(value) => {
              setStateFilter(value);
              setPage(1);
            }}
          >
            <SelectTrigger className="w-36">
              <SelectValue placeholder="処理ステータス" />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value="all">すべてのステータス</SelectItem>
                <SelectItem value="archive_queued">アーカイブ待ち</SelectItem>
                <SelectItem value="archiving">アーカイブ中</SelectItem>
                <SelectItem value="ready">復元可能</SelectItem>
                <SelectItem value="restore_queued">復元待ち</SelectItem>
                <SelectItem value="restoring">復元中</SelectItem>
                <SelectItem value="delete_queued">削除待ち</SelectItem>
                <SelectItem value="deleting">削除中</SelectItem>
                <SelectItem value="archive_failed">アーカイブ失敗</SelectItem>
                <SelectItem value="restore_failed">復元失敗</SelectItem>
                <SelectItem value="delete_failed">削除失敗</SelectItem>
              </SelectGroup>
            </SelectContent>
          </Select>
          <span className="text-muted-foreground text-xs tabular-nums">{total} 件のアーカイブ</span>
          {selectedArchives.length > 0 && (
            <>
              <span className="text-xs tabular-nums">{selectedArchives.length} 件選択中</span>
              {restorable.length > 0 && (
                <Button size="sm" variant="outline" onClick={() => void restoreMany(restorable)}>
                  <Undo2Icon data-icon="inline-start" />
                  復元 {restorable.length}
                </Button>
              )}
              {deletable.length > 0 && (
                <ArchiveDeleteDialog
                  archives={deletable}
                  onConfirm={() => deleteMany(deletable)}
                  trigger={
                    <Button size="sm" variant="destructive">
                      <Trash2Icon data-icon="inline-start" />
                      完全削除 {deletable.length}
                    </Button>
                  }
                />
              )}
            </>
          )}
        </div>
        {loading ? (
          <div className="flex justify-center py-20">
            <Spinner />
          </div>
        ) : archives.length === 0 ? (
          <Empty className="mx-4 border border-dashed lg:mx-6">
            <EmptyHeader>
              <EmptyTitle>タスクアーカイブはありません</EmptyTitle>
              <EmptyDescription>
                一時停止中または終端状態のタスクは現在のタスク一覧からアーカイブできます。
              </EmptyDescription>
            </EmptyHeader>
          </Empty>
        ) : (
          <Table className="**:data-[slot='table-cell']:px-4 **:data-[slot='table-head']:px-4">
            <TableHeader className="[&_tr]:border-t">
              <TableRow>
                <TableHead className="w-10">
                  <Checkbox
                    checked={selectedAll || (selectedSome ? "indeterminate" : false)}
                    onCheckedChange={(checked) =>
                      setSelected(checked === true ? new Set(selectable.map((archive) => archive.id)) : new Set())
                    }
                    aria-label="現在のページのアーカイブを選択"
                  />
                </TableHead>
                <TableHead>タスク</TableHead>
                <TableHead>元のステータス</TableHead>
                <TableHead>分類</TableHead>
                <TableHead>アーカイブ日時</TableHead>
                <TableHead>圧縮サイズ</TableHead>
                <TableHead>データ量</TableHead>
                <TableHead className="min-w-44">処理ステータス</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {archives.map((archive) => {
                const processing = ARCHIVE_PROCESSING_STATES.has(archive.state);
                const canRestore = archive.state === "ready" || archive.state === "restore_failed";
                const canDelete = archive.state === "ready" || archive.state === "delete_failed";
                return (
                  <TableRow key={archive.id} data-state={selected.has(archive.id) ? "selected" : undefined}>
                    <TableCell>
                      <Checkbox
                        checked={selected.has(archive.id)}
                        disabled={processing}
                        onCheckedChange={(checked) =>
                          setSelected((current) => {
                            const next = new Set(current);
                            if (checked === true) next.add(archive.id);
                            else next.delete(archive.id);
                            return next;
                          })
                        }
                        aria-label={`タスクアーカイブ #${archive.task_id} を選択`}
                      />
                    </TableCell>
                    <TableCell className="max-w-sm">
                      <div className="flex min-w-0 flex-col gap-0.5">
                        <span className="truncate font-medium">
                          {archive.task_name || archive.task_description || `タスク #${archive.task_id}`}
                        </span>
                        <span className="text-muted-foreground truncate text-xs">
                          #{archive.task_id} · {archive.task_description}
                        </span>
                      </div>
                    </TableCell>
                    <TableCell>
                      <StatusBadge domain="task" value={archive.original_status} dot />
                    </TableCell>
                    <TableCell className="text-muted-foreground">{archive.category_name || "未分類"}</TableCell>
                    <TableCell className="text-muted-foreground whitespace-nowrap text-xs">
                      {archiveDate(archive.archived_at || archive.requested_at)}
                    </TableCell>
                    <TableCell
                      className="whitespace-nowrap text-xs"
                      title={`圧縮前 ${formatArchiveBytes(archive.original_size)}`}
                    >
                      {archiveCompressionLabel(archive)}
                    </TableCell>
                    <TableCell className="text-xs tabular-nums">{archiveDataTotal(archive).toLocaleString()}</TableCell>
                    <TableCell>
                      <div className="flex min-w-0 flex-col gap-1.5">
                        <ArchiveStateBadge state={archive.state} />
                        {processing && <Progress value={archive.progress} />}
                        {archive.error && (
                          <p className="text-destructive text-xs [overflow-wrap:anywhere]">{archive.error}</p>
                        )}
                        {[...new Set(archive.warnings ?? [])].map((warning) => (
                          <p
                            key={warning}
                            className="text-amber-700 text-xs [overflow-wrap:anywhere] dark:text-amber-400"
                          >
                            {warning}
                          </p>
                        ))}
                        <span className="text-muted-foreground text-xs">
                          {archive.phase} · {archive.progress}%
                        </span>
                      </div>
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-0.5">
                        {ARCHIVE_FAILED_STATES.has(archive.state) && (
                          <Button
                            size="icon"
                            variant="ghost"
                            onClick={() => void retry(archive)}
                            aria-label="アーカイブ操作を再試行"
                            title="再試行"
                          >
                            <Undo2Icon />
                          </Button>
                        )}
                        {canRestore && (
                          <Button
                            size="icon"
                            variant="ghost"
                            onClick={() => void restoreMany([archive])}
                            aria-label="タスクを復元"
                            title="タスクを復元"
                          >
                            <Undo2Icon />
                          </Button>
                        )}
                        {canDelete && (
                          <ArchiveDeleteDialog
                            archives={[archive]}
                            onConfirm={() => deleteMany([archive])}
                            trigger={
                              <Button size="icon" variant="ghost" aria-label="アーカイブを完全削除" title="完全削除">
                                <Trash2Icon />
                              </Button>
                            }
                          />
                        )}
                      </div>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
        <TablePagination
          page={page}
          pageSize={pageSize}
          total={total}
          onPageChange={setPage}
          onPageSizeChange={(size) => {
            setPageSize(size);
            setPage(1);
          }}
        />
      </CardContent>
    </Card>
  );
}

const emptyDeleteOptions = (): DeleteTaskOptions => ({
  delete_assets: false,
  delete_traffic: false,
  delete_files: false,
  delete_findings: false,
  delete_llm_records: false,
});

const deleteOptionKeys: (keyof DeleteTaskOptions)[] = [
  "delete_assets",
  "delete_traffic",
  "delete_files",
  "delete_findings",
  "delete_llm_records",
];

// DeleteOptionFields は単体削除と一括削除のダイアログが共有する「関連データも同時に削除」チェックボックス
// ブロックを描画する。idPrefix は複数のダイアログが同じテーブル内に共存しても label/input の id が
// 重複しないようにするため。
function DeleteOptionFields({
  idPrefix,
  options,
  onOptionsChange,
  disabled,
}: {
  idPrefix: string;
  options: DeleteTaskOptions;
  onOptionsChange: React.Dispatch<React.SetStateAction<DeleteTaskOptions>>;
  disabled: boolean;
}) {
  const selectedCount = deleteOptionKeys.filter((key) => options[key]).length;
  let allChecked: boolean | "indeterminate" = false;
  if (selectedCount === deleteOptionKeys.length) {
    allChecked = true;
  } else if (selectedCount > 0) {
    allChecked = "indeterminate";
  }

  const updateOption = (key: keyof DeleteTaskOptions, checked: boolean) => {
    onOptionsChange((current) => ({ ...current, [key]: checked }));
  };

  const updateAllOptions = (checked: boolean) => {
    onOptionsChange({
      delete_assets: checked,
      delete_traffic: checked,
      delete_files: checked,
      delete_findings: checked,
      delete_llm_records: checked,
    });
  };

  return (
    <FieldSet disabled={disabled}>
      <FieldLegend variant="label">関連データも同時に削除</FieldLegend>
      <FieldGroup className="gap-3">
        <Field orientation="horizontal">
          <Checkbox
            id={`delete-all-${idPrefix}`}
            checked={allChecked}
            onCheckedChange={(checked) => updateAllOptions(checked === true)}
          />
          <FieldContent>
            <FieldLabel htmlFor={`delete-all-${idPrefix}`}>すべて削除</FieldLabel>
            <FieldDescription>
              以下すべての関連データを選択します。資産、トラフィック、ファイル、脆弱性、LLM
              リクエスト/レスポンス記録を含みます。
            </FieldDescription>
          </FieldContent>
        </Field>
        <Field orientation="horizontal">
          <Checkbox
            id={`delete-assets-${idPrefix}`}
            checked={options.delete_assets}
            onCheckedChange={(checked) => updateOption("delete_assets", checked === true)}
          />
          <FieldContent>
            <FieldLabel htmlFor={`delete-assets-${idPrefix}`}>関連資産</FieldLabel>
            <FieldDescription>
              このタスクにのみ属する資産を削除します。共有資産は現在のタスクとの関連付けだけを解除します。
            </FieldDescription>
          </FieldContent>
        </Field>
        <Field orientation="horizontal">
          <Checkbox
            id={`delete-traffic-${idPrefix}`}
            checked={options.delete_traffic}
            onCheckedChange={(checked) => updateOption("delete_traffic", checked === true)}
          />
          <FieldContent>
            <FieldLabel htmlFor={`delete-traffic-${idPrefix}`}>関連トラフィック</FieldLabel>
            <FieldDescription>
              関連資産の正確なホスト名で削除します。他のタスクからまだ参照されている共有ホストのトラフィックは保持されます。
            </FieldDescription>
          </FieldContent>
        </Field>
        <Field orientation="horizontal">
          <Checkbox
            id={`delete-files-${idPrefix}`}
            checked={options.delete_files}
            onCheckedChange={(checked) => updateOption("delete_files", checked === true)}
          />
          <FieldContent>
            <FieldLabel htmlFor={`delete-files-${idPrefix}`}>タスクファイル</FieldLabel>
            <FieldDescription>
              このタスクの作業ディレクトリにあるアップロードファイル、コマンド出力、その他の成果物を削除します。
            </FieldDescription>
          </FieldContent>
        </Field>
        <Field orientation="horizontal">
          <Checkbox
            id={`delete-findings-${idPrefix}`}
            checked={options.delete_findings}
            onCheckedChange={(checked) => updateOption("delete_findings", checked === true)}
          />
          <FieldContent>
            <FieldLabel htmlFor={`delete-findings-${idPrefix}`}>関連脆弱性</FieldLabel>
            <FieldDescription>
              このタスクが生成した独立した脆弱性レコードとレポートを完全に削除します。
            </FieldDescription>
          </FieldContent>
        </Field>
        <Field orientation="horizontal">
          <Checkbox
            id={`delete-llm-records-${idPrefix}`}
            checked={options.delete_llm_records}
            onCheckedChange={(checked) => updateOption("delete_llm_records", checked === true)}
          />
          <FieldContent>
            <FieldLabel htmlFor={`delete-llm-records-${idPrefix}`}>LLM リクエスト/レスポンス記録</FieldLabel>
            <FieldDescription>
              このタスクが記録した LLM リクエスト、レスポンス、Token、エラー詳細を完全に削除します。
            </FieldDescription>
          </FieldContent>
        </Field>
      </FieldGroup>
    </FieldSet>
  );
}

function DeleteTaskDialog({
  task,
  onDelete,
}: {
  task: Task;
  onDelete: (id: string, options: DeleteTaskOptions) => Promise<void>;
}) {
  const [open, setOpen] = React.useState(false);
  const [deleting, setDeleting] = React.useState(false);
  const [options, setOptions] = React.useState<DeleteTaskOptions>(emptyDeleteOptions);

  const handleOpenChange = (next: boolean) => {
    if (deleting) return;
    setOpen(next);
    if (next) setOptions(emptyDeleteOptions());
  };

  const handleDelete = async () => {
    setDeleting(true);
    try {
      await onDelete(task.id, options);
      setOpen(false);
    } finally {
      setDeleting(false);
    }
  };

  return (
    <AlertDialog open={open} onOpenChange={handleOpenChange}>
      <AlertDialogTrigger asChild>
        <Button size="icon" variant="outline" aria-label="タスクを削除">
          <Trash2Icon className="text-destructive" />
        </Button>
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>タスク #{task.id} を削除しますか？</AlertDialogTitle>
          <AlertDialogDescription className="break-words">
            {task.description ? (
              <>
                「
                <span className="break-all">
                  {task.description.length > 80 ? `${task.description.slice(0, 80)}…` : task.description}
                </span>
                」
              </>
            ) : (
              "このタスク"
            )}
            の実行記録と探索チェーンが完全に削除されます。
          </AlertDialogDescription>
        </AlertDialogHeader>
        <DeleteOptionFields idPrefix={task.id} options={options} onOptionsChange={setOptions} disabled={deleting} />
        <AlertDialogFooter>
          <AlertDialogCancel disabled={deleting}>キャンセル</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={deleting}
            onClick={(event) => {
              event.preventDefault();
              void handleDelete();
            }}
          >
            {deleting && <Spinner data-icon="inline-start" />}
            {deleting ? "削除中" : "削除"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

// BulkDeleteTasksDialog はチェックしたタスクをすべて、共通のクリーンアップオプションで削除する。
// バックエンドに一括 API がないため削除は 1 件ずつ実行し(deleteTasks 参照)、
// ボタンには進捗をライブ表示する。
// MoveTasksCategoryDialog は適用前に対象を確認するので、大きな選択を誤クリックしても
// 全タスクが黙って移動することはない。UNCATEGORIZED_VALUE は「分類なし」の
// 代替値で、Select が空文字の value を受け付けないため使う。
function MoveTasksCategoryDialog({
  categories,
  count,
  moving,
  onMove,
}: {
  categories: TaskCategory[];
  count: number;
  moving: boolean;
  onMove: (categoryID?: number) => Promise<void>;
}) {
  const [open, setOpen] = React.useState(false);
  const [target, setTarget] = React.useState(UNCATEGORIZED_VALUE);

  const handleOpenChange = (next: boolean) => {
    if (moving) return;
    setOpen(next);
    if (next) setTarget(UNCATEGORIZED_VALUE);
  };

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger asChild>
        <Button size="sm" variant="outline">
          <FolderInputIcon data-icon="inline-start" /> 分類を変更
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>選択した {count} 件のタスクの分類を変更</DialogTitle>
          <DialogDescription>
            対象の分類は選択したタスクに一括で適用されます。「未分類」を選ぶと現在の分類から外れます。
          </DialogDescription>
        </DialogHeader>
        <Field>
          <FieldLabel htmlFor="bulk-category">移動先の分類</FieldLabel>
          <Select value={target} onValueChange={setTarget} disabled={moving}>
            <SelectTrigger id="bulk-category" className="w-full">
              <SelectValue placeholder="分類を選択" />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value={UNCATEGORIZED_VALUE}>未分類</SelectItem>
                {categories.map((category) => (
                  <SelectItem key={category.id} value={String(category.id)}>
                    {category.name}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
          {categories.length === 0 && (
            <FieldDescription>分類がまだありません。「分類管理」から先に作成してください。</FieldDescription>
          )}
        </Field>
        <DialogFooter>
          <DialogClose asChild>
            <Button variant="outline" disabled={moving}>
              キャンセル
            </Button>
          </DialogClose>
          <Button
            disabled={moving}
            onClick={() => {
              void onMove(target === UNCATEGORIZED_VALUE ? undefined : Number(target)).then(() => setOpen(false));
            }}
          >
            {moving && <Spinner data-icon="inline-start" />}
            移動
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function BulkDeleteTasksDialog({
  ids,
  onDelete,
}: {
  ids: string[];
  onDelete: (ids: string[], options: DeleteTaskOptions, onProgress: (done: number) => void) => Promise<void>;
}) {
  const [open, setOpen] = React.useState(false);
  const [deleting, setDeleting] = React.useState(false);
  const [done, setDone] = React.useState(0);
  const [options, setOptions] = React.useState<DeleteTaskOptions>(emptyDeleteOptions);

  const handleOpenChange = (next: boolean) => {
    if (deleting) return;
    setOpen(next);
    if (next) {
      setOptions(emptyDeleteOptions());
      setDone(0);
    }
  };

  const handleDelete = async () => {
    setDeleting(true);
    setDone(0);
    try {
      await onDelete(ids, options, setDone);
      setOpen(false);
    } finally {
      setDeleting(false);
    }
  };

  return (
    <AlertDialog open={open} onOpenChange={handleOpenChange}>
      <AlertDialogTrigger asChild>
        <Button size="sm" variant="outline">
          <Trash2Icon className="text-destructive" /> 選択した {ids.length} 件を削除
        </Button>
      </AlertDialogTrigger>
      <AlertDialogContent className="max-h-[85vh] overflow-y-auto">
        <AlertDialogHeader>
          <AlertDialogTitle>選択した {ids.length} 件のタスクを削除しますか？</AlertDialogTitle>
          <AlertDialogDescription>
            これらのタスクの実行記録と探索チェーンが完全に削除されます。下のクリーンアップオプションは選択したタスクに一括で適用されます。
          </AlertDialogDescription>
        </AlertDialogHeader>
        <div className="text-muted-foreground flex flex-wrap gap-1 text-xs">
          {ids.slice(0, 30).map((id) => (
            <code key={id} className="bg-muted rounded px-1.5 py-0.5 font-mono">
              #{id}
            </code>
          ))}
          {ids.length > 30 && <span className="self-center">…ほか {ids.length} 件</span>}
        </div>
        <DeleteOptionFields idPrefix="bulk" options={options} onOptionsChange={setOptions} disabled={deleting} />
        <AlertDialogFooter>
          <AlertDialogCancel disabled={deleting}>キャンセル</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={deleting}
            onClick={(event) => {
              event.preventDefault();
              void handleDelete();
            }}
          >
            {deleting && <Spinner data-icon="inline-start" />}
            {deleting ? `削除中 ${done}/${ids.length}` : `${ids.length} 件のタスクを削除`}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

// CreateTaskSheet はタスク作成ドロワー。フォーム状態は TasksPage ではなくここに置く:
// 説明/ゴールをページコンポーネントが保持すると、1 文字入力するたびにドロワー背後の
// タスクテーブル全体(固定列と 20 個の AlertDialog ツリーを含む)が再描画され、
// 入力ラグとして現れていた。今はタイピングでドロワーだけが再描画される。
function SourceTaskPicker({
  tasks,
  value,
  onValueChange,
  portalContainer,
}: {
  tasks: Task[];
  value: string[];
  onValueChange: (value: string[]) => void;
  portalContainer?: React.RefObject<HTMLElement | null>;
}) {
  const tasksByID = React.useMemo(() => new Map(tasks.map((task) => [task.id, task])), [tasks]);
  const taskIDs = React.useMemo(() => tasks.map((task) => task.id), [tasks]);
  const atLimit = value.length >= MAX_SOURCE_TASKS;

  const handleValueChange = (next: string[]) => {
    onValueChange(next.slice(0, MAX_SOURCE_TASKS));
  };

  return (
    <Combobox
      items={taskIDs}
      itemToStringValue={(taskID) => {
        const task = tasksByID.get(taskID);
        return task ? `${task.id} ${task.description} ${task.goal}` : taskID;
      }}
      multiple
      value={value}
      onValueChange={handleValueChange}
    >
      <ComboboxChips>
        <ComboboxValue>
          {value.map((taskID) => (
            <ComboboxChip key={taskID}>#{taskID}</ComboboxChip>
          ))}
        </ComboboxValue>
        <ComboboxChipsInput
          id="source-tasks"
          placeholder={atLimit ? `関連付けられるタスクは最大 ${MAX_SOURCE_TASKS} 件` : "タスク ID、説明、ゴールを検索"}
          disabled={atLimit}
        />
      </ComboboxChips>
      <ComboboxContent portalContainer={portalContainer}>
        <ComboboxEmpty>一致するタスクはありません</ComboboxEmpty>
        <ComboboxList>
          {(taskID) => {
            const task = tasksByID.get(taskID);
            return (
              <ComboboxItem key={taskID} value={taskID} disabled={atLimit && !value.includes(taskID)}>
                <div className="flex min-w-0 flex-1 items-center gap-2">
                  <div className="min-w-0 flex-1">
                    <p className="truncate text-sm font-medium">
                      #{taskID} · {task?.description ?? "不明なタスク"}
                    </p>
                    {task?.goal && <p className="text-muted-foreground truncate text-xs">{task.goal}</p>}
                  </div>
                  {task && <StatusBadge domain="task" value={task.status} />}
                </div>
              </ComboboxItem>
            );
          }}
        </ComboboxList>
      </ComboboxContent>
    </Combobox>
  );
}

// CategoryPicker はタスク作成画面の単一選択カテゴリセレクター。既存カテゴリを検索でき、
// ライブラリにない名前を入力して Enter(またはドロップダウンの「作成」をクリック)すると
// 即座に作成して選択する。選択項目は削除可能な tag で表示する。カテゴリはグローバルな
// リソースなので、ここでの即時作成は「分類管理」での手動作成と等価。複数選択は許可しない。
function CategoryPicker({
  categories,
  value,
  onValueChange,
  onCategoryCreated,
  portalContainer,
}: {
  categories: TaskCategory[];
  value?: number;
  onValueChange: (categoryID?: number) => void;
  onCategoryCreated: () => void;
  portalContainer?: React.RefObject<HTMLElement | null>;
}) {
  const [inputValue, setInputValue] = React.useState("");
  const [creating, setCreating] = React.useState(false);
  // 新規作成した分類は親が再取得して初めて categories に反映される。その間、選択中の chip と
  // ドロップダウンが「不明な分類」と表示されないよう、ローカルに 1 部保持しておく。
  const [localExtra, setLocalExtra] = React.useState<TaskCategory[]>([]);

  const allCategories = React.useMemo(() => {
    const byID = new Map<number, TaskCategory>();
    for (const category of categories) byID.set(category.id, category);
    for (const category of localExtra) if (!byID.has(category.id)) byID.set(category.id, category);
    return [...byID.values()];
  }, [categories, localExtra]);

  const byID = React.useMemo(() => new Map(allCategories.map((c) => [String(c.id), c])), [allCategories]);
  const categoryIDs = React.useMemo(() => allCategories.map((c) => String(c.id)), [allCategories]);
  const selectedIDs = value != null ? [String(value)] : [];

  const trimmed = inputValue.trim();
  const lower = trimmed.toLowerCase();
  // base-ui 既定の部分文字列フィルタと揃えて「関連する分類があるか」を判定する。
  const matchCount = trimmed
    ? allCategories.filter((c) => c.name.toLowerCase().includes(lower)).length
    : allCategories.length;

  const createAndSelect = async () => {
    if (!trimmed || creating) return;
    // 完全同名が既に存在すればそれを選ぶだけで、重複作成はしない。
    const existing = allCategories.find((c) => c.name.toLowerCase() === lower);
    if (existing) {
      onValueChange(existing.id);
      setInputValue("");
      return;
    }
    setCreating(true);
    try {
      const created = await api.createTaskCategory(trimmed);
      setLocalExtra((prev) => [...prev, created]);
      onValueChange(created.id);
      setInputValue("");
      onCategoryCreated();
      toast.success(`分類「${created.name}」を作成しました`);
    } catch (e) {
      toast.error(`分類の作成に失敗しました：${(e as Error).message}`);
    } finally {
      setCreating(false);
    }
  };

  return (
    <Combobox
      items={categoryIDs}
      itemToStringValue={(id) => byID.get(id)?.name ?? id}
      multiple
      value={selectedIDs}
      onValueChange={(next: string[]) => {
        // 単一選択:直近に選んだ 1 件を採用する。chip を外す(クリアする)と未分類に戻る。
        const last = next[next.length - 1];
        onValueChange(last ? Number(last) : undefined);
        setInputValue("");
      }}
      inputValue={inputValue}
      onInputValueChange={setInputValue}
    >
      <ComboboxChips>
        <ComboboxValue>
          {selectedIDs.map((id) => (
            <ComboboxChip key={id}>{byID.get(id)?.name ?? "不明な分類"}</ComboboxChip>
          ))}
        </ComboboxValue>
        <ComboboxChipsInput
          id="task-category"
          placeholder={selectedIDs.length ? "" : "分類を検索、または新しい名前を入力して Enter で作成"}
          onKeyDown={(e) => {
            // 一致がまったくないときの Enter = 作成。一致項目があるときは base-ui の
            // 「Enter でハイライト項目を選択」をそのまま使う。
            if (e.key === "Enter" && matchCount === 0 && trimmed) {
              e.preventDefault();
              void createAndSelect();
            }
          }}
        />
      </ComboboxChips>
      <ComboboxContent portalContainer={portalContainer}>
        <ComboboxList>
          {(id: string) => (
            <ComboboxItem key={id} value={id}>
              {byID.get(id)?.name ?? id}
            </ComboboxItem>
          )}
        </ComboboxList>
        {matchCount === 0 &&
          (trimmed ? (
            <button
              type="button"
              disabled={creating}
              onClick={() => void createAndSelect()}
              className="flex w-full items-center gap-2 px-2 py-2 text-left text-sm hover:bg-accent hover:text-accent-foreground disabled:opacity-50"
            >
              {creating ? <Spinner className="size-4" /> : <PlusIcon className="size-4" />}
              分類「{trimmed}」を作成
            </button>
          ) : (
            <div className="px-2 py-2 text-sm text-muted-foreground">名前を入力して分類を検索または作成</div>
          ))}
      </ComboboxContent>
    </Combobox>
  );
}

const COMPANY_SCOPE_LABELS: Record<string, string> = {
  domain: "ドメイン",
  ip: "IP",
  cidr: "CIDR",
  icp: "ICP",
  keyword: "キーワード",
};

function companyScopeSummary(company: Company): string {
  const rows = company.scope ?? [];
  if (rows.length === 0) return "資産範囲が未設定";
  const preview = rows.slice(0, 3).map((row) => {
    const value = row.raw || row.value || row.domain || row.net || "";
    return `${COMPANY_SCOPE_LABELS[row.kind] ?? row.kind}：${value}`;
  });
  return `${preview.join(" · ")}${rows.length > preview.length ? ` · ほか ${rows.length - preview.length} 件` : ""}`;
}

function CompanyPicker({
  companies,
  value,
  onValueChange,
  portalContainer,
}: {
  companies: Company[];
  value: number[];
  onValueChange: (value: number[]) => void;
  portalContainer?: React.RefObject<HTMLElement | null>;
}) {
  const companiesByID = React.useMemo(
    () => new Map(companies.map((company) => [String(company.id), company])),
    [companies],
  );
  const companyIDs = React.useMemo(() => companies.map((company) => String(company.id)), [companies]);
  const selectedIDs = React.useMemo(() => value.map(String), [value]);

  return (
    <Combobox
      items={companyIDs}
      itemToStringValue={(companyID) => {
        const company = companiesByID.get(companyID);
        return company ? `${company.name} ${companyScopeSummary(company)}` : companyID;
      }}
      multiple
      value={selectedIDs}
      onValueChange={(next) => onValueChange(next.map(Number).filter(Number.isFinite))}
    >
      <ComboboxChips>
        <ComboboxValue>
          {selectedIDs.map((companyID) => (
            <ComboboxChip key={companyID}>{companiesByID.get(companyID)?.name ?? `企業 #${companyID}`}</ComboboxChip>
          ))}
        </ComboboxValue>
        <ComboboxChipsInput id="task-companies" placeholder="企業名または資産範囲で検索" />
      </ComboboxChips>
      <ComboboxContent portalContainer={portalContainer}>
        <ComboboxEmpty>一致する企業はありません</ComboboxEmpty>
        <ComboboxList>
          {(companyID) => {
            const company = companiesByID.get(companyID);
            return (
              <ComboboxItem key={companyID} value={companyID}>
                <div className="flex min-w-0 flex-1 flex-col gap-0.5">
                  <div className="flex min-w-0 items-center gap-2">
                    <span className="min-w-0 flex-1 truncate font-medium">{company?.name ?? `企業 #${companyID}`}</span>
                    <span className="text-muted-foreground shrink-0 text-xs tabular-nums">
                      {company?.asset_count ?? 0} 件の資産
                    </span>
                  </div>
                  {company && (
                    <span className="text-muted-foreground truncate text-xs" title={companyScopeSummary(company)}>
                      {companyScopeSummary(company)}
                    </span>
                  )}
                </div>
              </ComboboxItem>
            );
          }}
        </ComboboxList>
      </ComboboxContent>
    </Combobox>
  );
}

type CategoryManagementView = number | "uncategorized" | "new";

function CategoryDropTarget({
  value,
  name,
  count,
  selected,
  disabled,
  onSelect,
}: {
  value: string;
  name: string;
  count: number;
  selected: boolean;
  disabled: boolean;
  onSelect: () => void;
}) {
  const { isOver, setNodeRef } = useDroppable({ id: `category:${value}`, disabled });

  return (
    <button
      ref={setNodeRef}
      type="button"
      className={cn(
        "min-w-0 rounded-md border border-transparent px-2.5 py-2 text-left transition-colors",
        selected ? "bg-accent text-accent-foreground" : "hover:bg-accent/50",
        isOver && "border-primary bg-primary/10 text-foreground",
      )}
      onClick={onSelect}
    >
      <span className="block truncate font-medium text-sm">{name}</span>
      <span className="block truncate text-muted-foreground text-xs">
        {isOver ? "ドロップして移動" : `${count} 件のタスク`}
      </span>
    </button>
  );
}

function DraggableCategoryTask({ task, disabled, moving }: { task: Task; disabled: boolean; moving: boolean }) {
  const { attributes, isDragging, listeners, setNodeRef } = useDraggable({
    id: `task:${task.id}`,
    disabled,
  });

  return (
    <Item ref={setNodeRef} variant="outline" size="sm" className={cn(isDragging && "opacity-40")}>
      <ItemMedia className="group-has-data-[slot=item-description]/item:self-center group-has-data-[slot=item-description]/item:translate-y-0">
        {moving ? (
          <Spinner />
        ) : (
          <Button
            type="button"
            size="icon-xs"
            variant="ghost"
            className="touch-none cursor-grab active:cursor-grabbing"
            disabled={disabled}
            {...listeners}
            {...attributes}
            aria-label={`タスク #${task.id} をドラッグ`}
            title="タスクをドラッグ"
          >
            <GripVerticalIcon />
          </Button>
        )}
      </ItemMedia>
      <ItemContent className="min-w-0">
        <ItemTitle className="w-full min-w-0">
          <Link href={`/function/tasks/detail?id=${encodeURIComponent(task.id)}`} className="truncate hover:underline">
            {task.name?.trim() || task.description || `タスク #${task.id}`}
          </Link>
        </ItemTitle>
        <ItemDescription className="line-clamp-1">
          #{task.id} · {task.description}
        </ItemDescription>
      </ItemContent>
      <ItemActions>
        <StatusBadge domain="task" value={task.status} />
      </ItemActions>
    </Item>
  );
}

function CategoryTaskDragPreview({ task }: { task: Task }) {
  return (
    <Item variant="outline" size="sm" className="w-80 bg-background shadow-lg">
      <ItemMedia className="group-has-data-[slot=item-description]/item:self-center group-has-data-[slot=item-description]/item:translate-y-0">
        <GripVerticalIcon className="size-4 text-muted-foreground" />
      </ItemMedia>
      <ItemContent className="min-w-0">
        <ItemTitle className="w-full min-w-0 truncate">
          {task.name?.trim() || task.description || `タスク #${task.id}`}
        </ItemTitle>
        <ItemDescription className="line-clamp-1">#{task.id}</ItemDescription>
      </ItemContent>
    </Item>
  );
}

function CategoryManagementSheet({
  categories,
  tasks,
  onChanged,
  onTaskMoved,
}: {
  categories: TaskCategory[];
  tasks: Task[];
  onChanged: () => void;
  onTaskMoved: (taskID: string, category: TaskCategory | null) => void;
}) {
  const [open, setOpen] = React.useState(false);
  const [selectedView, setSelectedView] = React.useState<CategoryManagementView>("new");
  const [draftName, setDraftName] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const [deleteOpen, setDeleteOpen] = React.useState(false);
  const [deleting, setDeleting] = React.useState(false);
  const [movingTaskID, setMovingTaskID] = React.useState<string | null>(null);
  const [activeTaskID, setActiveTaskID] = React.useState<string | null>(null);
  const wasOpen = React.useRef(false);
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 6 } }),
    useSensor(KeyboardSensor),
  );

  React.useEffect(() => {
    if (open && !wasOpen.current) {
      const first = categories[0];
      if (first) {
        setSelectedView(first.id);
        setDraftName(first.name);
      } else if (tasks.some((task) => task.category_id == null)) {
        setSelectedView("uncategorized");
        setDraftName("");
      } else {
        setSelectedView("new");
        setDraftName("");
      }
    }
    wasOpen.current = open;
  }, [categories, open, tasks]);

  const selectedCategory = React.useMemo(
    () =>
      typeof selectedView === "number" ? (categories.find((category) => category.id === selectedView) ?? null) : null,
    [categories, selectedView],
  );

  const uncategorizedCount = React.useMemo(() => tasks.filter((task) => task.category_id == null).length, [tasks]);

  const visibleTasks = React.useMemo(() => {
    if (selectedView === "uncategorized") return tasks.filter((task) => task.category_id == null);
    if (typeof selectedView === "number") return tasks.filter((task) => task.category_id === selectedView);
    return [];
  }, [selectedView, tasks]);

  const activeTask = React.useMemo(() => tasks.find((task) => task.id === activeTaskID) ?? null, [activeTaskID, tasks]);

  const selectCategory = (category: TaskCategory) => {
    setSelectedView(category.id);
    setDraftName(category.name);
  };

  const selectUncategorized = () => {
    setSelectedView("uncategorized");
    setDraftName("");
  };

  const startNew = () => {
    setSelectedView("new");
    setDraftName("");
  };

  async function saveCategory() {
    const name = draftName.trim();
    if (!name || saving || selectedView === "uncategorized") return;
    setSaving(true);
    try {
      if (selectedView === "new") {
        const created = await api.createTaskCategory(name);
        setSelectedView(created.id);
        setDraftName(created.name);
        toast.success("分類を作成しました");
      } else {
        const updated = await api.renameTaskCategory(selectedView, name);
        setDraftName(updated.name);
        toast.success("分類を更新しました");
      }
      onChanged();
    } catch (error) {
      toast.error(`${selectedView === "new" ? "作成" : "更新"}に失敗しました：${(error as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  async function deleteCategory() {
    if (!selectedCategory || deleting) return;
    const deletedID = selectedCategory.id;
    setDeleting(true);
    try {
      await api.deleteTaskCategory(deletedID);
      const next = categories.find((category) => category.id !== deletedID);
      if (next) selectCategory(next);
      else selectUncategorized();
      toast.success("分類を削除しました。関連タスクは未分類に移動しました");
      setDeleteOpen(false);
      onChanged();
    } catch (error) {
      toast.error(`分類の削除に失敗しました：${(error as Error).message}`);
    } finally {
      setDeleting(false);
    }
  }

  async function moveTask(task: Task, destination: string) {
    if (movingTaskID) return;
    const category =
      destination === "uncategorized" ? null : (categories.find((item) => item.id === Number(destination)) ?? null);
    if (destination !== "uncategorized" && !category) {
      toast.error("移動先の分類が存在しません。更新してから再試行してください");
      return;
    }
    if (task.category_id === category?.id || (task.category_id == null && category == null)) return;

    setMovingTaskID(task.id);
    try {
      await api.updateTaskCategory(task.id, category?.id);
      onTaskMoved(task.id, category);
      toast.success(`タスク #${task.id} を「${category?.name ?? "未分類"}」に移動しました`);
    } catch (error) {
      toast.error(`タスクの移動に失敗しました：${(error as Error).message}`);
    } finally {
      setMovingTaskID(null);
    }
  }

  function handleDragStart(event: DragStartEvent) {
    const id = String(event.active.id);
    setActiveTaskID(id.startsWith("task:") ? id.slice("task:".length) : null);
  }

  function handleDragEnd(event: DragEndEvent) {
    const activeID = String(event.active.id);
    const taskID = activeID.startsWith("task:") ? activeID.slice("task:".length) : null;
    setActiveTaskID(null);
    if (!taskID || !event.over) return;
    const destination = String(event.over.id);
    if (!destination.startsWith("category:")) return;
    const task = tasks.find((item) => item.id === taskID);
    if (!task) return;
    void moveTask(task, destination.slice("category:".length));
  }

  const saveLabel = saving ? "保存中" : selectedView === "new" ? "分類を作成" : "変更を保存";

  return (
    <>
      <Sheet open={open} onOpenChange={setOpen}>
        <SheetTrigger asChild>
          <Button size="sm" variant="outline">
            <TagsIcon data-icon="inline-start" />
            分類管理
          </Button>
        </SheetTrigger>
        <SheetContent className="grid h-full w-full! max-w-none! grid-rows-[auto_minmax(0,1fr)_auto] gap-0 overflow-hidden p-0 sm:w-[48rem]! sm:max-w-[48rem]!">
          <SheetHeader className="border-b px-6 py-5">
            <SheetTitle>タスク分類の管理</SheetTitle>
            <SheetDescription>
              分類はタスクの絞り込みとアーカイブに使います。変更してもタスクの実行には影響せず、削除した分類のタスクは未分類に移動します。
            </SheetDescription>
          </SheetHeader>
          <DndContext
            sensors={sensors}
            onDragStart={handleDragStart}
            onDragCancel={() => setActiveTaskID(null)}
            onDragEnd={handleDragEnd}
          >
            <div className="grid min-h-0 overflow-y-auto lg:grid-cols-[15rem_minmax(0,1fr)] lg:overflow-hidden">
              <div className="flex min-h-0 flex-col border-b p-3 lg:border-r lg:border-b-0">
                <Button type="button" variant="outline" className="w-full" onClick={startNew}>
                  <PlusIcon data-icon="inline-start" />
                  分類を新規作成
                </Button>
                <ScrollArea className="mt-2 max-h-44 lg:max-h-none lg:flex-1">
                  <div className="flex flex-col gap-1 pr-2">
                    <CategoryDropTarget
                      value="uncategorized"
                      name="未分類"
                      count={uncategorizedCount}
                      selected={selectedView === "uncategorized"}
                      disabled={movingTaskID != null}
                      onSelect={selectUncategorized}
                    />
                    {categories.map((category) => (
                      <CategoryDropTarget
                        key={category.id}
                        value={String(category.id)}
                        name={category.name}
                        count={category.task_count}
                        selected={selectedView === category.id}
                        disabled={movingTaskID != null}
                        onSelect={() => selectCategory(category)}
                      />
                    ))}
                  </div>
                </ScrollArea>
              </div>
              <ScrollArea className="min-h-0">
                <FieldGroup className="p-6">
                  {selectedView !== "uncategorized" && (
                    <Field>
                      <FieldLabel htmlFor="task-category-name">分類名</FieldLabel>
                      <Input
                        id="task-category-name"
                        value={draftName}
                        onChange={(event) => setDraftName(event.target.value)}
                        placeholder="例:外部評価"
                        maxLength={80}
                        onKeyDown={(event) => {
                          if (event.key === "Enter") void saveCategory();
                        }}
                      />
                      <FieldDescription>
                        {selectedCategory
                          ? `現在 ${selectedCategory.task_count} 件のタスクがこの分類を使用しています。名前を変更するとタスク一覧にも反映されます。`
                          : "作成後は、タスク作成時とタスク一覧の絞り込みで使用できます。"}
                      </FieldDescription>
                    </Field>
                  )}
                  {selectedView !== "new" && (
                    <Field>
                      <div className="flex flex-wrap items-end justify-between gap-2">
                        <div className="flex min-w-0 flex-col gap-1">
                          <FieldLabel>{selectedCategory ? "分類内のタスク" : "未分類のタスク"}</FieldLabel>
                          <FieldDescription>
                            {selectedCategory
                              ? `この分類には ${visibleTasks.length} 件のタスクがあります。`
                              : `現在 ${visibleTasks.length} 件のタスクが未分類です。`}
                          </FieldDescription>
                        </div>
                      </div>
                      {visibleTasks.length === 0 ? (
                        <Empty className="min-h-36 border">
                          <EmptyHeader>
                            <EmptyTitle>
                              {selectedCategory ? "この分類にはまだタスクがありません" : "未分類のタスクはありません"}
                            </EmptyTitle>
                            <EmptyDescription>タスクが分類されるとここに表示されます。</EmptyDescription>
                          </EmptyHeader>
                        </Empty>
                      ) : (
                        <ItemGroup className="gap-2">
                          {visibleTasks.map((task) => (
                            <DraggableCategoryTask
                              key={task.id}
                              task={task}
                              moving={movingTaskID === task.id}
                              disabled={movingTaskID != null}
                            />
                          ))}
                        </ItemGroup>
                      )}
                    </Field>
                  )}
                </FieldGroup>
              </ScrollArea>
            </div>
            <DragOverlay>{activeTask ? <CategoryTaskDragPreview task={activeTask} /> : null}</DragOverlay>
          </DndContext>
          <SheetFooter className="border-t px-6 py-4 sm:flex-row sm:items-center">
            {selectedCategory && (
              <Button
                type="button"
                variant="destructive"
                className="sm:mr-auto"
                disabled={saving || deleting}
                onClick={() => setDeleteOpen(true)}
              >
                <Trash2Icon data-icon="inline-start" />
                分類を削除
              </Button>
            )}
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>
              閉じる
            </Button>
            {selectedView !== "uncategorized" && (
              <Button
                type="button"
                disabled={!draftName.trim() || saving || deleting}
                onClick={() => void saveCategory()}
              >
                {saving ? <Spinner data-icon="inline-start" /> : <SaveIcon data-icon="inline-start" />}
                {saveLabel}
              </Button>
            )}
          </SheetFooter>
        </SheetContent>
      </Sheet>
      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>分類「{selectedCategory?.name || "名称未設定の分類"}」を削除しますか？</AlertDialogTitle>
            <AlertDialogDescription>
              分類を削除すると、その中の {selectedCategory?.task_count ?? 0}{" "}
              件のタスクは自動的に「未分類」へ移動します。タスクのデータ自体は削除されません。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>キャンセル</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              disabled={deleting}
              onClick={(event) => {
                event.preventDefault();
                void deleteCategory();
              }}
            >
              {deleting && <Spinner data-icon="inline-start" />}
              {deleting ? "削除中" : "削除"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

function CreateTaskSheet({
  tasks,
  categories,
  onCreated,
  onCategoriesChanged,
}: {
  tasks: Task[];
  categories: TaskCategory[];
  onCreated: () => void;
  onCategoriesChanged: () => void;
}) {
  const [open, setOpen] = React.useState(false);
  const [name, setName] = React.useState("");
  const [categoryID, setCategoryID] = React.useState<number | undefined>(undefined);
  const [description, setDescription] = React.useState("");
  const [goal, setGoal] = React.useState("");
  const [selectedTemplateID, setSelectedTemplateID] = React.useState<number | null>(null);
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [companies, setCompanies] = React.useState<Company[]>([]);
  const [sourceTaskIDs, setSourceTaskIDs] = React.useState<string[]>([]);
  const [companyIDs, setCompanyIDs] = React.useState<number[]>([]);
  const [llmProfileIDs, setLLMProfileIDs] = React.useState<string[]>([]);
  const [creating, setCreating] = React.useState(false);
  const [timeoutMin, setTimeoutMin] = React.useState(""); // タスク単位のタイムアウト(分)。空/0 = 無制限
  const [heartbeatMin, setHeartbeatMin] = React.useState("10"); // planner ハートビート(分)。既定 10、下限 10(バックエンドと一致)
  const [seedFirstIntent, setSeedFirstIntent] = React.useState(false); // 作成時に最初の意図を投入し、worker が初回 planner を待たずに即実行する。既定は無効で、標準の「まず計画、次に実行」に従う
  const [coverageEnabled, setCoverageEnabled] = React.useState(true); // 資産カバレッジ機能。既定は有効。無効 = カバレッジを計算/表示せず、範囲を蓄積せず、範囲系ツールを隠す(company の関連付けには影響しない)
  const [interceptRules, setInterceptRules] = React.useState<AssetInterceptRuleInput[]>([]); // タスク単位の資産インターセプトルール(このタスクのみ有効、グローバル表には書き込まない)
  // 方式 1 のファイルアップロード:タスク作成前にファイルを drafts/<draftId>/uploads/ に一時保存し、
  // 返ってきた絶対パスを説明文に追記する。
  const [uploading, setUploading] = React.useState(false);
  const [uploadCount, setUploadCount] = React.useState(0);
  const draftIdRef = React.useRef<string>("");
  const fileInputRef = React.useRef<HTMLInputElement>(null);
  const sheetContentRef = React.useRef<HTMLDivElement>(null);

  // タスク作成画面のプロファイル選択用に LLM プロファイルを 1 回だけ読み込む。
  React.useEffect(() => {
    api
      .llmProfiles()
      .then(setProfiles)
      .catch(() => setProfiles([]));
    api
      .companies()
      .then(setCompanies)
      .catch(() => setCompanies([]));
  }, []);

  // pickFiles は選択されたファイルをこのドラフトのステージングディレクトリにアップロードし、
  // 絶対パスを説明文に追記する。タスクのエージェントは Read/Bash でパスから開く。
  async function pickFiles(files: FileList | null) {
    if (!files || files.length === 0) return;
    // crypto.randomUUID は安全なコンテキスト(https/localhost)でのみ使用可能。IP+http 経由では縮退する。
    if (!draftIdRef.current) {
      draftIdRef.current =
        globalThis.crypto?.randomUUID?.() ?? `d${Date.now().toString(36)}${Math.random().toString(36).slice(2, 10)}`;
    }
    setUploading(true);
    try {
      const r = await api.chatUpload("staging", draftIdRef.current, Array.from(files));
      setDescription((prev) => appendUploads(prev, r.attachments));
      setUploadCount((n) => n + r.attachments.length);
    } catch (e) {
      toast.error("アップロードに失敗しました：" + (e as Error).message);
    } finally {
      setUploading(false);
      if (fileInputRef.current) fileInputRef.current.value = ""; // 同じファイルを再選択できるようにする
    }
  }

  async function createTask() {
    if (!description.trim() || !goal.trim()) {
      toast.error("説明とゴールを入力してください");
      return;
    }
    if (sourceTaskIDs.length > MAX_SOURCE_TASKS) {
      toast.error(`関連付けられるソースタスクは最大 ${MAX_SOURCE_TASKS} 件です`);
      return;
    }
    setCreating(true);
    try {
      const timeoutSec = Math.max(0, Math.floor(Number(timeoutMin) || 0)) * 60;
      const heartbeatSec = Math.max(10, Math.floor(Number(heartbeatMin) || 10)) * 60; // 下限 10 分、バックエンドの正規化と一致
      await api.createTask({
        name: name.trim(),
        categoryId: categoryID,
        description: description.trim(),
        goal: goal.trim(),
        llmProfileIds: llmProfileIDs.map(Number),
        sourceTaskIds: sourceTaskIDs,
        companyIds: companyIDs,
        timeoutSeconds: timeoutSec,
        seedFirstIntent,
        planHeartbeatSeconds: heartbeatSec,
        coverageEnabled,
        interceptRules: interceptRules
          .map((r) => ({ ...r, pattern: r.pattern.trim() }))
          .filter((r) => r.pattern !== ""),
      });
      toast.success("タスクを作成しました");
      setName("");
      setCategoryID(undefined);
      setDescription("");
      setGoal("");
      setSelectedTemplateID(null);
      setSourceTaskIDs([]);
      setCompanyIDs([]);
      setLLMProfileIDs([]);
      setTimeoutMin("");
      setHeartbeatMin("10");
      setSeedFirstIntent(false);
      setCoverageEnabled(true);
      setInterceptRules([]);
      setUploadCount(0);
      draftIdRef.current = "";
      setOpen(false);
      onCreated();
    } catch (e) {
      toast.error("作成に失敗しました：" + (e as Error).message);
    } finally {
      setCreating(false);
    }
  }

  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetTrigger asChild>
        <Button size="sm">
          <PlusIcon /> タスクを作成
        </Button>
      </SheetTrigger>
      {/* 45vw 幅の右側ドロワー:画面全体の高さでスクロールでき、長いフォームもダイアログの高さ制限を受けない。狭い画面では全幅になる。
            内容は flex 列:ヘッダー/フッターは固定、中央のフィールド領域が flex-1 で独立スクロールする。 */}
      <SheetContent
        ref={sheetContentRef}
        side="right"
        className="w-full! max-w-none! gap-0 p-0 sm:w-[45vw]! sm:max-w-[45vw]!"
      >
        <SheetHeader className="border-b p-6">
          <SheetTitle>タスクを作成</SheetTitle>
          <SheetDescription>
            テスト対象とゴールを入力してください。詳細パラメータは必要に応じて展開できます。
          </SheetDescription>
        </SheetHeader>

        <div className="flex-1 overflow-y-auto p-6">
          <div className="grid gap-5">
            <TaskTemplateControls
              description={description}
              goal={goal}
              categoryID={categoryID}
              interceptRules={interceptRules}
              selectedTemplateID={selectedTemplateID}
              onSelectedTemplateIDChange={setSelectedTemplateID}
              onApply={(template) => {
                setDescription(template.description);
                setGoal(template.goal);
                setCategoryID(template.category_id ?? undefined);
                setInterceptRules(template.intercept_rules ?? []);
                setUploadCount(0);
              }}
              portalContainer={sheetContentRef}
            />
            <div className="grid gap-2">
              <Label htmlFor="name">名前（任意）</Label>
              <Input
                id="name"
                placeholder="例:Acme 公式サイトのペネトレーションテスト"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <Field>
              <FieldLabel htmlFor="task-category">タスク分類</FieldLabel>
              <CategoryPicker
                categories={categories}
                value={categoryID}
                onValueChange={setCategoryID}
                onCategoryCreated={onCategoriesChanged}
                portalContainer={sheetContentRef}
              />
              <FieldDescription>
                任意、単一の分類。タスク一覧の絞り込みとアーカイブに使われ、Agent の実行には影響しません。
              </FieldDescription>
            </Field>
            <div className="grid gap-2">
              <Label htmlFor="description">説明</Label>
              <Textarea
                id="description"
                className="min-h-32"
                placeholder="テスト対象と背景。例:example.com というサイトをテストする"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
              />
              {/* ファイルアップロード(複数可):drafts/ に一時保存し、絶対パスを上の説明文に追記する。worker はそれを使って Read/Bash で開く。 */}
              <div className="flex flex-wrap items-center gap-2">
                <input
                  ref={fileInputRef}
                  type="file"
                  multiple
                  className="hidden"
                  onChange={(e) => void pickFiles(e.target.files)}
                />
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => fileInputRef.current?.click()}
                  disabled={uploading}
                >
                  {uploading ? <Loader2Icon className="animate-spin" /> : <PaperclipIcon />}
                  ファイルをアップロード
                </Button>
                <span className="text-muted-foreground text-xs">
                  {uploadCount > 0
                    ? `${uploadCount} 件のファイルをアップロード済み。絶対パスを説明文の末尾に追記しました（編集可能）`
                    : "複数選択可。アップロード後にファイルの絶対パスを説明文へ追記し、worker が Read/Bash で開けるようにします"}
                </span>
              </div>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="goal">ゴール</Label>
              <Textarea
                id="goal"
                className="min-h-32"
                placeholder="何を達成するか。例:管理画面へのアクセス権を取得する、サーバー権限を取得する"
                value={goal}
                onChange={(e) => setGoal(e.target.value)}
              />
            </div>
            <Field>
              <FieldLabel htmlFor="source-tasks">関連タスク</FieldLabel>
              <SourceTaskPicker
                tasks={tasks}
                value={sourceTaskIDs}
                onValueChange={setSourceTaskIDs}
                portalContainer={sheetContentRef}
              />
              <FieldDescription>
                最大 {MAX_SOURCE_TASKS}{" "}
                件のタスクを関連付けられます。選択したタスクの永続ブラックボード、資産範囲、関連トラフィックをリアルタイムに読み取り専用で継承します。新しいタスクの書き込み先は独立したブラックボードです。
              </FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="task-companies">関連企業の資産範囲</FieldLabel>
              <CompanyPicker
                companies={companies}
                value={companyIDs}
                onValueChange={setCompanyIDs}
                portalContainer={sheetContentRef}
              />
              <FieldDescription>
                タスク作成時に、選択した企業の既存資産を「テスト資産」に追加し、ドメイン、IP、CIDR、ICP、企業キーワードを範囲のコンテキストとして
                Agent に渡します。意図を自動生成したり、実行ゴールを強制変更したりはしません。
              </FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="task-intercept-rules">
                タスク単位の資産インターセプト / 許可ルール（任意）
              </FieldLabel>
              <AssetInterceptRulesEditor value={interceptRules} onChange={setInterceptRules} />
              <FieldDescription>
                このタスクにのみ適用され、グローバルルールには書き込まれません。判定順序:まず「インターセプト」ルール（グローバルを含む）で照合し、一致すればテスト禁止。一致せず、このタスクに「許可」ルールが設定されている場合は、いずれかの許可ルールに一致したときのみ通過し、一致しなければ同じくテスト不可。許可ルールが
                1 つも設定されていなければホワイトリストは有効になりません。
              </FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="llm-profiles">LLM 設定チェーン</FieldLabel>
              <TaskLLMProfileChain
                profiles={profiles}
                value={llmProfileIDs}
                onValueChange={setLLMProfileIDs}
                inputId="llm-profiles"
                portalContainer={sheetContentRef}
              />
              <FieldDescription>
                リストの順にフェイルオーバーします。先頭が現在の設定で、明確にクォータ不足のときだけ次の項目に切り替わります。
              </FieldDescription>
            </Field>

            {/* 詳細パラメータは既定で折りたたむ:タイムアウト/ハートビート/最初の意図は展開したときだけ場所を取り、よく使う経路をすっきり保つ。 */}
            <Collapsible>
              <CollapsibleTrigger className="group flex w-full items-center gap-2 border-t pt-4 text-sm font-medium">
                <ChevronRightIcon className="text-muted-foreground size-4 transition-transform group-data-[state=open]:rotate-90" />
                詳細設定
                <span className="text-muted-foreground ml-auto text-xs font-normal">
                  タイムアウト · ハートビート · 最初の意図
                </span>
              </CollapsibleTrigger>
              <CollapsibleContent className="grid gap-5 pt-5">
                <div className="grid gap-2">
                  <Label htmlFor="timeout-min">タスクのタイムアウト（分、任意）</Label>
                  <Input
                    id="timeout-min"
                    type="number"
                    min={0}
                    className="w-40"
                    placeholder="空欄 = 無制限"
                    value={timeoutMin}
                    onChange={(e) => setTimeoutMin(e.target.value)}
                  />
                  <p className="text-muted-foreground text-xs">
                    到達すると優雅な締めくくり（各 agent の書き戻し + planner の最終判定）を実行し、タスクは timeout
                    の終端状態に入ります。
                  </p>
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="heartbeat-min">planner ハートビート（分）</Label>
                  <Input
                    id="heartbeat-min"
                    type="number"
                    min={10}
                    className="w-40"
                    placeholder="既定 10"
                    value={heartbeatMin}
                    onChange={(e) => setHeartbeatMin(e.target.value)}
                  />
                  <p className="text-muted-foreground text-xs">
                    前回の計画終了 / タスク開始からこの時間が経過し、その間にトリガーがない場合に自動で計画を 1
                    回実行します（ハングの救済 + 実行中の worker の監督を促す）。下限は 10 分です。
                  </p>
                </div>
                <div className="grid gap-2">
                  <label htmlFor="seed-first-intent" className="flex items-center gap-2 text-sm">
                    <Checkbox
                      id="seed-first-intent"
                      checked={seedFirstIntent}
                      onCheckedChange={(v) => setSeedFirstIntent(!!v)}
                    />
                    最初の意図（説明 + ゴール）を直接投入する
                  </label>
                  <p className="text-muted-foreground text-xs">
                    有効にすると作成時に「説明 + ゴール」を 1 つの意図として投入し、worker
                    は初回計画を待たずに直接実行を開始します。実行後に planner が引き継いで判定 / 補足します。CTF
                    のように 1
                    回の作業で解決することが多い場面で有効にするのがおすすめです。無効の場合は標準の「まず計画、次に実行」に従います。
                  </p>
                </div>
                <div className="grid gap-2">
                  <label htmlFor="coverage-enabled" className="flex items-center gap-2 text-sm">
                    <Checkbox
                      id="coverage-enabled"
                      checked={coverageEnabled}
                      onCheckedChange={(v) => setCoverageEnabled(!!v)}
                    />
                    資産カバレッジ機能
                  </label>
                  <p className="text-muted-foreground text-xs">
                    既定で有効:テストのカバレッジを計算・表示し、状況図にテスト進捗を表示し、テスト範囲を自動で蓄積します。無効にするとカバレッジの計算
                    / 表示を行わず、 状況図は資産のみを表示して進捗を表示せず、agent
                    も範囲系ツールを受け取らなくなります。無効にしても「関連企業の資産範囲」には影響しません。
                  </p>
                </div>
              </CollapsibleContent>
            </Collapsible>
          </div>
        </div>

        <SheetFooter className="flex-row justify-end gap-2 border-t p-4">
          <SheetClose asChild>
            <Button variant="outline">キャンセル</Button>
          </SheetClose>
          <Button onClick={createTask} disabled={creating || uploading}>
            {creating && <Spinner data-icon="inline-start" />}
            {creating ? "作成中" : "作成"}
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}
