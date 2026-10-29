"use client";

import * as React from "react";

import {
  Loader2Icon,
  PlugZapIcon,
  PlusIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  SaveIcon,
  StarIcon,
  Trash2Icon,
  ZapIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import type { LLMPoolMember, LLMPoolStatus, LLMProfile, LLMRetryOverride } from "@/lib/types";
import { cn } from "@/lib/utils";

import { ProfileRetryFields, RetryPolicyPanel, ZERO_OVERRIDE } from "./_components/retry";

// 思考スイッチ（thinking.type）と思考強度（reasoning_effort）は【互いに独立】した 2 つのフィールドで、
// それぞれ個別に設定する。thinking フィールドを持たず強度パラメータだけで思考を有効化できる API も
// あるため、切り離しておく必要がある。
// 保存時の空文字列 = そのフィールドを【送信しない】。Radix Select は空の value を受け付けないため、
// UI では "none" センチネルで「送信しない」を表し、保存・読込時に "" と相互変換する（NONE / fromStore / toStore）。
const NONE = "none";
const fromStore = (v?: string) => (v ? v : NONE);
const toStore = (v: string) => (v === NONE ? "" : v);
const THINKING_TYPES: { value: string; label: string }[] = [
  { value: NONE, label: "送信しない（既定）" },
  { value: "disabled", label: "オフ" },
  { value: "enabled", label: "オン" },
];
// 出力上限にどのリクエストフィールド名を使うか（openai 形式のみ意味を持つ）。NONE ↔ "" は同じセンチネル変換を通す。
const MAX_TOKENS_FIELDS: { value: string; label: string }[] = [
  { value: NONE, label: "max_tokens（既定）" },
  { value: "max_completion_tokens", label: "max_completion_tokens" },
];
// 他の 2 形式はフィールド名が固定されているため、このオプションは意味を持たない。説明文でその旨を明記する。
const MAX_TOKENS_FIELD_HINTS: Record<string, string> = {
  openai:
    "上限を送るキー。max_tokens が既定で、ほとんどの互換ゲートウェイはこれしか受け付けない。一方、OpenAI 公式の推論モデル（o シリーズ / GPT-5）は max_completion_tokens しか受け付けず、max_tokens を渡すと unsupported_parameter を返す。",
  anthropic: "openai 形式でのみ選択可能。Anthropic のフィールド名は max_tokens に固定されている。",
  "openai-responses": "openai 形式でのみ選択可能。Responses API のフィールド名は max_output_tokens に固定されている。",
};
const EFFORT_LEVELS: { value: string; label: string }[] = [
  { value: NONE, label: "送信しない（既定）" },
  { value: "low", label: "low" },
  { value: "medium", label: "medium" },
  { value: "high", label: "high" },
  { value: "xhigh", label: "xhigh" },
  { value: "max", label: "max" },
];

function cooldownText(secs: number) {
  if (secs <= 0) return "";
  if (secs < 60) return `${secs}s`;
  return `${Math.ceil(secs / 60)}min`;
}

// カード上に表示する設定の「正常かどうか」。Key 未設定の設定はそもそもリクエストを送れないため、
// サーキットブレーカーより先に示すべき。その他の状態はポーリングのサーキット記録から得る
// （ポーリングが無効なら新しい記録は生まれず、その場合の「正常」= 既知の障害なし）。
type Health = { label: string; cls: string; hint?: string };
function healthOf(p: LLMProfile, m?: LLMPoolMember): Health {
  if (!p.api_key_hint) {
    return {
      label: "Key 未設定",
      cls: "border-muted-foreground/40 text-muted-foreground",
      hint: "API Key が未入力のため呼び出せない",
    };
  }
  if (m?.state === "tripped") {
    return {
      label: m.cooldown_secs > 0 ? `遮断中 · ${cooldownText(m.cooldown_secs)}` : "遮断中",
      cls: "border-destructive/50 text-destructive",
      hint: m.last_error,
    };
  }
  if (m?.state === "degraded") {
    return {
      label: `異常 · 失敗 ${m.fails} 回`,
      cls: "border-amber-500/50 text-amber-600 dark:text-amber-400",
      hint: m.last_error,
    };
  }
  return { label: "正常", cls: "border-emerald-500/50 text-emerald-600 dark:text-emerald-400" };
}

// ─────────────────────────────────────────────────────────────────────────────
// ポーリング設定ドロワー
// ─────────────────────────────────────────────────────────────────────────────

function PoolSheet({
  open,
  onOpenChange,
  pool,
  onReload,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  pool: LLMPoolStatus | null;
  onReload: () => Promise<void>;
}) {
  const [busy, setBusy] = React.useState(false);

  // クールダウンの残り秒数はバックエンドが算出する。ドロワーが開いていて異常な設定がある場合のみ
  // 定期的に取得し、カウントダウンを進める。
  React.useEffect(() => {
    if (!open || !pool?.enabled || !pool.chain.some((m) => m.state !== "ok")) return;
    const t = setInterval(() => void onReload(), 10_000);
    return () => clearInterval(t);
  }, [open, pool, onReload]);

  async function toggle(patch: { llm_pool_enabled?: boolean; llm_pool_bind_fallback?: boolean }) {
    if (busy) return;
    setBusy(true);
    try {
      await api.setSettings(patch);
      await onReload();
      if (patch.llm_pool_enabled !== undefined) {
        toast.success(patch.llm_pool_enabled ? "LLM ポーリングを有効にしました" : "LLM ポーリングを無効にしました");
      } else {
        toast.success("フォールバック設定を更新しました");
      }
    } catch (e) {
      toast.error(`設定に失敗しました：${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  }

  async function recover(id?: string) {
    try {
      await api.resetLLMPool(id);
      await onReload();
      toast.success(id ? "この設定を復旧しました" : "すべての設定を復旧しました");
    } catch (e) {
      toast.error(`復旧に失敗しました：${(e as Error).message}`);
    }
  }

  const enabled = pool?.enabled ?? false;
  const chain = pool?.chain ?? [];
  // ポーリングに参加するメンバー（「ポーリング不参加」とされたものを除く）。順序はバックエンドの実際の試行順。
  const inChain = chain.filter((m) => m.active || !m.excluded);
  const tripped = chain.filter((m) => m.state === "tripped");

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="flex flex-col gap-0 p-0 data-[side=right]:sm:max-w-lg">
        <SheetHeader className="px-4">
          <SheetTitle className="flex items-center gap-2">
            <ZapIcon className="size-4" /> LLM ポーリング · フェイルオーバー
          </SheetTitle>
          <SheetDescription>
            有効にすると、<b>モデル未指定</b>の Agent は現在の設定が利用不可（残高不足 / Key 失効 / レート制限 /
            サービス障害）のとき、自動で次の設定に切り替わる。
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-6">
          <div className="flex items-center justify-between gap-4 rounded-lg border p-3">
            <div className="grid gap-0.5">
              <Label className="text-sm">ポーリングを有効化</Label>
              <p className="text-muted-foreground text-xs">
                既定は無効。無効時は常にアクティブ設定のみを使い、失敗すればそのまま失敗となる。
              </p>
            </div>
            <Switch
              checked={enabled}
              disabled={busy}
              onCheckedChange={(v) => void toggle({ llm_pool_enabled: v })}
              aria-label="LLM ポーリングのスイッチ"
            />
          </div>

          {enabled && (
            <>
              <div className="flex items-center justify-between gap-4 rounded-lg border p-3">
                <div className="grid gap-0.5">
                  <Label className="text-sm">モデル指定時もフォールバック</Label>
                  <p className="text-muted-foreground text-xs">
                    既定は無効。Agent
                    やタスクが特定の設定を指定した場合はそれだけを使い、失敗すればそのまま失敗となる（こっそり別モデルへ切り替えたりはしない）。
                    有効にすると、指定された設定が失敗したときも下のポーリングチェーンへフォールバックする。
                  </p>
                </div>
                <Switch
                  checked={pool?.bind_fallback ?? false}
                  disabled={busy}
                  onCheckedChange={(v) => void toggle({ llm_pool_bind_fallback: v })}
                  aria-label="バインド設定失敗時のフォールバックスイッチ"
                />
              </div>

              <Separator />

              <div className="grid gap-2">
                <div className="flex items-center justify-between">
                  <Label className="text-sm">ポーリング順序</Label>
                  {tripped.length > 0 && (
                    <Button size="sm" variant="ghost" onClick={() => void recover()}>
                      <RotateCcwIcon /> すべて復旧
                    </Button>
                  )}
                </div>
                {inChain.length < 2 && (
                  <p className="text-muted-foreground text-xs">
                    現在利用可能な設定は {inChain.length} 個のため、ポーリングは機能しない。少なくとも API Key
                    入力済みでポーリングに参加する設定が 2 個必要。
                  </p>
                )}
                {chain.map((m) => {
                  const excluded = m.excluded && !m.active;
                  const order = excluded ? null : inChain.findIndex((x) => x.profile_id === m.profile_id) + 1;
                  return (
                    <div
                      key={m.profile_id}
                      className={cn(
                        "grid gap-1 rounded-lg border p-2.5 text-sm",
                        excluded && "opacity-55",
                        m.state === "tripped" && "border-destructive/40",
                      )}
                    >
                      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                        <span className="w-5 shrink-0 text-center font-mono text-muted-foreground text-xs">
                          {order ?? "—"}
                        </span>
                        <span className="font-medium">{m.name}</span>
                        {m.active && (
                          <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                            アクティブ
                          </Badge>
                        )}
                        {excluded && <Badge variant="outline">ポーリング不参加</Badge>}
                        <div className="ml-auto flex items-center gap-2">
                          {m.state === "tripped" && m.cooldown_secs > 0 && (
                            <span className="text-muted-foreground text-xs">
                              クールダウン {cooldownText(m.cooldown_secs)}
                            </span>
                          )}
                          {m.state === "degraded" && (
                            <span className="text-muted-foreground text-xs">連続失敗 {m.fails} 回</span>
                          )}
                          {m.state !== "ok" && (
                            <Button
                              size="icon"
                              variant="ghost"
                              className="size-7"
                              aria-label="即時復旧"
                              title="即時復旧：サーキットを解除し、次回の呼び出しでこの設定を再試行する"
                              onClick={() => void recover(m.profile_id)}
                            >
                              <RotateCcwIcon className="size-3.5" />
                            </Button>
                          )}
                        </div>
                      </div>
                      <div className="flex flex-wrap items-center gap-x-3 pl-7 text-muted-foreground text-xs">
                        <code className="truncate font-mono">{m.model}</code>
                        {!m.active && <span>優先度 {m.priority}</span>}
                      </div>
                      {m.last_error && (
                        <p className="truncate pl-7 font-mono text-muted-foreground text-xs" title={m.last_error}>
                          {m.last_error}
                        </p>
                      )}
                    </div>
                  );
                })}
                {chain.length === 0 && (
                  <div className="rounded-lg border border-dashed p-4 text-center text-muted-foreground text-sm">
                    設定がありません
                  </div>
                )}
              </div>

              <div className="rounded-lg border border-dashed p-3 text-muted-foreground text-xs leading-relaxed">
                アクティブ設定は常に第 1
                順位で、それ以外は優先度の高い順（各設定で指定）。ある設定が失敗するとクールダウンに入り（60s → 5min →
                30min）、クールダウン中はスキップされ、復旧後は自動で戻る。コンテキストウィンドウに現在のリクエストが収まらない設定もスキップされる。モデルを指定した
                Agent とタスクは既定でポーリングに参加しない。
              </div>
            </>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}

// ─────────────────────────────────────────────────────────────────────────────
// モデル設定ドロワー（新規作成 / 編集で同じフォームを共用）
// ─────────────────────────────────────────────────────────────────────────────

function ProfileSheet({
  profile,
  open,
  onOpenChange,
  onSaved,
}: {
  profile: LLMProfile | null; // null = 新規作成
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onSaved: (id: string) => void;
}) {
  const isNew = !profile;
  const [name, setName] = React.useState("");
  const [format, setFormat] = React.useState<"anthropic" | "openai" | "openai-responses">("anthropic");
  const [model, setModel] = React.useState("");
  const [baseUrl, setBaseUrl] = React.useState("");
  const [proxy, setProxy] = React.useState("");
  const [apiKey, setApiKey] = React.useState("");
  const [keyHint, setKeyHint] = React.useState("");
  const [rps, setRps] = React.useState("0");
  const [rpm, setRpm] = React.useState("0");
  const [cw, setCw] = React.useState("0"); // コンテキストウィンドウ（K tokens）。0 = 既定 200K
  const [thinkingType, setThinkingType] = React.useState(NONE);
  const [effort, setEffort] = React.useState(NONE);
  const [priority, setPriority] = React.useState("0"); // ポーリング順位。大きいほど先
  const [poolExclude, setPoolExclude] = React.useState(false);
  const [streaming, setStreaming] = React.useState(true); // true = ストリーミング（既定）、false = 非ストリーミング
  const [maxTokens, setMaxTokens] = React.useState("0"); // 1 回の応答の出力上限。0 = 送信しない
  const [maxTokensField, setMaxTokensField] = React.useState(NONE); // 上限に使うフィールド名。NONE = max_tokens
  const [sessionHeaderKey, setSessionHeaderKey] = React.useState(""); // カスタムセッションヘッダー名。空 = 送信しない
  const [retry, setRetry] = React.useState<LLMRetryOverride>(ZERO_OVERRIDE); // この設定のリトライ上書き。すべて 0 = グローバルに従う
  const [testing, setTesting] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [models, setModels] = React.useState<string[]>([]);
  const [loadingModels, setLoadingModels] = React.useState(false);
  const [modelsOpen, setModelsOpen] = React.useState(false);

  // 開くたびに渡された profile からフォームへ値を流し込む（新規なら既定値に戻す）。ドロワーを閉じて
  // 開き直せばまっさらな状態から始まり、前の設定の残像を引きずらない。
  React.useEffect(() => {
    if (!open) return;
    setName(profile?.name ?? "");
    setFormat(profile?.format === "openai" || profile?.format === "openai-responses" ? profile.format : "anthropic");
    setModel(profile?.model ?? "");
    setBaseUrl(profile?.base_url ?? "");
    setProxy(profile?.proxy ?? "");
    setRps(String(profile?.rate_per_second ?? 0));
    setRpm(String(profile?.rate_per_minute ?? 0));
    setCw(String(profile?.context_window_k ?? 0));
    setThinkingType(fromStore(profile?.thinking_type));
    setEffort(fromStore(profile?.reasoning_effort));
    setPriority(String(profile?.priority ?? 0));
    setPoolExclude(profile?.pool_exclude ?? false);
    setStreaming(profile?.streaming ?? true);
    setMaxTokens(String(profile?.max_tokens ?? 0));
    setMaxTokensField(fromStore(profile?.max_tokens_field));
    setSessionHeaderKey(profile?.session_header_key ?? "");
    setRetry(profile?.retry ?? ZERO_OVERRIDE);
    setApiKey("");
    setKeyHint(profile?.api_key_hint ?? "");
    setModels([]);
    setModelsOpen(false);
  }, [open, profile]);

  const profileId = profile ? Number(profile.id) : undefined;

  async function loadModels() {
    if (loadingModels) return;
    setLoadingModels(true);
    setModels([]);
    try {
      const r = await api.fetchLLMModels(format, baseUrl, apiKey, proxy, profileId);
      if (r.ok && r.models && r.models.length > 0) {
        setModels(r.models);
        setModelsOpen(true);
        toast.success(`${r.models.length} 個のモデルを読み込みました`);
      } else {
        toast.error(`モデルの読み込みに失敗しました：${r.error ?? "モデルを取得できませんでした"}`);
      }
    } catch (e) {
      toast.error(`モデルの読み込みでエラーが発生しました：${(e as Error).message}`);
    } finally {
      setLoadingModels(false);
    }
  }

  async function testConnection() {
    if (testing) return;
    setTesting(true);
    try {
      // 設定が実際に使う思考パラメータでテストする。こうすればそのフィールドをサポートしないモデルは
      // タスク実行時に初めて壊れるのではなく、ここで失敗する。profile id を渡すと、Key 入力欄が空のときは保存済みの Key を使う。
      const r = await api.testLLM(
        format,
        model,
        baseUrl,
        apiKey,
        proxy,
        toStore(thinkingType),
        toStore(effort),
        profileId,
        streaming,
        sessionHeaderKey.trim(),
      );
      // 応答内容も併せて表示する。モデルが実際に話したことが見えてこそ、セッション内で通ったのと同じと言える。
      if (r.ok)
        toast.success(`接続成功 · ${r.latency_ms ?? "?"}ms · ${r.model ?? model}`, {
          description: r.reply ? `応答：${r.reply}` : undefined,
        });
      else toast.error(`接続に失敗しました：${r.error ?? "不明"}`);
    } catch (e) {
      toast.error(`テストでエラーが発生しました：${(e as Error).message}`);
    } finally {
      setTesting(false);
    }
  }

  async function save() {
    if (!name.trim() || !model.trim()) {
      toast.error("名前とモデルを入力してください");
      return;
    }
    if (saving) return;
    setSaving(true);
    try {
      const { id } = await api.saveLLMProfile({
        ...(profile ? { id: Number(profile.id) } : {}),
        name: name.trim(),
        format,
        model: model.trim(),
        base_url: baseUrl.trim(),
        proxy: proxy.trim(),
        api_key: apiKey,
        rate_per_second: Number(rps) || 0,
        rate_per_minute: Number(rpm) || 0,
        context_window_k: Number(cw) || 0,
        thinking_type: toStore(thinkingType),
        reasoning_effort: toStore(effort),
        priority: Number(priority) || 0,
        pool_exclude: poolExclude,
        streaming,
        max_tokens: Math.max(0, Number(maxTokens) || 0),
        // フィールド名の切り替えは openai（Chat Completions）でのみ意味を持つ。他の形式は一律で既定へ戻す。
        // バックエンドでも同じ正規化を行うが、ここでは UI が矛盾した値を送らないようにしておく。
        max_tokens_field: format === "openai" ? toStore(maxTokensField) : "",
        session_header_key: sessionHeaderKey.trim(),
        retry,
      });
      if (isNew) toast.success(`新規作成しました：${name.trim()}（カードの「アクティブに設定」で有効化）`);
      else
        toast.success(
          profile?.is_default ? "保存しました。アクティブ設定は即時反映され、再起動は不要です" : "保存しました",
        );
      onSaved(String(id));
      onOpenChange(false);
    } catch (e) {
      toast.error(`保存に失敗しました：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="flex flex-col gap-0 p-0 data-[side=right]:min-w-[420px] data-[side=right]:sm:max-w-xl"
      >
        <SheetHeader className="px-4">
          <SheetTitle className="flex items-center gap-2">
            {isNew ? "モデル設定を新規作成" : `編集：${profile?.name}`}
            {profile?.is_default && (
              <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                アクティブ
              </Badge>
            )}
          </SheetTitle>
          <SheetDescription>
            {isNew
              ? "新規作成しても自動ではアクティブになりません。カードの「アクティブに設定」で有効化してください。"
              : "変更後は保存をクリック。アクティブ設定は保存後、すべての Agent に即時反映されます。"}
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="p-name">名前</Label>
              <Input id="p-name" placeholder="例：OpenAI 本番" value={name} onChange={(e) => setName(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label>形式</Label>
              <Select value={format} onValueChange={(v) => setFormat(v as "anthropic" | "openai" | "openai-responses")}>
                <SelectTrigger>
                  <SelectValue placeholder="形式を選択" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="anthropic">Anthropic</SelectItem>
                  <SelectItem value="openai">OpenAI (Chat Completions)</SelectItem>
                  <SelectItem value="openai-responses">OpenAI (Responses API)</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-model">モデル</Label>
            <div className="flex gap-2">
              <Input
                id="p-model"
                className="font-mono"
                placeholder="claude-opus-4-8"
                value={model}
                onChange={(e) => setModel(e.target.value)}
              />
              {/* modal：この Popover の内容は <body> に portal され、Sheet のスクロールロックの外にある。
                  modal を付けないとリストは描画されてもスクロールできない。modal にすると自身が最上位のスクロールロックを持つ。 */}
              <Popover open={modelsOpen} onOpenChange={setModelsOpen} modal>
                <PopoverTrigger asChild>
                  <Button
                    type="button"
                    variant="outline"
                    size="icon"
                    className="shrink-0"
                    disabled={loadingModels}
                    onClick={loadModels}
                    title="API から利用可能なモデルを読み込む"
                  >
                    {loadingModels ? <Loader2Icon className="animate-spin" /> : <RefreshCwIcon />}
                  </Button>
                </PopoverTrigger>
                {models.length > 0 && (
                  <PopoverContent className="max-h-72 w-72 gap-0 overflow-y-auto overscroll-contain p-1" align="end">
                    {models.map((m) => (
                      <button
                        key={m}
                        type="button"
                        className="w-full shrink-0 rounded-md px-2 py-1.5 text-left font-mono text-xs hover:bg-accent hover:text-accent-foreground"
                        onClick={() => {
                          setModel(m);
                          setModelsOpen(false);
                        }}
                      >
                        {m}
                      </button>
                    ))}
                  </PopoverContent>
                )}
              </Popover>
            </div>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-base-url">Base URL（任意）</Label>
            <Input
              id="p-base-url"
              className="font-mono"
              placeholder="https://api.openai.com/v1"
              value={baseUrl}
              onChange={(e) => setBaseUrl(e.target.value)}
            />
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-proxy">プロキシ（任意）</Label>
            <Input
              id="p-proxy"
              className="font-mono"
              placeholder="socks5://user:pass@127.0.0.1:1080 · http://127.0.0.1:8080"
              value={proxy}
              onChange={(e) => setProxy(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">
              LLM の送信リクエストのみこのプロキシを通す。http/https/socks5 に対応し、認証情報も指定可能（例：
              socks5://user:pass@host:port。パスワードに特殊文字を含む場合は URL
              エンコードが必要）。空欄ならプロキシを使わず直接接続する。
            </p>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-session-header">カスタムセッションヘッダー（任意）</Label>
            <Input
              id="p-session-header"
              className="font-mono"
              placeholder="例：x-session-id（空欄 = 送信しない）"
              value={sessionHeaderKey}
              onChange={(e) => setSessionHeaderKey(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">
              ヘッダー名を入力すると、毎回のリクエストにこの HTTP ヘッダーが付与され、値には自動で{" "}
              <b>現在のセッションの session id</b> が入る（chat セッションなら conv-12、worker なら exp3-worker-i87
              など）。session-id ヘッダーでプロンプトキャッシュ /
              スティッキールーティングを行うゲートウェイ向け。同一セッション内の複数ターンでは安定し、セッションごとに異なる値になる。空欄なら送信しない。
            </p>
          </div>

          <div className="grid gap-2">
            <Label htmlFor="p-api-key">API Key</Label>
            <Input
              id="p-api-key"
              type="password"
              placeholder={keyHint ? `設定済み（${keyHint}）。空欄なら変更しない` : "sk-…"}
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
            />
          </div>

          <div className="grid gap-4 sm:grid-cols-3">
            <div className="grid gap-2">
              <Label htmlFor="p-rps">毎秒レート制限</Label>
              <Input id="p-rps" type="number" min={0} value={rps} onChange={(e) => setRps(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="p-rpm">毎分レート制限</Label>
              <Input id="p-rpm" type="number" min={0} value={rpm} onChange={(e) => setRpm(e.target.value)} />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="p-cw">コンテキストウィンドウ（K）</Label>
              <Input
                id="p-cw"
                type="number"
                min={0}
                max={1000}
                value={cw}
                onChange={(e) => setCw(e.target.value)}
                placeholder="200"
              />
            </div>
          </div>
          <p className="-mt-2 text-muted-foreground text-xs">
            レート制限 0 = 無制限、すべての Agent で共有。コンテキストウィンドウの単位は K（千 token）で、0 = 既定
            200K、上限 1000（つまり 1M）。高く設定しすぎると圧縮が発動しなくなる。
          </p>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-4">
              <div className="grid gap-0.5">
                <Label htmlFor="p-priority" className="text-sm">
                  ポーリング優先度
                </Label>
                <p className="text-muted-foreground text-xs">
                  数値が大きいほど先に選ばれる。アクティブ設定はこの値に関係なく常に第 1
                  順位。同じ優先度の設定は順番に先頭を務めるため、自然に枠を分担できる。
                </p>
              </div>
              <Input
                id="p-priority"
                type="number"
                className="w-24 shrink-0"
                value={priority}
                onChange={(e) => setPriority(e.target.value)}
              />
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">ポーリングに参加しない</Label>
                <p className="text-muted-foreground text-xs">
                  有効にするとフェイルオーバー先として選ばれなくなる（Agent /
                  タスクからの明示指定では引き続き使用可能）。「特定の Agent
                  専用にし、他の失敗時に消費されたくない」高価な設定に向く。
                </p>
              </div>
              <Switch checked={poolExclude} onCheckedChange={setPoolExclude} aria-label="ポーリングに参加しない" />
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">ストリーミング出力 · streaming</Label>
                <p className="text-muted-foreground text-xs">
                  有効（既定）ならストリーミング SSE を使い、実行中のリアルタイム進捗とリアルタイム token
                  カウントが得られる。無効なら真の非ストリーミング（stream:false、
                  応答を一括で返す）となり、一部ゲートウェイの不出来な SSE 実装（空フレーム /
                  思考フィールドの欠落）を回避できる。 代償として実行中のリアルタイム進捗は失われる。
                </p>
              </div>
              <Switch checked={streaming} onCheckedChange={setStreaming} aria-label="ストリーミング出力" />
            </div>
          </div>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-4">
              <div className="grid gap-0.5">
                <Label htmlFor="p-max-tokens" className="text-sm">
                  出力上限 · max tokens
                </Label>
                <p className="text-muted-foreground text-xs">
                  1 回の応答で生成する token の上限。毎回のリクエストに含めて送信する。0（既定）=
                  このフィールドを送信せず、サーバー側の既定値に任せる。
                  上記の「コンテキストウィンドウ」とは別物で、あちらはモデルの総容量であり、ローカルで圧縮しきい値の計算に使うだけ。
                  小さくしすぎると推論モデルが思考段階で打ち切られ、答えが 1 文字も出なくなる。
                </p>
              </div>
              <Input
                id="p-max-tokens"
                type="number"
                min={0}
                className="w-28 shrink-0"
                value={maxTokens}
                onChange={(e) => setMaxTokens(e.target.value)}
                placeholder="0"
              />
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">上限フィールド名</Label>
                <p className="text-muted-foreground text-xs">{MAX_TOKENS_FIELD_HINTS[format]}</p>
              </div>
              <Select
                value={format === "openai" ? maxTokensField : NONE}
                onValueChange={setMaxTokensField}
                disabled={format !== "openai"}
              >
                <SelectTrigger className="w-56 shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {MAX_TOKENS_FIELDS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="grid gap-3 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-4">
              <div className="grid gap-0.5">
                <Label className="text-sm">思考スイッチ · thinking.type</Label>
                <p className="text-muted-foreground text-xs">
                  thinking フィールドを送信するかどうか。送信しない = フィールドを付けない（MiniMax
                  など未対応モデルとの互換用）。オフ = disabled を送信、オン = enabled を送信。下の強度とは互いに独立。
                </p>
              </div>
              <Select value={thinkingType} onValueChange={setThinkingType}>
                <SelectTrigger className="w-32 shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {THINKING_TYPES.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="flex items-center justify-between gap-4 border-t pt-3">
              <div className="grid gap-0.5">
                <Label className="text-sm">思考強度 · reasoning_effort</Label>
                <p className="text-muted-foreground text-xs">
                  独立した強度の段階（OpenAI reasoning_effort / Anthropic output_config.effort）。thinking
                  フィールドを持たず強度だけで思考を有効化できる API
                  もあるため、単独で設定でき、思考スイッチは送信しない。
                </p>
              </div>
              <Select value={effort} onValueChange={setEffort}>
                <SelectTrigger className="w-32 shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {EFFORT_LEVELS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <ProfileRetryFields value={retry} onChange={setRetry} />
        </div>

        <div className="flex gap-2 border-t px-4 py-3">
          <Button variant="outline" onClick={testConnection} disabled={testing}>
            {testing ? <Loader2Icon className="animate-spin" /> : <PlugZapIcon />}
            {testing ? "テスト中…" : "接続テスト"}
          </Button>
          <Button onClick={save} disabled={saving} className="flex-1">
            {saving && <Loader2Icon className="animate-spin" />}
            {!saving && (isNew ? <PlusIcon /> : <SaveIcon />)}
            {isNew ? "作成" : "保存"}
          </Button>
        </div>
      </SheetContent>
    </Sheet>
  );
}

// ─────────────────────────────────────────────────────────────────────────────

export default function LLMPage() {
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [pool, setPool] = React.useState<LLMPoolStatus | null>(null);
  const [poolOpen, setPoolOpen] = React.useState(false);
  // ドロワーの開閉状態と内容を別々に保持する。閉じている間も editing を保つことで、
  // 閉じるアニメーション中にタイトルが「編集：X」から「新規作成」へちらつくのを防ぐ。editing = null は新規作成を表す。
  const [editOpen, setEditOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<LLMProfile | null>(null);
  const openEditor = React.useCallback((p: LLMProfile | null) => {
    setEditing(p);
    setEditOpen(true);
  }, []);

  const loadPool = React.useCallback(async () => {
    try {
      setPool(await api.llmPool());
    } catch {
      /* ignore */
    }
  }, []);

  const load = React.useCallback(async () => {
    try {
      setProfiles(await api.llmProfiles());
    } catch {
      /* ignore */
    }
    await loadPool();
  }, [loadPool]);

  React.useEffect(() => {
    void load();
  }, [load]);

  // カードのヘルスバッジは profile id ごとにポーリング状態を取得する。
  const health = React.useMemo(() => {
    const m = new Map<string, LLMPoolMember>();
    for (const c of pool?.chain ?? []) m.set(c.profile_id, c);
    return m;
  }, [pool]);

  async function activate(id: string, name: string) {
    try {
      await api.activateLLMProfile(id);
      toast.success(`アクティブにしました：${name}`);
      await load();
    } catch (e) {
      toast.error(`アクティブ化に失敗しました：${(e as Error).message}`);
    }
  }

  async function remove(p: LLMProfile) {
    if (p.is_default) {
      toast.error("現在アクティブな設定は削除できません");
      return;
    }
    try {
      await api.deleteLLMProfile(p.id);
      toast.success(`削除しました：${p.name}`);
      await load();
    } catch (e) {
      toast.error(`削除に失敗しました：${(e as Error).message}`);
    }
  }

  const poolOn = pool?.enabled ?? false;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="font-semibold text-xl tracking-tight">LLM</h1>
          <p className="text-muted-foreground text-sm">
            すべての Agent で共有される形式 / モデル /
            レート制限の設定。カードをクリックで編集、星印が現在のアクティブ設定。
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button size="sm" variant="outline" onClick={() => setPoolOpen(true)}>
            <ZapIcon /> ポーリング設定
            {poolOn && (
              <Badge variant="outline" className="ml-1 border-emerald-500/50 text-emerald-600 dark:text-emerald-400">
                有効
              </Badge>
            )}
          </Button>
          <Button size="sm" variant="outline" onClick={() => openEditor(null)}>
            <PlusIcon /> 新規
          </Button>
        </div>
      </div>

      <Tabs defaultValue="profiles" className="flex-1">
        <TabsList>
          <TabsTrigger value="profiles">モデル設定</TabsTrigger>
          <TabsTrigger value="retry">リトライとバックオフ</TabsTrigger>
        </TabsList>

        <TabsContent value="profiles" className="mt-4">
          <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
            {profiles.map((p) => {
              const h = healthOf(p, health.get(p.id));
              return (
                // biome-ignore lint/a11y/useSemanticElements: カード内に独自の操作ボタンがあるため、ネイティブ <button> を使うとボタンの入れ子（不正な HTML）になる
                <Card
                  key={p.id}
                  role="button"
                  tabIndex={0}
                  onClick={() => openEditor(p)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      openEditor(p);
                    }
                  }}
                  className={cn(
                    "cursor-pointer gap-0 py-4 outline-none transition-colors hover:border-foreground/30",
                    p.is_default && "border-amber-400/50 bg-amber-400/5",
                  )}
                >
                  <CardContent className="grid gap-2 px-4">
                    <div className="flex items-start gap-2">
                      <StarIcon
                        className={cn(
                          "mt-0.5 size-4 shrink-0",
                          p.is_default ? "fill-amber-400 text-amber-400" : "text-muted-foreground",
                        )}
                      />
                      <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="truncate font-medium text-sm">{p.name}</span>
                          <Badge variant="outline" className="uppercase">
                            {p.format}
                          </Badge>
                          <Badge variant="outline" className={cn("ml-auto", h.cls)} title={h.hint}>
                            {h.label}
                          </Badge>
                        </div>
                        <code className="mt-1 block truncate font-mono text-muted-foreground text-xs">{p.model}</code>
                      </div>
                    </div>

                    <div className="flex flex-wrap gap-x-3 gap-y-0.5 pl-6 text-muted-foreground text-xs">
                      {p.api_key_hint && <span>{p.api_key_hint}</span>}
                      <span>
                        {p.rate_per_second}/s · {p.rate_per_minute}/min
                      </span>
                      {p.proxy && <span className="truncate">プロキシ {p.proxy}</span>}
                      {p.reasoning_effort && (
                        <span>思考 {p.reasoning_effort === "off" ? "オフ" : p.reasoning_effort}</span>
                      )}
                      {/* ポーリング関連の 2 フィールドはポーリング有効時のみ意味を持つため、無効時は表示しない */}
                      {poolOn &&
                        !p.is_default &&
                        (p.pool_exclude ? <span>ポーリング不参加</span> : <span>優先度 {p.priority ?? 0}</span>)}
                    </div>

                    <div className="mt-1 flex gap-2">
                      <Button
                        size="sm"
                        variant="outline"
                        className="flex-1"
                        disabled={p.is_default}
                        onClick={(e) => {
                          e.stopPropagation();
                          void activate(p.id, p.name);
                        }}
                      >
                        {p.is_default ? "アクティブ" : "アクティブに設定"}
                      </Button>
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label="設定を削除"
                        onClick={(e) => {
                          e.stopPropagation();
                          void remove(p);
                        }}
                      >
                        <Trash2Icon className="text-destructive" />
                      </Button>
                    </div>
                  </CardContent>
                </Card>
              );
            })}
            {profiles.length === 0 && (
              <div className="col-span-full rounded-lg border border-dashed p-10 text-center text-muted-foreground text-sm">
                モデル設定がまだありません。右上の「新規」から最初の設定を作成してください。
              </div>
            )}
          </div>
        </TabsContent>

        <TabsContent value="retry" className="mt-4">
          <RetryPolicyPanel />
        </TabsContent>
      </Tabs>

      <ProfileSheet profile={editing} open={editOpen} onOpenChange={setEditOpen} onSaved={() => void load()} />
      <PoolSheet open={poolOpen} onOpenChange={setPoolOpen} pool={pool} onReload={loadPool} />
    </div>
  );
}
