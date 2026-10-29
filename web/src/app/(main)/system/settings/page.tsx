"use client";

import * as React from "react";

import { CpuIcon, FlaskConicalIcon, KeyboardIcon, RadioTowerIcon, SearchIcon, ShieldAlertIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import { CHAT_SEND_MODE_OPTIONS, type ChatSendMode, setChatSendMode, useChatSendMode } from "@/lib/chat-send-mode";
import type { Settings } from "@/lib/types";

import { UpdateCard } from "./_components/update-card";

export default function SystemSettingsPage() {
  const [trafficCapture, setTrafficCapture] = React.useState(false);
  const [agentTrafficBinding, setAgentTrafficBinding] = React.useState(false);
  const [webSearch, setWebSearch] = React.useState(false);
  const [backend, setBackend] = React.useState("ddgs");
  const [braveKeySet, setBraveKeySet] = React.useState(false);
  const [braveKeyInput, setBraveKeyInput] = React.useState("");
  const [tavilyKeySet, setTavilyKeySet] = React.useState(false);
  const [tavilyKeyInput, setTavilyKeyInput] = React.useState("");
  const [savingTavilyKey, setSavingTavilyKey] = React.useState(false);
  const [proxyInput, setProxyInput] = React.useState("");
  const [savingProxy, setSavingProxy] = React.useState(false);
  const [globalProxyInput, setGlobalProxyInput] = React.useState("");
  const [savingGlobalProxy, setSavingGlobalProxy] = React.useState(false);
  const [testing, setTesting] = React.useState(false);
  const [loaded, setLoaded] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [savingKey, setSavingKey] = React.useState(false);
  const [pyInterp, setPyInterp] = React.useState("");
  const [workers, setWorkers] = React.useState("3");
  const [savingWorkers, setSavingWorkers] = React.useState(false);
  // 操作制約の注入範囲（既定は両方オン）。
  const [injectPlanner, setInjectPlanner] = React.useState(true);
  const [injectWorker, setInjectWorker] = React.useState(true);
  // 実験機能：noa コンテキスト圧縮（既定はオフ）。
  const [noaCompaction, setNoaCompaction] = React.useState(false);
  // フロントエンドのみの設定：/api/settings を経由せず、localStorage を直接読み書きする。
  const sendMode = useChatSendMode();

  const apply = React.useCallback((s: Settings) => {
    setTrafficCapture(!!s.traffic_capture);
    setAgentTrafficBinding(!!s.agent_traffic_binding);
    setWebSearch(!!s.web_search_enabled);
    setBackend(s.web_search_backend || "ddgs");
    setBraveKeySet(!!s.brave_key_set);
    setTavilyKeySet(!!s.tavily_key_set);
    setProxyInput(s.web_search_proxy ?? "");
    setGlobalProxyInput(s.global_proxy ?? "");
    setPyInterp(s.python_interpreter ?? "");
    setWorkers(String(s.workers ?? 3));
    setInjectPlanner(s.constraints_inject_planner !== false);
    setInjectWorker(s.constraints_inject_worker !== false);
    setNoaCompaction(!!s.noa_compaction);
  }, []);

  const saveWorkers = () => {
    const n = Number(workers);
    if (!Number.isInteger(n) || n <= 0) {
      toast.error("同時実行数は 0 より大きい整数である必要があります");
      return;
    }
    setSavingWorkers(true);
    api
      .setSettings({ workers: n })
      .then((s) => {
        apply(s);
        toast.success("ワーカー agent の同時実行数を保存しました（以後に開始するタスクから有効）");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingWorkers(false));
  };

  const savePython = () => {
    setSaving(true);
    api
      .setSettings({ python_interpreter: pyInterp.trim() })
      .then((s) => {
        apply(s);
        toast.success("Python インタプリタの設定を保存しました");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSaving(false));
  };
  const detectPython = () => {
    setSaving(true);
    api
      .detectPython()
      .then((r) => setPyInterp(r.python_interpreter))
      .catch(() => undefined)
      .finally(() => setSaving(false));
  };

  React.useEffect(() => {
    api
      .settings()
      .then(apply)
      .catch(() => undefined)
      .finally(() => setLoaded(true));
  }, [apply]);

  const toggleTraffic = (v: boolean) => {
    setTrafficCapture(v); // optimistic
    setSaving(true);
    api
      .setSettings({ traffic_capture: v })
      .then(apply)
      .catch(() => setTrafficCapture(!v)) // revert on failure
      .finally(() => setSaving(false));
  };

  const toggleInjectPlanner = (v: boolean) => {
    setInjectPlanner(v); // optimistic
    api
      .setSettings({ constraints_inject_planner: v })
      .then(apply)
      .catch(() => setInjectPlanner(!v)); // revert on failure
  };

  const toggleAgentTrafficBinding = (v: boolean) => {
    setAgentTrafficBinding(v);
    setSaving(true);
    api
      .setSettings({ agent_traffic_binding: v })
      .then((s) => {
        apply(s);
        toast.success(
          v ? "Agent の自動トラフィック紐付けを有効にしました" : "Agent の自動トラフィック紐付けを無効にしました",
        );
      })
      .catch((e) => {
        setAgentTrafficBinding(!v);
        toast.error(`保存に失敗しました：${(e as Error).message}`);
      })
      .finally(() => setSaving(false));
  };

  const toggleInjectWorker = (v: boolean) => {
    setInjectWorker(v); // optimistic
    api
      .setSettings({ constraints_inject_worker: v })
      .then(apply)
      .catch(() => setInjectWorker(!v)); // revert on failure
  };

  const toggleNoaCompaction = (v: boolean) => {
    setNoaCompaction(v); // optimistic
    api
      .setSettings({ noa_compaction: v })
      .then((s) => {
        apply(s);
        toast.success(
          v
            ? "noa コンテキスト圧縮を有効にしました（以後に開始する実行から有効）"
            : "noa コンテキスト圧縮を無効にしました（内蔵圧縮に戻ります）",
        );
      })
      .catch((e) => {
        setNoaCompaction(!v); // revert on failure
        toast.error(`保存に失敗しました：${(e as Error).message}`);
      });
  };

  // Web 検索のパッチ（有効化やバックエンド）を永続化する。楽観更新し、再取得も行う。
  const saveWebSearch = (patch: Partial<Settings>) => {
    setSaving(true);
    api
      .setSettings(patch)
      .then((s) => {
        apply(s);
        toast.success("Web 検索の設定を保存しました");
      })
      .catch((e) => {
        toast.error("保存に失敗しました：" + (e as Error).message);
        api
          .settings()
          .then(apply)
          .catch(() => undefined);
      })
      .finally(() => setSaving(false));
  };

  const saveBraveKey = () => {
    setSavingKey(true);
    api
      .setSettings({ brave_search_api_key: braveKeyInput })
      .then((s) => {
        apply(s);
        setBraveKeyInput("");
        toast.success("Brave API Key を保存しました");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingKey(false));
  };

  const saveTavilyKey = () => {
    setSavingTavilyKey(true);
    api
      .setSettings({ tavily_search_api_key: tavilyKeyInput })
      .then((s) => {
        apply(s);
        setTavilyKeyInput("");
        toast.success("Tavily API Key を保存しました");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingTavilyKey(false));
  };

  const saveProxy = () => {
    setSavingProxy(true);
    api
      .setSettings({ web_search_proxy: proxyInput.trim() })
      .then((s) => {
        apply(s);
        toast.success(
          proxyInput.trim() ? "外向きプロキシを保存しました" : "外向きプロキシを削除しました（直接接続に変更）",
        );
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingProxy(false));
  };

  const saveGlobalProxy = () => {
    setSavingGlobalProxy(true);
    api
      .setSettings({ global_proxy: globalProxyInput.trim() })
      .then((s) => {
        apply(s);
        toast.success(
          globalProxyInput.trim()
            ? "グローバルプロキシを保存しました"
            : "グローバルプロキシを削除しました（直接接続に変更）",
        );
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingGlobalProxy(false));
  };

  // 実際のテスト検索（"test"）を現在のフォーム値（バックエンド + プロキシ + 入力中の
  // Key）で実行する。保存済みの値はサーバー側でフォールバックされる。結果は toast 表示。
  const runTest = () => {
    setTesting(true);
    api
      .testWebSearch({
        web_search_backend: backend,
        web_search_proxy: proxyInput.trim(),
        brave_search_api_key: braveKeyInput,
        tavily_search_api_key: tavilyKeyInput,
      })
      .then((r) => {
        if (r.ok) toast.success(`検索テスト成功 · ${r.backend} が ${r.count} 件の結果を返しました`);
        else toast.error("検索テストに失敗しました：" + (r.error || "不明なエラー"));
      })
      .catch((e) => toast.error("検索テストに失敗しました：" + (e as Error).message))
      .finally(() => setTesting(false));
  };

  // brave-free が選択されているが Key が未保存・未入力 → ツールは無効のまま。
  const braveNeedsKey = webSearch && backend === "brave-free" && !braveKeySet;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div>
        <h1 className="text-xl font-semibold tracking-tight">システム設定</h1>
        <p className="text-muted-foreground text-sm">グローバルな実行時スイッチ</p>
      </div>

      {/* grid ではなくマルチカラム：Web 検索カードは他より何倍も高く、しかも選択した
          バックエンドで高さが変わる（brave/tavily の key 入力は条件レンダリング）。
          grid だと最も高いカードで行全体が埋まり、横に大きな空白が残る。マルチカラム
          なら内容の高さで自動的にバランスして埋まる。カードの間隔は gap ではなく mb で
          取る——マルチカラムでは column-gap は列間隔にしか効かず、行間隔は子要素が
          自分で指定する必要がある。 */}
      <div className="columns-1 gap-4 md:gap-6 lg:columns-2">
        <UpdateCard />

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              トラフィックキャプチャ
            </CardTitle>
            <CardDescription>
              有効にすると、すべての Agent の HTTP トラフィックが記録プロキシ経由で全量 DB に保存され、 Agent に
              traffic_search / traffic_get ツールとプロキシ設定が注入されます（プロンプトに プロキシの説明を含む）。
              <br />
              無効（既定）時はトラフィックを一切記録しません：Agent は プロキシ設定とトラフィックツールを
              <b>受け取らず</b>、プロンプトにもプロキシ関連の記述が
              <b>含まれません</b>。切り替えると即座に Agent が再構築されて反映されます。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="traffic-capture" className="text-sm font-normal text-muted-foreground">
              {trafficCapture ? "有効 · トラフィックを記録しプロキシを注入中" : "無効 · 記録もプロキシ注入もなし"}
            </Label>
            <Switch
              id="traffic-capture"
              checked={trafficCapture}
              disabled={!loaded || saving}
              onCheckedChange={toggleTraffic}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              Agent の自動トラフィック紐付け
            </CardTitle>
            <CardDescription id="agent-traffic-binding-description">
              既定はオフ。有効にすると、脆弱性の登録時に起動するレポート Agent が既存の HTTP
              リクエスト/レスポンスを照合し、対応するトラフィックを紐付けてからレポートを作成します。
              <b>パケットの確認や追加のツール呼び出しにより Token 消費が増えます。</b>
              <br />
              TCP や未キャプチャ、一致するトラフィックがない場合でも通常どおり報告できます。このスイッチは
              トラフィックキャプチャ、手動紐付け、保存済み証拠の閲覧には影響しません。次の Agent ラウンドから
              有効になります。オフにすると新しい自動紐付けは即座に拒否されます。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="agent-traffic-binding" className="text-sm font-normal text-muted-foreground">
              {agentTrafficBinding ? "有効 · Token 消費が増えます" : "無効 · 手動紐付けは引き続き可能"}
            </Label>
            <Switch
              id="agent-traffic-binding"
              aria-describedby="agent-traffic-binding-description"
              checked={agentTrafficBinding}
              disabled={!loaded || saving}
              onCheckedChange={toggleAgentTrafficBinding}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              グローバルプロキシ
            </CardTitle>
            <CardDescription>
              すべての Agent の<b>ターゲットトラフィック</b>がこのプロキシ経由で外部に出ます（送信元 IP の 隠蔽 /
              踏み台経由）。<b>http / https / socks5</b> に対応し、 <code>user:pass</code> 認証も可能。空欄＝直接接続。
              <br />
              <b>トラフィックキャプチャ</b>が有効な場合、記録プロキシの<b>上流</b>として機能します （トラフィックは全量
              DB に保存されたうえで、このプロキシ経由で外部に出る）。キャプチャが無効な場合は、 Agent の bash / WebFetch
              に直接注入されて外部に出ます。Web 検索プロキシ、LLM プロキシとは独立しています。
              <br />
              <b>ヒント</b>：socks5 は<b>キャプチャ無効</b>時に各コマンドラインツールの <code>ALL_PROXY</code>
              対応に依存します（curl は利用可、一部ツールは無視する可能性あり）。主に socks5 を使う場合は
              トラフィックキャプチャの有効化を推奨します——この経路は MITM 自身が接続するため、
              ツール側は意識せず安定して動作します。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-2">
            <Label htmlFor="global-proxy" className="text-sm font-normal text-muted-foreground">
              プロキシ URL
            </Label>
            <div className="flex items-center gap-2">
              <Input
                id="global-proxy"
                autoComplete="off"
                placeholder="socks5://user:pass@host:1080 または http://host:port（空欄=直接接続）"
                value={globalProxyInput}
                disabled={!loaded || savingGlobalProxy}
                onChange={(e) => setGlobalProxyInput(e.target.value)}
              />
              <Button type="button" onClick={saveGlobalProxy} disabled={!loaded || savingGlobalProxy}>
                保存
              </Button>
            </div>
            <p className="text-muted-foreground text-xs">
              {globalProxyInput.trim()
                ? "設定済み · すべてのターゲットトラフィックがこのプロキシ経由で外部に出ます"
                : "未設定 · ターゲットトラフィックは直接外部に出ます"}
            </p>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <ShieldAlertIcon className="size-4" />
              操作制約の注入
            </CardTitle>
            <CardDescription>
              有効にすると、各タスクの<b>操作制約</b>（タスク概要の「操作制約」で管理する allow/deny
              エントリ）を対応する Agent のシステムプロンプトに組み込み、探索の境界を定めます
              （例：「現在のポートのみテスト」「ブルートフォース禁止」）。
              <br />
              <b>プランナー（planner）</b>と <b>ワーカー（worker）</b>に注入するかを個別に制御できます。
              既定は両方オン。切り替えは即時反映（次のラウンドで読み込み）され、Agent の再構築は不要です。
              オフにするとその Agent は制約を参照しなくなります。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="inject-planner" className="text-sm font-normal text-muted-foreground">
                プランナー（planner）へ注入{injectPlanner ? " · 有効" : " · 無効"}
              </Label>
              <Switch
                id="inject-planner"
                checked={injectPlanner}
                disabled={!loaded}
                onCheckedChange={toggleInjectPlanner}
              />
            </div>
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="inject-worker" className="text-sm font-normal text-muted-foreground">
                ワーカー（worker）へ注入{injectWorker ? " · 有効" : " · 無効"}
              </Label>
              <Switch
                id="inject-worker"
                checked={injectWorker}
                disabled={!loaded}
                onCheckedChange={toggleInjectWorker}
              />
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <FlaskConicalIcon className="size-4" />
              実験機能
            </CardTitle>
            <CardDescription>
              検証中の仕組みで、既定はオフ。Agent の挙動を変えたり安定性に影響する可能性があるため、
              影響を理解したうえで有効にしてください。
              <br />
              <b>noa コンテキスト圧縮</b>：モデルが長い会話履歴を主体的に圧縮します（norma v0.4.0）。
              有効にすると、プラットフォームが接続する 4 種類の Agent（
              <b>プランナー / ワーカー / メイン Agent / チャット</b>）は内蔵圧縮に代えて noa
              がコンテキストを管理し、圧縮前の原文はタスクの
              作業ディレクトリにアーカイブされて遡及できます。切り替えは即時反映（以後に開始する実行から有効）
              され、Agent の再構築は不要です。オフにすると直ちに内蔵圧縮へ戻ります。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="noa-compaction" className="text-sm font-normal text-muted-foreground">
              noa コンテキスト圧縮{noaCompaction ? " · 有効" : " · 無効"}
            </Label>
            <Switch
              id="noa-compaction"
              checked={noaCompaction}
              disabled={!loaded}
              onCheckedChange={toggleNoaCompaction}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <SearchIcon className="size-4" />
              Web 検索
            </CardTitle>
            <CardDescription>
              これは Web 検索の<b>全体スイッチ + ソース設定</b>です。有効にして初めて、<b>各 Agent の設定</b>で
              <b>web_search</b> を個別に有効化できます（返すのはタイトル/リンク/要約のみで、本文取得は行いません。
              本文取得は WebFetch が担当）。Web 検索は記録プロキシを<b>通らず</b>、トラフィックキャプチャとは
              独立しています。
              <br />
              ソースは <b>DuckDuckGo（ddgs）</b>（Key 不要）、<b>Brave（無料版）</b>（Brave API Key の入力が必要）、{" "}
              <b>Tavily</b>（Tavily API Key の入力が必要）、<b>DeepSeek</b>（現在の LLM 設定を再利用）から選択
              できます。全体スイッチがオフのとき、各 Agent の Web 検索スイッチは使用できません。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="web-search" className="text-sm font-normal text-muted-foreground">
                {webSearch
                  ? "全体スイッチ有効 · 各 Agent の設定で個別に有効化できます"
                  : "無効 · 各 Agent で Web 検索を有効化できません"}
              </Label>
              <Switch
                id="web-search"
                checked={webSearch}
                disabled={!loaded || saving}
                onCheckedChange={(v) => {
                  setWebSearch(v); // optimistic
                  saveWebSearch({ web_search_enabled: v });
                }}
              />
            </div>

            {webSearch && (
              <div className="flex items-center justify-between gap-4">
                <Label className="text-sm font-normal text-muted-foreground">検索ソース</Label>
                <Select
                  value={backend}
                  disabled={!loaded || saving}
                  onValueChange={(v) => {
                    setBackend(v); // optimistic
                    saveWebSearch({ web_search_backend: v });
                  }}
                >
                  <SelectTrigger className="w-48 shrink-0">
                    <SelectValue placeholder="ソースを選択" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="ddgs">DuckDuckGo（ddgs · 無料・Key 不要）</SelectItem>
                    <SelectItem value="brave-free">Brave（無料版 · Key 必須）</SelectItem>
                    <SelectItem value="tavily">Tavily（Key 必須）</SelectItem>
                    <SelectItem value="deepseek">DeepSeek（公式）</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            )}

            {webSearch && backend === "deepseek" && (
              <div className="border-border/60 bg-muted/30 flex flex-col gap-2 rounded-md border p-3">
                <p className="text-sm font-medium">DeepSeek 公式の Web 検索</p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  このソースは<b>現在アクティブな LLM 設定</b>をそのまま再利用します。そのため
                  <b>DeepSeek 公式モデルのみ対応</b>し、かつその設定は
                  <b>anthropic プロトコルを使用している必要があります</b>——DeepSeek の OpenAI
                  プロトコルエンドポイントはサーバーサイド検索に 対応していません。LLM
                  設定を切り替えるとこのソースが使えなくなる場合があります。
                </p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  他のソースと異なり、検索は <b>DeepSeek のサーバー側で実行</b>されます：検索ごとに モデル呼び出しが 1
                  回余分に発生し（Token 費用がかかる）、検索リクエストは<b>上記の外向き プロキシを通らず</b>、
                  <b>トラフィック履歴にも記録されません</b>。返される結果は<b>タイトルと リンクのみ</b>
                  （要約なし）で、本文が必要な場合は WebFetch が取得します。
                </p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  上記の条件を満たすかどうかはご自身で確認してください。システム側では制限しません。
                  下の「検索テスト」ボタンで実際に 1 回実行して検証できます。
                </p>
              </div>
            )}

            {webSearch && backend === "brave-free" && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="brave-key" className="text-sm font-normal text-muted-foreground">
                  Brave Search API Key
                  {braveKeySet && <span className="ml-2 text-xs text-emerald-500">設定済み</span>}
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="brave-key"
                    type="password"
                    autoComplete="off"
                    placeholder={braveKeySet ? "設定済み（空欄なら変更なし）" : "Brave API Key を入力"}
                    value={braveKeyInput}
                    disabled={!loaded || savingKey}
                    onChange={(e) => setBraveKeyInput(e.target.value)}
                  />
                  <Button
                    type="button"
                    onClick={saveBraveKey}
                    disabled={!loaded || savingKey || braveKeyInput.trim() === ""}
                  >
                    保存
                  </Button>
                </div>
                {braveNeedsKey && (
                  <p className="text-xs text-amber-500">
                    Brave を選択しましたが Key が未設定です —— Key を保存するまで検索ツールは有効になりません。
                  </p>
                )}
                <p className="text-muted-foreground text-xs">
                  無料版の枠は月約 2,000 回。https://brave.com/search/api/ で Key を取得してください。
                </p>
              </div>
            )}

            {webSearch && backend === "tavily" && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="tavily-key" className="text-sm font-normal text-muted-foreground">
                  Tavily Search API Key
                  {tavilyKeySet && <span className="ml-2 text-xs text-emerald-500">設定済み</span>}
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="tavily-key"
                    type="password"
                    autoComplete="off"
                    placeholder={tavilyKeySet ? "設定済み（空欄なら変更なし）" : "Tavily API Key を入力（tvly-…）"}
                    value={tavilyKeyInput}
                    disabled={!loaded || savingTavilyKey}
                    onChange={(e) => setTavilyKeyInput(e.target.value)}
                  />
                  <Button
                    type="button"
                    onClick={saveTavilyKey}
                    disabled={!loaded || savingTavilyKey || tavilyKeyInput.trim() === ""}
                  >
                    保存
                  </Button>
                </div>
                {webSearch && backend === "tavily" && !tavilyKeySet && (
                  <p className="text-xs text-amber-500">
                    Tavily を選択しましたが Key が未設定です —— Key を保存するまで検索ツールは有効になりません。
                  </p>
                )}
                <p className="text-muted-foreground text-xs">
                  https://tavily.com で登録して API Key を取得してください。
                </p>
              </div>
            )}

            {webSearch && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="ws-proxy" className="text-sm font-normal text-muted-foreground">
                  外向きプロキシ（任意）
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="ws-proxy"
                    autoComplete="off"
                    placeholder="http://host:port または socks5://host:port（空欄=直接接続）"
                    value={proxyInput}
                    disabled={!loaded || savingProxy}
                    onChange={(e) => setProxyInput(e.target.value)}
                  />
                  <Button type="button" onClick={saveProxy} disabled={!loaded || savingProxy}>
                    保存
                  </Button>
                </div>
                <p className="text-muted-foreground text-xs">
                  独立した外向きプロキシで、検索エンドポイントへのアクセスにのみ使用します（VPN/SOCKS
                  など）。トラフィック記録用の MITM
                  プロキシとは無関係です。外部ネットワークに直接到達できない場合はこのプロキシ経由でアクセスします。
                </p>
              </div>
            )}

            {webSearch && (
              <div className="flex items-center justify-between gap-4 border-t pt-4">
                <p className="text-muted-foreground text-xs">
                  現在の設定（ソース + プロキシ + Key）で「test」を実際に検索し、利用可能か検証します。
                </p>
                <Button
                  type="button"
                  variant="outline"
                  onClick={runTest}
                  disabled={!loaded || testing}
                  className="shrink-0"
                >
                  {testing ? "テスト中…" : "検索をテスト"}
                </Button>
              </div>
            )}
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              カスタムスクリプト · Python インタプリタ
            </CardTitle>
            <CardDescription>
              カスタムの <b>script</b> 種別ツールが Python を実行する際に使用します。起動時に自動検出され （python3
              を優先）、ここでは venv や特定バージョンの絶対パスを手動入力できます。空欄なら 実行時に自動検出します。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Input
                className="font-mono text-sm"
                placeholder="/usr/bin/python3（空欄=自動検出）"
                value={pyInterp}
                disabled={!loaded || saving}
                onChange={(e) => setPyInterp(e.target.value)}
              />
              <Button variant="outline" onClick={detectPython} disabled={!loaded || saving}>
                再検出
              </Button>
              <Button onClick={savePython} disabled={!loaded || saving}>
                保存
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <CpuIcon className="size-4" />
              作業並列数 · Work Agent 数
            </CardTitle>
            <CardDescription>
              タスクごとに並列実行するワーカー agent の数（既定 3）。値が大きいほど並列探索が増え、
              消費も増えます。変更は<b>以後に開始するタスクから有効</b>で、実行中のタスクには影響しません。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Input
                type="number"
                min={1}
                className="w-32 font-mono text-sm"
                placeholder="3"
                value={workers}
                disabled={!loaded || savingWorkers}
                onChange={(e) => setWorkers(e.target.value)}
              />
              <Button onClick={saveWorkers} disabled={!loaded || savingWorkers}>
                保存
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <KeyboardIcon className="size-4" />
              セッション入力欄の送信キー
            </CardTitle>
            <CardDescription>
              チャットページとタスク詳細のメイン Agent セッション入力欄がこの設定を共有します。選択後は
              即時反映され、保存は不要です。
              <br />
              この設定は<b>このブラウザにのみ保存</b>され、アカウント間で同期されません。別のブラウザを使う
              場合やサイトデータを消去した場合は再設定が必要です。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="chat-send-mode" className="text-sm font-normal text-muted-foreground">
              送信方式
            </Label>
            <Select value={sendMode} onValueChange={(v) => setChatSendMode(v as ChatSendMode)}>
              <SelectTrigger id="chat-send-mode" className="w-72">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {CHAT_SEND_MODE_OPTIONS.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
