"use client";

import * as React from "react";

import { BellIcon, PlusIcon, SendIcon, Trash2Icon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { NotificationChannel, NotificationFilter, NotificationMeta } from "@/lib/types";

import {
  CHANNEL_FIELDS,
  type ChannelForm,
  emptyForm,
  KIND_LABEL,
  parseIDs,
  parseKeywords,
  parseKV,
  SEVERITY_OPTIONS,
} from "./_components/channel-fields";
import { ConfigField, FilterSummary } from "./_components/channel-form";
import { DeliveryList } from "./_components/delivery-list";
import { formatBacklog, StatTile } from "./_components/stat-tile";

// このページはオーケストレーションのみを担う：データの読み込み、フォーム状態の維持、
// API の呼び出し。フィールド定義と解析は _components/channel-fields.ts、コントロールと
// フィルタ要約は _components/channel-form.tsx、配信記録は
// _components/delivery-list.tsx にある——分割したのはそれぞれ単独で読めるからで、
// 1 ファイルに詰め込むとこのページは 1100 行近くになる。
export default function NotifyPage() {
  const [meta, setMeta] = React.useState<NotificationMeta | null>(null);
  const [channels, setChannels] = React.useState<NotificationChannel[]>([]);
  const [tab, setTab] = React.useState<"channels" | "deliveries">("channels");

  const [open, setOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<NotificationChannel | null>(null);
  const [form, setForm] = React.useState<ChannelForm>(emptyForm("dingtalk"));
  const [saving, setSaving] = React.useState(false);
  const [testing, setTesting] = React.useState(false);

  const [globalSaving, setGlobalSaving] = React.useState(false);
  const [baseURL, setBaseURL] = React.useState("");
  const [digestMin, setDigestMin] = React.useState("");

  const load = React.useCallback(() => {
    api
      .notifyMeta()
      .then((m) => {
        setMeta(m);
        setBaseURL(m.public_base_url);
        setDigestMin(m.digest_interval_min);
      })
      .catch((e) => toast.error("通知設定の読み込みに失敗しました：" + (e as Error).message));
    // チャネル一覧の読み込み失敗は必ず通知する：静かに失敗すると「チャネルが
    // 1 つもない」ように見え、ユーザーは設定が消えたと勘違いして、直接エラーを
    // 出すより不安になるため。
    api
      .notifyChannels()
      .then(setChannels)
      .catch((e) => toast.error("チャネル一覧の読み込みに失敗しました：" + (e as Error).message));
  }, []);
  React.useEffect(() => {
    load();
  }, [load]);

  function setF(patch: Partial<ChannelForm>) {
    setForm((f) => ({ ...f, ...patch }));
  }
  function setCfg(key: string, value: unknown) {
    setForm((f) => ({ ...f, config: { ...f.config, [key]: value } }));
  }

  function openAdd() {
    setEditing(null);
    setForm(emptyForm(meta?.kinds[0]?.kind ?? "dingtalk"));
    setOpen(true);
  }

  function openEdit(ch: NotificationChannel) {
    setEditing(ch);
    // filter はバックエンドでは Go 構造体で、常にオブジェクトにシリアライズされる（null にはならない）ため、フォールバックは不要。
    const f = ch.filter;
    setForm({
      name: ch.name,
      kind: ch.kind,
      mode: ch.mode,
      enabled: ch.enabled,
      ratePerMin: String(ch.rate_per_min),
      // バックエンドが返す config では資格情報がマスク値になっている。そのまま
      // フォームに入れ、送信時もそのまま返すことで、バックエンドは DB の元値を
      // 保持できる。
      config: { ...ch.config },
      minSeverity: f.min_severity ?? "",
      includeText: (f.vulnclass_include ?? []).join("\n"),
      excludeText: (f.vulnclass_exclude ?? []).join("\n"),
      taskIDsText: (f.task_ids ?? []).join(","),
      assetIDsText: (f.asset_ids ?? []).join(","),
      onStatusChange: f.on_status_change ?? false,
    });
    setOpen(true);
  }

  // buildConfig はフォーム状態をチャネル config へ変換する。
  //
  // ルールは 1 つだけ、値は 2 種類：
  //   - マスク値（"__masked__..."）はそのまま返す → バックエンドは「このフィールドは
  //     変更なし、DB の元値を保持」と解釈する
  //   - それ以外はユーザー入力どおりに送信し、空文字は「このフィールドをクリア」
  //
  // 資格情報フィールドを特別扱いしない（「空ならスキップ」など）のは、それだと
  // 設定ミスのある鍵をユーザーが**消せなくなる**ため——画面上に「消したい」を
  // 表現する操作が存在しなくなる。現行ルールなら入力欄を空にする＝そのフィールドを
  // 空にする、という唯一かつユーザーが制御できる意味になる。
  // マスク値は入力欄に出ない（ConfigField 参照）ので、「欄に文字がある」は常に
  // 「ユーザーが能動的に入力した」と等しい。
  function buildConfig(): Record<string, unknown> {
    const defs = CHANNEL_FIELDS[form.kind] ?? [];
    const out: Record<string, unknown> = {};
    for (const d of defs) {
      const raw = form.config[d.key];
      if (d.kind === "switch") {
        out[d.key] = raw === true;
        continue;
      }
      if (typeof raw === "string" && raw.startsWith("__masked__")) {
        out[d.key] = raw;
        continue;
      }
      if (d.kind === "number") {
        const n = Number(raw);
        out[d.key] = Number.isFinite(n) && n > 0 ? n : 0;
        continue;
      }
      if (d.kind === "kv") {
        out[d.key] = parseKV(String(raw ?? ""));
        continue;
      }
      if (d.kind === "list") {
        out[d.key] = String(raw ?? "")
          .split(/[\s,，]+/)
          .map((s) => s.trim())
          .filter(Boolean);
        continue;
      }
      out[d.key] = String(raw ?? "").trim();
    }
    return out;
  }

  function buildFilter(): NotificationFilter {
    return {
      min_severity: form.minSeverity || undefined,
      vulnclass_include: parseKeywords(form.includeText),
      vulnclass_exclude: parseKeywords(form.excludeText),
      task_ids: parseIDs(form.taskIDsText),
      asset_ids: parseIDs(form.assetIDsText),
      on_status_change: form.onStatusChange,
    };
  }

  async function saveForm() {
    if (!form.name.trim()) {
      toast.error("チャネル名を入力してください");
      return;
    }
    setSaving(true);
    try {
      const payload = {
        name: form.name.trim(),
        kind: form.kind,
        mode: form.mode,
        enabled: form.enabled,
        config: buildConfig(),
        filter: buildFilter(),
        rate_per_min: form.ratePerMin.trim() === "" ? undefined : Number(form.ratePerMin),
      };
      if (editing) {
        await api.notifyUpdateChannel(editing.id, payload);
        toast.success("保存しました");
        setOpen(false);
      } else {
        await api.notifyCreateChannel(payload);
        toast.success("チャネルを追加しました");
        setOpen(false);
      }
      load();
    } catch (e) {
      toast.error("保存に失敗しました：" + (e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  async function testChannel() {
    if (!editing) return;
    setTesting(true);
    try {
      const r = await api.notifyTestChannel(editing.id);
      toast.success(`テストメッセージを送信しました（${r.latency_ms} ms）。グループで確認してください`);
    } catch (e) {
      // バックエンドはチャネルが返した生のエラーをそのまま返す。設定調査の唯一の
      // 手がかりなので、原文のまま表示する。
      toast.error("テストに失敗しました：" + (e as Error).message, { duration: 12000 });
    } finally {
      setTesting(false);
    }
  }

  async function removeChannel(ch: NotificationChannel) {
    try {
      await api.notifyDeleteChannel(ch.id);
      toast.success(`削除しました：${ch.name}`);
      setOpen(false);
      load();
    } catch (e) {
      toast.error("削除に失敗しました：" + (e as Error).message);
    }
  }

  async function toggleEnabled(ch: NotificationChannel) {
    try {
      await api.notifyUpdateChannel(ch.id, { enabled: !ch.enabled });
      load();
    } catch (e) {
      toast.error("操作に失敗しました：" + (e as Error).message);
    }
  }

  async function toggleGlobal(on: boolean) {
    setGlobalSaving(true);
    try {
      await api.setSettings({ notify_enabled: on });
      setMeta((m) => (m ? { ...m, enabled: on } : m));
      toast.success(on ? "通知を有効にしました" : "通知を一時停止しました");
    } catch (e) {
      toast.error("操作に失敗しました：" + (e as Error).message);
    } finally {
      setGlobalSaving(false);
    }
  }

  async function saveGlobal() {
    setGlobalSaving(true);
    try {
      const patch: Record<string, unknown> = { notify_public_base_url: baseURL.trim() };
      const n = Number(digestMin);
      if (Number.isFinite(n) && n > 0) patch.notify_digest_interval_min = n;
      await api.setSettings(patch);
      toast.success("保存しました");
      load();
    } catch (e) {
      toast.error("保存に失敗しました：" + (e as Error).message);
    } finally {
      setGlobalSaving(false);
    }
  }

  const fields = CHANNEL_FIELDS[form.kind] ?? [];
  const secretKeys = new Set(meta?.kinds.find((k) => k.kind === form.kind)?.secret_keys ?? []);
  const defaultRate = meta?.kinds.find((k) => k.kind === form.kind)?.default_rate_per_min ?? 0;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">通知配信</h1>
          <p className="text-muted-foreground text-sm">
            脆弱性の検出時に DingTalk / Lark / WeCom などのチャネルへ配信 ·
            チャネルごとに配信タイミングとフィルタルールを個別に設定可能
          </p>
        </div>
        {meta && (
          // label ではなく div を使う：Switch は aria-label を自前で持つため、外側に
          // label を重ねてもネイティブコントロールと関連づかず、テキストをクリックすると
          // 切り替わりそうに見えてしまう。
          <div className="flex shrink-0 items-center gap-2 text-sm">
            <span className="text-muted-foreground">全体スイッチ</span>
            <Switch
              checked={meta.enabled}
              disabled={globalSaving}
              onCheckedChange={toggleGlobal}
              aria-label="通知の全体スイッチ"
            />
          </div>
        )}
      </div>

      {meta && (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
          <StatTile label="チャネル" value={`${meta.stats.channels_on} / ${meta.stats.channels}`} hint="有効 / 合計" />
          <StatTile label="本日の配信済み" value={String(meta.stats.sent_today)} />
          <StatTile label="送信待ち" value={String(meta.stats.pending)} />
          <StatTile label="失敗" value={String(meta.stats.failed)} tone={meta.stats.failed > 0 ? "red" : undefined} />
          <StatTile
            label="最長バックログ"
            value={formatBacklog(meta.stats.backlog_age_ms)}
            // バックログの経過時間は件数よりはるかに有用：3 件のバックログが 3 秒前
            // なのか 3 時間前なのかは大きく違う。
            hint={meta.stats.backlog_age_ms > 5 * 60_000 ? "配信が詰まっている可能性" : undefined}
            tone={meta.stats.backlog_age_ms > 5 * 60_000 ? "red" : undefined}
          />
        </div>
      )}

      <Card className="gap-3">
        <CardHeader>
          <CardTitle className="text-base">グローバル設定</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4 sm:grid-cols-2">
          <div className="grid gap-2">
            <Label htmlFor="n-base">リンク先ベース URL</Label>
            <Input
              id="n-base"
              placeholder="https://artex.example.com"
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">
              メッセージ内の「詳細を見る」ボタンのリンク先。空欄ならボタンを付けません。
            </p>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="n-digest">集約周期（分）</Label>
            <Input
              id="n-digest"
              type="number"
              min={1}
              max={1440}
              placeholder="30"
              value={digestMin}
              onChange={(e) => setDigestMin(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">「集約」モードのチャネルにのみ適用されます。</p>
          </div>
          <div className="sm:col-span-2">
            <Button onClick={saveGlobal} disabled={globalSaving}>
              グローバル設定を保存
            </Button>
          </div>
        </CardContent>
      </Card>

      <Tabs value={tab} onValueChange={(v) => setTab(v as "channels" | "deliveries")} className="flex flex-col gap-4">
        <TabsList>
          <TabsTrigger value="channels">チャネル</TabsTrigger>
          <TabsTrigger value="deliveries">配信記録</TabsTrigger>
        </TabsList>

        <TabsContent value="channels">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
            <button
              type="button"
              onClick={openAdd}
              className="text-foreground/70 border-foreground/70 hover:bg-muted/60 hover:shadow-sm flex min-h-[130px] flex-col items-center justify-center gap-2 rounded-xl border border-dashed transition"
            >
              <PlusIcon className="size-6" />
              <span className="text-sm">チャネルを追加</span>
            </button>

            {channels.map((ch) => (
              <Card
                key={ch.id}
                onClick={() => openEdit(ch)}
                className="hover:border-primary/60 cursor-pointer gap-3 transition hover:shadow-sm"
              >
                <CardHeader>
                  <div className="flex items-center gap-2">
                    <BellIcon className="text-muted-foreground size-4 shrink-0" />
                    <CardTitle className="truncate text-base">{ch.name}</CardTitle>
                    {/* カード全体がクリック可能（編集へ遷移）なので、この 2 つの
                        コントロールはそれぞれバブリングを止める必要がある。止めないと
                        スイッチ/削除が編集も同時に発火させる。stopPropagation は
                        コントロール自身に付け、div で包まない：包むと「操作できそうで
                        役割を持たない」静的要素が生まれ、a11y 警告を誘発するうえ
                        意味論的にも筋が通らない。 */}
                    <div className="ml-auto flex items-center gap-2">
                      <Switch
                        checked={ch.enabled}
                        onCheckedChange={() => toggleEnabled(ch)}
                        onClick={(e) => e.stopPropagation()}
                        aria-label="有効化"
                      />
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label="削除"
                        onClick={(e) => {
                          e.stopPropagation();
                          // void で Promise を明示的に破棄する：removeChannel 自身が
                          // catch して toast するので、ここで await する必要はない
                          // （onClick は async ではない）。
                          void removeChannel(ch);
                        }}
                      >
                        <Trash2Icon className="text-destructive" />
                      </Button>
                    </div>
                  </div>
                </CardHeader>
                <CardContent className="grid gap-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge variant="outline">{KIND_LABEL[ch.kind] ?? ch.kind}</Badge>
                    <Badge variant="outline">{ch.mode === "digest" ? "集約" : "リアルタイム"}</Badge>
                    {!ch.enabled && <Badge variant="outline">無効化済み</Badge>}
                  </div>
                  <FilterSummary filter={ch.filter} />
                </CardContent>
              </Card>
            ))}
          </div>
        </TabsContent>

        <TabsContent value="deliveries">
          <DeliveryList channels={channels} />
        </TabsContent>
      </Tabs>

      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent side="right" className="w-full data-[side=right]:sm:max-w-lg">
          <SheetHeader>
            <SheetTitle>{editing ? editing.name : "通知チャネルを追加"}</SheetTitle>
            <SheetDescription>
              {KIND_LABEL[form.kind] ?? form.kind}
              {defaultRate > 0 ? ` · 既定レート制限 ${defaultRate} 件/分` : " · レート制限なし"}
            </SheetDescription>
          </SheetHeader>

          <div className="flex min-h-0 flex-1 flex-col overflow-y-auto px-4">
            <div className="grid gap-4 py-4">
              <div className="grid gap-2">
                <Label>チャネル種別</Label>
                <Select
                  value={form.kind}
                  onValueChange={(v) => {
                    // 種別を変える＝資格情報フィールドの一式が変わるので、旧設定をマージしてはならない。
                    setF({ kind: v, config: {} });
                  }}
                  disabled={!!editing}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {(meta?.kinds ?? []).map((k) => (
                      <SelectItem key={k.kind} value={k.kind}>
                        {KIND_LABEL[k.kind] ?? k.kind}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {editing && (
                  <p className="text-muted-foreground text-xs">
                    チャネル種別は変更できません。種別を変えると資格情報の一式が変わるため、新規にチャネルを作成してください。
                  </p>
                )}
              </div>

              <div className="grid gap-2">
                <Label htmlFor="n-name">チャネル名</Label>
                <Input
                  id="n-name"
                  placeholder="インシデント対応グループ / 日次レポートグループ"
                  value={form.name}
                  onChange={(e) => setF({ name: e.target.value })}
                />
              </div>

              {fields.length === 0 ? (
                <p className="text-muted-foreground text-sm">
                  このチャネルのフォームは未定義です（フロントエンドに CHANNEL_FIELDS
                  の項目がありません）。追加してから再試行してください。
                </p>
              ) : (
                fields.map((d) => (
                  <ConfigField
                    key={d.key}
                    def={d}
                    value={form.config[d.key]}
                    isSecret={secretKeys.has(d.key)}
                    onChange={(v) => setCfg(d.key, v)}
                  />
                ))
              )}

              <div className="grid gap-2">
                <Label>配信タイミング</Label>
                <Select value={form.mode} onValueChange={(v) => setF({ mode: v as "realtime" | "digest" })}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="realtime">リアルタイム · 脆弱性ごとに 1 件ずつ送信</SelectItem>
                    <SelectItem value="digest">集約 · 周期ごとに 1 件へまとめて送信</SelectItem>
                  </SelectContent>
                </Select>
                <p className="text-muted-foreground text-xs">
                  「高以上はリアルタイム、それ以外は集約」にしたい場合はチャネルを 2 つ作成します：リアルタイム +
                  しきい値「高以上」と、集約 + レベル指定なし。
                </p>
              </div>

              <div className="grid gap-2">
                <Label htmlFor="n-rate">レート制限（件/分）</Label>
                <Input
                  id="n-rate"
                  type="number"
                  min={0}
                  placeholder={defaultRate > 0 ? String(defaultRate) : "0 = 制限なし"}
                  value={form.ratePerMin}
                  onChange={(e) => setF({ ratePerMin: e.target.value })}
                />
                <p className="text-muted-foreground text-xs">
                  空欄ならチャネルの既定値。0 は制限なし。上限を超えてもメッセージは失われず、送信が遅れるだけです。
                </p>
              </div>

              <div className="border-t pt-4">
                <p className="mb-3 text-sm font-medium">フィルタルール（空欄ならフィルタなし）</p>
                <div className="grid gap-4">
                  <div className="grid gap-2">
                    <Label>最低レベル</Label>
                    <Select
                      value={form.minSeverity || "all"}
                      onValueChange={(v) => setF({ minSeverity: v === "all" ? "" : v })}
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {SEVERITY_OPTIONS.map((o) => (
                          <SelectItem key={o.value || "all"} value={o.value || "all"}>
                            {o.label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-inc">この脆弱性種別のみ配信</Label>
                    <Textarea
                      id="n-inc"
                      placeholder={"SQL インジェクション\nコマンド実行"}
                      value={form.includeText}
                      onChange={(e) => setF({ includeText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">
                      1 行に 1 キーワード。大文字小文字を区別しない部分一致です。空欄＝すべての種別。
                    </p>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-exc">この脆弱性種別を除外</Label>
                    <Textarea
                      id="n-exc"
                      placeholder={"情報漏洩"}
                      value={form.excludeText}
                      onChange={(e) => setF({ excludeText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">
                      除外は包含より優先されます：両方に一致した場合は除外されます。
                    </p>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-tasks">タスク ID を限定</Label>
                    <Input
                      id="n-tasks"
                      placeholder="1, 2, 3"
                      value={form.taskIDsText}
                      onChange={(e) => setF({ taskIDsText: e.target.value })}
                    />
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-assets">資産 ID を限定</Label>
                    <Input
                      id="n-assets"
                      placeholder="10, 11"
                      value={form.assetIDsText}
                      onChange={(e) => setF({ assetIDsText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">
                      タスク/資産が空欄＝限定なし。入力した場合は脆弱性と共通のタスク/資産を持つことが条件になります。
                    </p>
                  </div>
                  <div className="flex items-center gap-2 text-sm">
                    <Switch
                      checked={form.onStatusChange}
                      onCheckedChange={(v) => setF({ onStatusChange: v })}
                      aria-label="ステータス変更を受信"
                    />
                    脆弱性の対応ステータス変更時にも配信（リアルタイムモードのみ）
                  </div>
                </div>
              </div>

              <div className="flex items-center gap-2 text-sm">
                <Switch checked={form.enabled} onCheckedChange={(v) => setF({ enabled: v })} aria-label="有効化" />
                このチャネルを有効化
              </div>
            </div>

            <div className="flex gap-2 pt-2 pb-6">
              <Button onClick={saveForm} disabled={saving}>
                {editing ? "保存" : "追加"}
              </Button>
              {editing && (
                <Button variant="outline" onClick={testChannel} disabled={testing}>
                  <SendIcon /> テストメッセージを送信
                </Button>
              )}
            </div>
          </div>
        </SheetContent>
      </Sheet>
    </div>
  );
}
