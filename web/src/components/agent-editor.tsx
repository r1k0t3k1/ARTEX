"use client";

import * as React from "react";
import { toast } from "sonner";
import {
  EyeIcon,
  GitCompareIcon,
  InfoIcon,
  PencilIcon,
  RotateCcwIcon,
  SaveIcon,
  Trash2Icon,
  XIcon,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Textarea } from "@/components/ui/textarea";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { Switch } from "@/components/ui/switch";
import { Separator } from "@/components/ui/separator";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { Markdown } from "@/components/markdown";
import { api } from "@/lib/api";
import { cn } from "@/lib/utils";
import type {
  Agent,
  AgentDetail,
  AgentTrigger,
  MCPServer,
  PromptVar,
  PromptVersion,
  Settings,
  SkillItem,
  Tool,
} from "@/lib/types";

// トラフィック系ツールはグローバルの「トラフィックキャプチャ」スイッチで制御される
// ホストツールで、紐付けは可能だがキャプチャが有効なときのみ使用できる。
// このリストは traffic.SeedToolMetas と同期させること。
const TRAFFIC_TOOL_KEYS = new Set(["traffic_search", "traffic_get"]);

// AgentEditor は 1 つの Agent を編集するタブ式エディタで、agents ページのドロワー内で
// 使われる（ディープリンク時はページ全体でも再利用される）。タブは「設定とプロンプト」/
// MCP / Skill / Tools。設定とプロンプトは従来どおり保存し、可視性とツール紐付けは即時反映する。
export function AgentEditor({ agentKey, onSaved }: { agentKey: string; onSaved?: () => void }) {
  const [detail, setDetail] = React.useState<AgentDetail | null>(null);
  const [versions, setVersions] = React.useState<PromptVersion[]>([]);
  const [variables, setVariables] = React.useState<PromptVar[]>([]);
  const [mcp, setMcp] = React.useState<MCPServer[]>([]);
  const [skills, setSkills] = React.useState<SkillItem[]>([]);
  const [tools, setTools] = React.useState<Tool[]>([]);
  const [loaded, setLoaded] = React.useState(false);
  const [viewVer, setViewVer] = React.useState<PromptVersion | null>(null);
  const [diffVer, setDiffVer] = React.useState<PromptVersion | null>(null);

  const [prompt, setPrompt] = React.useState("");
  const [mcpVisible, setMcpVisible] = React.useState<number[]>([]);
  const [skillVisible, setSkillVisible] = React.useState<string[]>([]);
  const [preview, setPreview] = React.useState("");
  const [maxTurns, setMaxTurns] = React.useState("0");
  const [runSecs, setRunSecs] = React.useState("600");
  // "" = フォロー（未紐付け）。それ以外はプロファイル id の文字列
  const [llmProfileId, setLlmProfileId] = React.useState("");
  const [llmProfiles, setLlmProfiles] = React.useState<NonNullable<AgentDetail["llm_profiles"]>>([]);
  const [webSearch, setWebSearch] = React.useState(false);
  const [interactiveShell, setInteractiveShell] = React.useState(false);
  const [wrapup, setWrapup] = React.useState("");
  const [wrapupDefault, setWrapupDefault] = React.useState("");
  const [wrapupTurns, setWrapupTurns] = React.useState("0");
  const [wrapupTurnsDefault, setWrapupTurnsDefault] = React.useState(5);
  // タスクレベルのタイムアウト収尾プロンプト（worker/planner のみ）
  const [ttSupported, setTtSupported] = React.useState(false);
  const [ttWrapup, setTtWrapup] = React.useState("");
  const [ttWrapupDefault, setTtWrapupDefault] = React.useState("");
  const [ttTurns, setTtTurns] = React.useState("0");
  const [ttTurnsDefault, setTtTurnsDefault] = React.useState(5);
  const [settings, setSettings] = React.useState<Settings | null>(null);

  React.useEffect(() => {
    api
      .mcpServers()
      .then(setMcp)
      .catch(() => {});
    api
      .skills()
      .then(setSkills)
      .catch(() => {});
    api
      .tools()
      .then(setTools)
      .catch(() => {});
    api
      .settings()
      .then(setSettings)
      .catch(() => {});
  }, []);
  // グローバルなゲート: トラフィック系ツールはトラフィックキャプチャ、Web 検索はマスタースイッチが必要。
  const captureOn = !!settings?.traffic_capture;
  const webSearchGlobalOn = !!settings?.web_search_enabled;

  const reload = React.useCallback(() => {
    api
      .getAgent(agentKey)
      .then((d) => {
        setDetail(d);
        setPrompt(d.prompt ?? "");
        setVariables(d.variables ?? []);
        setVersions(d.versions ?? []);
        setMcpVisible(d.visibility?.mcp ?? []);
        setSkillVisible(d.visibility?.skill ?? []);
        setMaxTurns(String(d.agent?.max_turns ?? 0));
        setRunSecs(String(d.agent?.run_seconds ?? 600));
        setLlmProfileId(d.agent?.llm_profile_id != null ? String(d.agent.llm_profile_id) : "");
        setLlmProfiles(d.llm_profiles ?? []);
        setWebSearch(!!d.agent?.web_search);
        setInteractiveShell(!!d.agent?.interactive_shell);
        setWrapup(d.wrapup_prompt ?? "");
        setWrapupDefault(d.wrapup_default ?? "");
        setWrapupTurns(String(d.wrapup_max_turns ?? 0));
        setWrapupTurnsDefault(d.wrapup_max_turns_default ?? 5);
        setTtSupported(!!d.task_timeout_wrapup_supported);
        setTtWrapup(d.task_timeout_wrapup_prompt ?? "");
        setTtWrapupDefault(d.task_timeout_wrapup_default ?? "");
        setTtTurns(String(d.task_timeout_wrapup_max_turns ?? 0));
        setTtTurnsDefault(d.task_timeout_wrapup_max_turns_default ?? 5);
      })
      .catch(() => setDetail(null))
      .finally(() => setLoaded(true));
  }, [agentKey]);
  React.useEffect(() => {
    reload();
  }, [reload]);

  async function doPreview() {
    try {
      const r = await api.previewAgentPrompt(agentKey, prompt);
      setPreview(r.error ? "レンダリングエラー：" + r.error : r.rendered);
    } catch (e) {
      setPreview("プレビューに失敗しました：" + (e as Error).message);
    }
  }
  async function savePrompt() {
    try {
      const r = await api.saveAgentPrompt(agentKey, prompt);
      toast.success(`バージョン v${r.version} として保存しました`);
      reload();
      onSaved?.();
    } catch (e) {
      toast.error("保存に失敗しました：" + (e as Error).message);
    }
  }
  async function resetPrompt() {
    try {
      const r = await api.resetAgentPrompt(agentKey);
      toast.success(`組み込みの既定値に戻しました（v${r.version}）`);
      reload();
    } catch (e) {
      toast.error("復元に失敗しました：" + (e as Error).message);
    }
  }
  async function saveWrapup() {
    try {
      const turns = Math.max(0, Math.floor(Number(wrapupTurns) || 0));
      await api.saveAgentWrapup(agentKey, wrapup, turns);
      toast.success(
        wrapup.trim() || turns > 0
          ? "収尾設定を保存しました（次回実行から有効）"
          : "クリアしました。組み込みの既定値を使用します",
      );
      reload();
    } catch (e) {
      toast.error("保存に失敗しました：" + (e as Error).message);
    }
  }
  async function resetWrapup() {
    try {
      await api.resetAgentWrapup(agentKey);
      toast.success("組み込みの既定値に戻しました");
      reload();
    } catch (e) {
      toast.error("復元に失敗しました：" + (e as Error).message);
    }
  }
  async function saveTaskTimeoutWrapup() {
    try {
      const turns = Math.max(0, Math.floor(Number(ttTurns) || 0));
      await api.saveAgentTaskTimeoutWrapup(agentKey, ttWrapup, turns);
      toast.success("タスクタイムアウト収尾設定を保存しました（次回実行から有効）");
      reload();
    } catch (e) {
      toast.error("保存に失敗しました：" + (e as Error).message);
    }
  }
  async function resetTaskTimeoutWrapup() {
    try {
      await api.resetAgentTaskTimeoutWrapup(agentKey);
      toast.success("組み込みの既定値に戻しました");
      reload();
    } catch (e) {
      toast.error("復元に失敗しました：" + (e as Error).message);
    }
  }
  async function saveConfig() {
    try {
      // この agent が実際に表示しているフィールドのみ送信し、非表示項目（例: goals の max_turns）を
      // 既定値で上書きしないようにする。
      const patch: Parameters<typeof api.saveAgentConfig>[1] = {
        llm_profile_id: llmProfileId === "" ? null : Number(llmProfileId),
      };
      if (showConfig) {
        patch.max_turns = Math.max(0, Math.floor(Number(maxTurns) || 0));
        patch.run_seconds = Math.max(0, Math.floor(Number(runSecs) || 0));
      }
      if (showWebSearch) patch.web_search = webSearch;
      if (showInteractiveShell) patch.interactive_shell = interactiveShell;
      await api.saveAgentConfig(agentKey, patch);
      toast.success("実行設定を保存しました（即時反映）");
      reload();
    } catch (e) {
      toast.error("保存に失敗しました：" + (e as Error).message);
    }
  }
  // applyVis は楽観的に更新して永続化し、成功/失敗を toast で通知する。失敗時は
  // 以前の選択に戻し、UI が実際の状態と異なる表示にならないようにする。
  async function applyVis(nextMcp: number[], nextSkill: string[], okMsg: string) {
    const prevMcp = mcpVisible;
    const prevSkill = skillVisible;
    setMcpVisible(nextMcp);
    setSkillVisible(nextSkill);
    try {
      await api.setAgentVisibility(agentKey, nextMcp, nextSkill);
      toast.success(okMsg);
      onSaved?.(); // refresh the list so the card's MCP/Skill counts stay in sync
    } catch (e) {
      setMcpVisible(prevMcp);
      setSkillVisible(prevSkill);
      toast.error("保存に失敗しました：" + (e as Error).message);
    }
  }
  function toggleMcp(id: number) {
    const on = mcpVisible.includes(id);
    const name = mcp.find((m) => m.id === id)?.name ?? String(id);
    applyVis(
      on ? mcpVisible.filter((x) => x !== id) : [...mcpVisible, id],
      skillVisible,
      `${on ? "無効化" : "有効化"}しました: MCP「${name}」を可視`,
    );
  }
  function toggleSkill(name: string) {
    const on = skillVisible.includes(name);
    applyVis(
      mcpVisible,
      on ? skillVisible.filter((x) => x !== name) : [...skillVisible, name],
      `${on ? "無効化" : "有効化"}しました: Skill「${name}」を可視`,
    );
  }
  async function toggleTool(t: Tool) {
    const on = t.agents.includes(agentKey);
    const nextAgents = on ? t.agents.filter((k) => k !== agentKey) : [...t.agents, agentKey];
    // optimistic update
    setTools((ts) => ts.map((x) => (x.key === t.key ? { ...x, agents: nextAgents } : x)));
    try {
      await api.saveTool(t.key, {
        description: t.description,
        schema: t.schema,
        agents: nextAgents,
        enabled: t.enabled,
      });
      toast.success(`ツール「${t.key}」を${on ? "紐付け解除" : "紐付け"}しました`);
      onSaved?.(); // 一覧を再読み込みし、カードのツール件数を同期させる
    } catch (e) {
      toast.error("ツール紐付けの保存に失敗しました：" + (e as Error).message);
      reload();
      api
        .tools()
        .then(setTools)
        .catch(() => {});
    }
  }

  if (loaded && !detail) {
    return <div className="text-muted-foreground p-6 text-center text-sm">Agent が見つかりません：{agentKey}</div>;
  }
  // 設定は会話型の main agent と固定予算の goals 分解器には意味がない。
  // それ以外の agent（worker、カスタムアシスタント）はこれを尊重する。
  const showConfig = agentKey !== "mainagent" && agentKey !== "goals";
  // Web 検索はワンショットの goals 分解器を除くすべての会話/実行 agent に適用され、
  // グローバルのマスタースイッチで制御される。
  const showWebSearch = agentKey !== "goals";
  // 対話型シェル（永続 PTY セッションのツール群）も goals 以外の agent に開放。グローバルなゲートはない。
  const showInteractiveShell = agentKey !== "goals";
  // すべての agent（goals/mainagent を含む）は何らかの LLM 上で動作するため、
  // 「既定モデル」の紐付けはすべての agent に開放されている。
  const showLLM = true;
  // トリガー（P3）はカスタム agent にのみ紐付く。
  const isCustom = !!detail && !detail.agent?.builtin;

  return (
    <Tabs defaultValue="prompt" className="flex min-h-0 flex-1 flex-col">
      <TabsList className="mx-4 mt-2 w-fit">
        <TabsTrigger value="prompt">設定とプロンプト</TabsTrigger>
        <TabsTrigger value="wrapup">収尾プロンプト</TabsTrigger>
        <TabsTrigger value="mcp">MCP</TabsTrigger>
        <TabsTrigger value="skill">Skill</TabsTrigger>
        <TabsTrigger value="tools">Tools</TabsTrigger>
        {isCustom && <TabsTrigger value="triggers">トリガー</TabsTrigger>}
      </TabsList>

      {/* 設定 + プロンプト */}
      <TabsContent value="prompt" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <div className="grid gap-4">
          {(showLLM || showConfig || showWebSearch || showInteractiveShell) && (
            <div className="grid gap-3 rounded-md border p-3">
              {showLLM && (
                <div className="grid gap-1.5">
                  <Label htmlFor="llm-profile" className="text-xs">
                    既定モデル（LLM 設定）
                  </Label>
                  <div className="flex flex-wrap items-center gap-3">
                    <Select
                      value={llmProfileId || "__follow__"}
                      onValueChange={(v) => setLlmProfileId(v === "__follow__" ? "" : v)}
                    >
                      <SelectTrigger id="llm-profile" className="h-8 w-72">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="__follow__">タスク / グローバルのアクティブ設定に従う</SelectItem>
                        {llmProfiles.map((p) => (
                          <SelectItem key={p.id} value={String(p.id)}>
                            {p.name}（{p.model}）{p.is_default ? " · 既定" : ""}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    <span className="text-muted-foreground max-w-md text-xs">
                      この Agent に固定の LLM 設定を紐付ける（「設定を保存」で有効）。優先順位：Agent の紐付け &gt;
                      タスク/セッション指定 &gt; グローバルのアクティブ設定。
                    </span>
                  </div>
                </div>
              )}
              <div className="flex flex-wrap items-end gap-3">
                {showConfig && (
                  <>
                    <div className="grid gap-1.5">
                      <Label htmlFor="max-turns" className="text-xs">
                        最大ループ回数（0=無制限）
                      </Label>
                      <Input
                        id="max-turns"
                        type="number"
                        min={0}
                        className="h-8 w-32"
                        value={maxTurns}
                        onChange={(e) => setMaxTurns(e.target.value)}
                      />
                    </div>
                    <div className="grid gap-1.5">
                      <Label htmlFor="run-seconds" className="text-xs">
                        実行時間（秒、0=無制限）
                      </Label>
                      <Input
                        id="run-seconds"
                        type="number"
                        min={0}
                        className="h-8 w-32"
                        value={runSecs}
                        onChange={(e) => setRunSecs(e.target.value)}
                      />
                    </div>
                  </>
                )}
                <Button size="sm" variant="outline" onClick={saveConfig}>
                  <SaveIcon /> 設定を保存
                </Button>
              </div>
              {showWebSearch && (
                <div className="flex items-center gap-3 border-t pt-3">
                  <Switch
                    id="web-search"
                    checked={webSearch}
                    disabled={!webSearchGlobalOn}
                    onCheckedChange={setWebSearch}
                  />
                  <div className="grid gap-0.5">
                    <Label htmlFor="web-search" className="text-sm">
                      Web 検索
                    </Label>
                    <span className="text-muted-foreground text-xs">
                      {webSearchGlobalOn
                        ? "この Agent で有効にすると（上の保存で反映）、web_search による Web 検索が使える"
                        : "ここで有効にするには、先に「システム設定」で Web 検索を有効にしてバックエンドを設定する必要がある"}
                    </span>
                  </div>
                </div>
              )}
              {showInteractiveShell && (
                <div className="flex items-center gap-3 border-t pt-3">
                  <Switch id="interactive-shell" checked={interactiveShell} onCheckedChange={setInteractiveShell} />
                  <div className="grid gap-0.5">
                    <Label htmlFor="interactive-shell" className="text-sm">
                      対話型シェル
                    </Label>
                    <span className="text-muted-foreground text-xs">
                      この Agent で有効にすると（上の保存で反映）、永続 PTY
                      セッションのツール（shell_open/send/read/close/list）で msfconsole/ssh/REPL
                      などの対話型プログラムを操作できる
                    </span>
                  </div>
                </div>
              )}
            </div>
          )}

          <div className="grid gap-2">
            <Label className="text-muted-foreground text-xs">
              変数（クリックでプレースホルダを挿入。レンダリング時に実行時データで置換される）
            </Label>
            <div className="flex flex-wrap gap-2">
              {variables.map((v) => (
                <Tooltip key={v.name}>
                  <TooltipTrigger asChild>
                    <button
                      type="button"
                      onClick={() => setPrompt((p) => `${p}{{.${v.name}}}`)}
                      className="hover:bg-muted inline-flex items-center gap-1 rounded-md border bg-muted/40 px-2 py-1 font-mono text-xs"
                    >
                      {`{{.${v.name}}}`}
                      <Badge variant="secondary" className="px-1 py-0 text-[10px]">
                        {v.source}
                      </Badge>
                    </button>
                  </TooltipTrigger>
                  <TooltipContent className="max-w-xs">
                    <p className="font-medium">{v.description}</p>
                    <p className="text-muted-foreground mt-1">例：{v.example}</p>
                  </TooltipContent>
                </Tooltip>
              ))}
              {variables.length === 0 && <span className="text-muted-foreground text-xs">（変数なし）</span>}
            </div>
          </div>

          <Textarea
            className="font-mono text-xs"
            rows={16}
            value={prompt}
            placeholder="空欄の場合は組み込みの既定プロンプトを使用"
            onChange={(e) => setPrompt(e.target.value)}
          />

          <div className="flex flex-wrap gap-2">
            <Dialog>
              <DialogTrigger asChild>
                <Button variant="outline" size="sm" onClick={doPreview}>
                  <EyeIcon /> レンダリングをプレビュー
                </Button>
              </DialogTrigger>
              <DialogContent className="sm:max-w-2xl">
                <DialogHeader>
                  <DialogTitle>レンダリングプレビュー</DialogTitle>
                  <DialogDescription>すべての {`{{.Var}}`} はバックエンドでサンプル値に置換済み。</DialogDescription>
                </DialogHeader>
                <div className="max-h-[60vh] overflow-auto rounded-md border bg-muted/30 p-3">
                  <Markdown text={preview} />
                </div>
              </DialogContent>
            </Dialog>
            <Button size="sm" onClick={savePrompt}>
              <SaveIcon /> 新しいバージョンとして保存
            </Button>
            <Button variant="outline" size="sm" onClick={resetPrompt}>
              <RotateCcwIcon /> 既定に戻す
            </Button>
          </div>

          <Separator />
          <div className="grid gap-2">
            <Label className="text-muted-foreground text-xs">バージョン履歴</Label>
            <ul className="grid gap-1">
              {versions.map((ver, i) => (
                <li
                  key={ver.version}
                  className="flex items-center gap-2 rounded-md px-1 py-0.5 text-xs hover:bg-muted/50"
                >
                  <span className="font-mono shrink-0">v{ver.version}</span>
                  {i === 0 && (
                    <Badge variant="secondary" className="px-1.5 py-0 shrink-0">
                      現在
                    </Badge>
                  )}
                  <span className="text-muted-foreground truncate flex-1">{ver.note}</span>
                  {ver.ts && (
                    <span className="text-muted-foreground/60 shrink-0 tabular-nums">
                      {new Date(ver.ts).toLocaleDateString("ja-JP", {
                        month: "2-digit",
                        day: "2-digit",
                        hour: "2-digit",
                        minute: "2-digit",
                      })}
                    </span>
                  )}
                  <Button variant="ghost" size="icon-sm" className="size-6 shrink-0" onClick={() => setViewVer(ver)}>
                    <EyeIcon className="size-3" />
                  </Button>
                  {i > 0 && versions[0] && (
                    <Button variant="ghost" size="icon-sm" className="size-6 shrink-0" onClick={() => setDiffVer(ver)}>
                      <GitCompareIcon className="size-3" />
                    </Button>
                  )}
                </li>
              ))}
              {versions.length === 0 && (
                <li className="text-muted-foreground text-xs">（保存済みのバージョンなし。組み込みの既定を使用）</li>
              )}
            </ul>
          </div>

          {/* バージョン表示ダイアログ */}
          <Dialog
            open={!!viewVer}
            onOpenChange={(o) => {
              if (!o) setViewVer(null);
            }}
          >
            <DialogContent className="sm:max-w-2xl">
              <DialogHeader>
                <DialogTitle>
                  v{viewVer?.version}
                  {viewVer?.version === versions[0]?.version && (
                    <Badge variant="secondary" className="ml-2 px-1.5 py-0 align-middle">
                      現在
                    </Badge>
                  )}
                </DialogTitle>
                <DialogDescription>
                  {viewVer?.note || "（メモなし）"}
                  {viewVer?.ts && (
                    <span className="ml-2 text-muted-foreground/60">
                      {new Date(viewVer.ts).toLocaleString("ja-JP")}
                    </span>
                  )}
                </DialogDescription>
              </DialogHeader>
              <pre className="bg-muted max-h-[55vh] overflow-auto whitespace-pre-wrap rounded-md p-3 font-mono text-xs">
                {viewVer?.template_text || "（空）"}
              </pre>
              <div className="flex gap-2 justify-end">
                {viewVer && viewVer.version !== versions[0]?.version && (
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => {
                      if (viewVer) {
                        setDiffVer(viewVer);
                        setViewVer(null);
                      }
                    }}
                  >
                    <GitCompareIcon className="mr-1 size-3.5" /> 現在のバージョンと比較
                  </Button>
                )}
                <Button
                  size="sm"
                  onClick={() => {
                    if (viewVer) {
                      setPrompt(viewVer.template_text);
                      setViewVer(null);
                      toast.success(
                        `v${viewVer.version} をエディタに読み込みました。確認後「新しいバージョンとして保存」をクリックしてください`,
                      );
                    }
                  }}
                >
                  エディタに読み込む
                </Button>
              </div>
            </DialogContent>
          </Dialog>

          {/* バージョン比較ダイアログ */}
          <Dialog
            open={!!diffVer}
            onOpenChange={(o) => {
              if (!o) setDiffVer(null);
            }}
          >
            <DialogContent className="sm:max-w-3xl">
              <DialogHeader>
                <DialogTitle>
                  バージョン比較：v{diffVer?.version} → v{versions[0]?.version}（現在）
                </DialogTitle>
                <DialogDescription>
                  <span className="inline-flex items-center gap-3 text-xs">
                    <span className="rounded bg-red-500/15 px-1.5 py-0.5 text-red-600 dark:text-red-400">- 削除</span>
                    <span className="rounded bg-green-500/15 px-1.5 py-0.5 text-green-600 dark:text-green-400">
                      + 追加
                    </span>
                  </span>
                </DialogDescription>
              </DialogHeader>
              <DiffView oldText={diffVer?.template_text ?? ""} newText={versions[0]?.template_text ?? ""} />
            </DialogContent>
          </Dialog>
        </div>
      </TabsContent>

      {/* 収尾プロンプト */}
      <TabsContent value="wrapup" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <div className="grid gap-3">
          <p className="text-muted-foreground text-xs leading-relaxed">
            この Agent が<b>タイムアウト</b>または<b>ステップ枯渇</b>で終了した際、システムはこの「収尾プロンプト」を
            注入して 1 ラウンドの収尾を実行する。まず認識済みだが未保存の内容を書き戻し、次に要約を 1 文出力する
            （中途半端な終わりを防ぐ）。空欄の場合は組み込みの既定値を使用する。
          </p>
          <div className="flex items-center gap-2">
            <Label className="text-xs">収尾プロンプト本文</Label>
            {wrapup.trim() ? (
              <Badge variant="secondary" className="px-1.5 py-0">
                カスタム
              </Badge>
            ) : (
              <Badge variant="outline" className="px-1.5 py-0">
                組み込みの既定を使用
              </Badge>
            )}
          </div>
          <Textarea
            className="font-mono text-xs"
            rows={10}
            value={wrapup}
            placeholder={wrapupDefault || "空欄の場合は組み込みの既定収尾プロンプトを使用"}
            onChange={(e) => setWrapup(e.target.value)}
          />
          <div className="grid gap-1.5">
            <Label htmlFor="wrapup-turns" className="text-xs">
              収尾ラウンド数（収尾フェーズ自体の最大実行ラウンド。0=組み込みの既定 {wrapupTurnsDefault} ラウンド）
            </Label>
            <Input
              id="wrapup-turns"
              type="number"
              min={0}
              className="h-8 w-32"
              value={wrapupTurns}
              onChange={(e) => setWrapupTurns(e.target.value)}
            />
          </div>
          <div className="flex gap-2">
            <Button size="sm" onClick={saveWrapup}>
              保存
            </Button>
            <Button size="sm" variant="outline" onClick={resetWrapup}>
              既定に戻す
            </Button>
          </div>
          {wrapupDefault && (
            <>
              <Separator />
              <div className="grid gap-1.5">
                <Label className="text-muted-foreground text-xs">組み込みの既定（読み取り専用、参考用）</Label>
                <pre className="text-muted-foreground max-h-40 overflow-y-auto rounded-md border bg-muted/30 p-2 text-xs whitespace-pre-wrap">
                  {wrapupDefault}
                </pre>
              </div>
            </>
          )}

          {ttSupported && (
            <>
              <Separator className="my-2" />
              <p className="text-muted-foreground text-xs leading-relaxed">
                <b>タスクタイムアウト収尾</b>（上記の per-run 収尾とは<b>別物</b>）：<b>タスク全体</b>
                がタイムアウト上限に 達して終了しようとする際に注入される。per-run
                とは意味が逆になることが多い（例：planner では per-run は
                「止まらず計画を続けろ」、タスクタイムアウト時は「時間になったので停止し最終判定を行え」）。空欄なら組み込みの既定を使用。
              </p>
              <div className="flex items-center gap-2">
                <Label className="text-xs">タスクタイムアウト収尾プロンプト本文</Label>
                {ttWrapup.trim() ? (
                  <Badge variant="secondary" className="px-1.5 py-0">
                    カスタム
                  </Badge>
                ) : (
                  <Badge variant="outline" className="px-1.5 py-0">
                    組み込みの既定を使用
                  </Badge>
                )}
              </div>
              <Textarea
                className="font-mono text-xs"
                rows={10}
                value={ttWrapup}
                placeholder={ttWrapupDefault || "空欄の場合は組み込みの既定タスクタイムアウト収尾プロンプトを使用"}
                onChange={(e) => setTtWrapup(e.target.value)}
              />
              <div className="grid gap-1.5">
                <Label htmlFor="tt-turns" className="text-xs">
                  収尾ラウンド数（0=組み込みの既定 {ttTurnsDefault} ラウンド）
                </Label>
                <Input
                  id="tt-turns"
                  type="number"
                  min={0}
                  className="h-8 w-32"
                  value={ttTurns}
                  onChange={(e) => setTtTurns(e.target.value)}
                />
              </div>
              <div className="flex gap-2">
                <Button size="sm" onClick={saveTaskTimeoutWrapup}>
                  保存
                </Button>
                <Button size="sm" variant="outline" onClick={resetTaskTimeoutWrapup}>
                  既定に戻す
                </Button>
              </div>
              {ttWrapupDefault && (
                <div className="grid gap-1.5">
                  <Label className="text-muted-foreground text-xs">組み込みの既定（読み取り専用、参考用）</Label>
                  <pre className="text-muted-foreground max-h-40 overflow-y-auto rounded-md border bg-muted/30 p-2 text-xs whitespace-pre-wrap">
                    {ttWrapupDefault}
                  </pre>
                </div>
              )}
            </>
          )}
        </div>
      </TabsContent>

      {/* MCP の可視性 */}
      <TabsContent value="mcp" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <p className="text-muted-foreground mb-3 text-xs">この Agent から参照可能にする MCP サーバーを選択する。</p>
        <div className="grid gap-2">
          {mcp.map((m) => (
            <label key={m.id} className="flex items-center gap-2 rounded-md border p-2 text-sm">
              <Checkbox checked={mcpVisible.includes(m.id)} onCheckedChange={() => toggleMcp(m.id)} />
              {m.name}
              <span className="text-muted-foreground ml-auto text-xs">{m.transport}</span>
            </label>
          ))}
          {mcp.length === 0 && <span className="text-muted-foreground text-xs">（MCP なし）</span>}
        </div>
      </TabsContent>

      {/* Skill の可視性 */}
      <TabsContent value="skill" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <p className="text-muted-foreground mb-3 text-xs">この Agent から参照可能にする Skill を選択する。</p>
        <div className="grid gap-2">
          {skills.map((s) => (
            <label key={s.name} className="flex items-center gap-2 rounded-md border p-2 text-sm">
              <Checkbox checked={skillVisible.includes(s.name)} onCheckedChange={() => toggleSkill(s.name)} />
              <span className="font-mono text-xs">{s.name}</span>
              {s.description && <span className="text-muted-foreground ml-auto truncate text-xs">{s.description}</span>}
            </label>
          ))}
          {skills.length === 0 && <span className="text-muted-foreground text-xs">（Skill なし）</span>}
        </div>
      </TabsContent>

      {/* Tools の紐付け */}
      <TabsContent value="tools" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        <p className="text-muted-foreground mb-3 text-xs">この Agent に紐付ける組み込みツールを選択する。</p>
        <div className="grid gap-2">
          {tools.map((t) => {
            const isTraffic = TRAFFIC_TOOL_KEYS.has(t.key);
            const gated = isTraffic && !captureOn; // トラフィック系ツールはトラフィックキャプチャが必要
            return (
              <label
                key={t.key}
                className={cn("flex items-center gap-2 rounded-md border p-2 text-sm", gated && "opacity-60")}
              >
                <Checkbox
                  checked={t.agents.includes(agentKey)}
                  disabled={gated}
                  onCheckedChange={() => toggleTool(t)}
                />
                <span className="font-mono text-xs">{t.key}</span>
                {isTraffic && (
                  <Badge variant="secondary" className="px-1 py-0 text-[9px]">
                    トラフィック
                  </Badge>
                )}
                {!t.enabled && (
                  <Badge variant="outline" className="text-destructive px-1 py-0 text-[9px]">
                    無効化済み
                  </Badge>
                )}
                {gated ? (
                  <span className="text-muted-foreground ml-auto text-xs">トラフィックキャプチャが必要</span>
                ) : (
                  t.description && (
                    <span className="text-muted-foreground ml-auto line-clamp-1 max-w-[55%] text-xs">
                      {t.description}
                    </span>
                  )
                )}
              </label>
            );
          })}
          {tools.length === 0 && <span className="text-muted-foreground text-xs">（ツールなし）</span>}
        </div>
      </TabsContent>

      {/* トリガー（P3、カスタム agent のみ） */}
      {isCustom && (
        <TabsContent value="triggers" className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
          <AgentTriggersTab agentKey={agentKey} agent={detail?.agent} />
        </TabsContent>
      )}
    </Tabs>
  );
}

// ---------- Diff helpers ----------

type DiffLine = { type: "same" | "add" | "del"; text: string };

function computeDiff(oldText: string, newText: string): DiffLine[] {
  const a = oldText.split("\n");
  const b = newText.split("\n");
  const m = a.length;
  const n = b.length;
  const dp: number[][] = Array.from({ length: m + 1 }, () => new Array(n + 1).fill(0));
  for (let i = 1; i <= m; i++)
    for (let j = 1; j <= n; j++)
      dp[i][j] = a[i - 1] === b[j - 1] ? dp[i - 1][j - 1] + 1 : Math.max(dp[i - 1][j], dp[i][j - 1]);
  const result: DiffLine[] = [];
  let i = m;
  let j = n;
  while (i > 0 || j > 0) {
    if (i > 0 && j > 0 && a[i - 1] === b[j - 1]) {
      result.unshift({ type: "same", text: a[i - 1] });
      i--;
      j--;
    } else if (j > 0 && (i === 0 || dp[i][j - 1] >= dp[i - 1][j])) {
      result.unshift({ type: "add", text: b[j - 1] });
      j--;
    } else {
      result.unshift({ type: "del", text: a[i - 1] });
      i--;
    }
  }
  return result;
}

function DiffView({ oldText, newText }: { oldText: string; newText: string }) {
  const lines = React.useMemo(() => computeDiff(oldText, newText), [oldText, newText]);
  return (
    <pre className="max-h-[60vh] overflow-auto rounded-md border bg-muted/30 p-2 font-mono text-xs leading-5">
      {lines.map((l, idx) => (
        <div
          key={idx}
          className={cn(
            "whitespace-pre-wrap px-1",
            l.type === "del" && "bg-red-500/15 text-red-700 dark:text-red-400",
            l.type === "add" && "bg-green-500/15 text-green-700 dark:text-green-400",
            l.type === "same" && "text-muted-foreground",
          )}
        >
          <span className="select-none mr-1 opacity-50">{l.type === "del" ? "-" : l.type === "add" ? "+" : " "}</span>
          {l.text}
        </div>
      ))}
    </pre>
  );
}

// AgentTriggersTab はカスタム agent の P3 トリガー（一覧 + 追加 + 削除）を管理する。
// 各トリガーは発火条件（定期 / finding 検出 / ゴール達成 / タスクタイムアウト / ツール呼び出し。
// 複数選択可）を持ち、発火すると基本ユーザーメッセージとバックエンドが付加する自動コンテキストと
// ともに新しい会話が並行して実行される。
function AgentTriggersTab({ agentKey, agent }: { agentKey: string; agent?: Agent }) {
  const [triggers, setTriggers] = React.useState<AgentTrigger[]>([]);
  const [tools, setTools] = React.useState<Tool[]>([]);
  // トリガー後の処理ポリシー（agent ごと）。初期値は agent detail から取得し、変更は即保存する。
  const [runMode, setRunMode] = React.useState<"serial" | "parallel">(agent?.trigger_run_mode ?? "serial");
  const [mergeMode, setMergeMode] = React.useState<"by_task" | "all" | "none">(agent?.trigger_merge_mode ?? "all");
  const [maxParallel, setMaxParallel] = React.useState(String(agent?.trigger_max_parallel ?? 5));
  React.useEffect(() => {
    setRunMode(agent?.trigger_run_mode ?? "serial");
    setMergeMode(agent?.trigger_merge_mode ?? "all");
    setMaxParallel(String(agent?.trigger_max_parallel ?? 5));
  }, [agent?.trigger_run_mode, agent?.trigger_merge_mode, agent?.trigger_max_parallel]);

  async function saveBehavior(patch: {
    trigger_run_mode?: "serial" | "parallel";
    trigger_merge_mode?: "by_task" | "all" | "none";
    trigger_max_parallel?: number;
  }) {
    try {
      await api.saveAgentConfig(agentKey, patch);
    } catch (e) {
      toast.error("ポリシーの保存に失敗しました：" + (e as Error).message);
    }
  }
  const [onInterval, setOnInterval] = React.useState(false);
  const [intervalSec, setIntervalSec] = React.useState("60");
  const [onFinding, setOnFinding] = React.useState(false);
  const [onGoalMet, setOnGoalMet] = React.useState(false);
  const [onTaskTimeout, setOnTaskTimeout] = React.useState(false);
  const [onToolCall, setOnToolCall] = React.useState(false);
  const [onTaskCreate, setOnTaskCreate] = React.useState(false);
  const [intervalMsg, setIntervalMsg] = React.useState("");
  const [findingMsg, setFindingMsg] = React.useState("");
  const [goalMsg, setGoalMsg] = React.useState("");
  const [taskTimeoutMsg, setTaskTimeoutMsg] = React.useState("");
  const [toolCallMsg, setToolCallMsg] = React.useState("");
  const [taskCreateMsg, setTaskCreateMsg] = React.useState("");
  const [toolNames, setToolNames] = React.useState<string[]>([]);
  const [saving, setSaving] = React.useState(false);
  // null = 新規作成モード。非 null = その id のトリガーを編集中。
  const [editingId, setEditingId] = React.useState<number | null>(null);

  const reload = React.useCallback(() => {
    api
      .agentTriggers(agentKey)
      .then(setTriggers)
      .catch(() => setTriggers([]));
  }, [agentKey]);
  React.useEffect(() => {
    reload();
  }, [reload]);
  React.useEffect(() => {
    api
      .tools()
      .then(setTools)
      .catch(() => setTools([]));
  }, []);

  function toggleTool(key: string) {
    setToolNames((prev) => (prev.includes(key) ? prev.filter((k) => k !== key) : [...prev, key]));
  }

  // resetForm はフォームをクリアし「新規作成」モードに戻す。
  function resetForm() {
    setEditingId(null);
    setOnInterval(false);
    setIntervalSec("60");
    setOnFinding(false);
    setOnGoalMet(false);
    setOnTaskTimeout(false);
    setOnToolCall(false);
    setOnTaskCreate(false);
    setIntervalMsg("");
    setFindingMsg("");
    setGoalMsg("");
    setTaskTimeoutMsg("");
    setToolCallMsg("");
    setTaskCreateMsg("");
    setToolNames([]);
  }

  // startEdit は既存のトリガーをフォームに読み込み、「編集」モードに入る。
  function startEdit(t: AgentTrigger) {
    setEditingId(t.id);
    setOnInterval(t.interval_sec > 0);
    setIntervalSec(t.interval_sec > 0 ? String(t.interval_sec) : "60");
    setOnFinding(t.on_finding);
    setOnGoalMet(t.on_goal_met);
    setOnTaskTimeout(t.on_task_timeout);
    setOnToolCall(t.on_tool_call);
    setOnTaskCreate(t.on_task_create);
    setIntervalMsg(t.interval_message);
    setFindingMsg(t.finding_message);
    setGoalMsg(t.goal_message);
    setTaskTimeoutMsg(t.task_timeout_message);
    setToolCallMsg(t.tool_call_message);
    setTaskCreateMsg(t.task_create_message);
    setToolNames(t.tool_names ?? []);
  }

  // submit は editingId に応じて「新規作成」または「変更を保存」を行う。編集時はそのトリガーの有効状態を保持する。
  async function submit() {
    const n = onInterval ? Math.max(1, Math.floor(Number(intervalSec) || 0)) : 0;
    if (n === 0 && !onFinding && !onGoalMet && !onTaskTimeout && !onToolCall && !onTaskCreate) {
      toast.error("少なくとも 1 つのトリガー条件を選択してください");
      return;
    }
    if (onToolCall && toolNames.length === 0) {
      toast.error("ツール呼び出しトリガーでは少なくとも 1 つのツールを選択してください");
      return;
    }
    const body = {
      interval_sec: n,
      on_finding: onFinding,
      on_goal_met: onGoalMet,
      on_task_timeout: onTaskTimeout,
      on_tool_call: onToolCall,
      on_task_create: onTaskCreate,
      interval_message: intervalMsg.trim(),
      finding_message: findingMsg.trim(),
      goal_message: goalMsg.trim(),
      task_timeout_message: taskTimeoutMsg.trim(),
      tool_call_message: toolCallMsg.trim(),
      task_create_message: taskCreateMsg.trim(),
      tool_names: onToolCall ? toolNames : [],
    };
    setSaving(true);
    try {
      if (editingId != null) {
        const cur = triggers.find((x) => x.id === editingId);
        await api.updateTrigger(editingId, { ...body, enabled: cur?.enabled ?? true });
        toast.success("変更を保存しました");
      } else {
        await api.createTrigger(agentKey, { ...body, enabled: true });
        toast.success("トリガーを追加しました");
      }
      resetForm();
      reload();
    } catch (e) {
      toast.error((editingId != null ? "保存に失敗しました：" : "追加に失敗しました：") + (e as Error).message);
    } finally {
      setSaving(false);
    }
  }
  async function toggleEnabled(t: AgentTrigger) {
    try {
      await api.updateTrigger(t.id, {
        enabled: !t.enabled,
        interval_sec: t.interval_sec,
        on_finding: t.on_finding,
        on_goal_met: t.on_goal_met,
        on_task_timeout: t.on_task_timeout,
        on_tool_call: t.on_tool_call,
        on_task_create: t.on_task_create,
        interval_message: t.interval_message,
        finding_message: t.finding_message,
        goal_message: t.goal_message,
        task_timeout_message: t.task_timeout_message,
        tool_call_message: t.tool_call_message,
        task_create_message: t.task_create_message,
        tool_names: t.tool_names,
      });
      reload();
    } catch (e) {
      toast.error("保存に失敗しました：" + (e as Error).message);
    }
  }
  async function del(id: number) {
    try {
      await api.deleteTrigger(id);
      if (editingId === id) resetForm();
      reload();
    } catch (e) {
      toast.error("削除に失敗しました：" + (e as Error).message);
    }
  }

  function condLabel(t: AgentTrigger): string {
    const parts: string[] = [];
    if (t.interval_sec > 0) parts.push(`${t.interval_sec} 秒ごと`);
    if (t.on_finding) parts.push("finding 検出時");
    if (t.on_goal_met) parts.push("ゴール達成時");
    if (t.on_task_timeout) parts.push("タスクタイムアウト時");
    if (t.on_tool_call) parts.push(`ツール呼び出し（${t.tool_names.length}）`);
    if (t.on_task_create) parts.push("タスク作成時");
    return parts.join(" · ") || "（条件なし）";
  }

  return (
    <div className="grid gap-4">
      <p className="text-muted-foreground text-xs">
        トリガーはこのカスタム Agent を自動実行させる。トリガーが発火するたびに<b>新しいセッションで実行</b>される
        （「チャット」ページで確認できる）。トリガー条件は複数選択可能で、システムは「今回なぜ発火したか + 関連する
        タスク/finding/ゴール」を、入力した基本メッセージの後ろに自動で付加する。
      </p>

      {/* トリガー後の処理ポリシー */}
      <div className="grid gap-3 rounded-md border p-3">
        <Label className="text-muted-foreground text-xs">
          トリガー後の処理ポリシー（トリガーのキューイング/統合実行の方法を決める）
        </Label>
        <div className="flex flex-wrap items-center gap-4">
          <div className="grid gap-1">
            <Label className="text-xs">実行モード</Label>
            <Select
              value={runMode}
              onValueChange={(v) => {
                const rm = v as "serial" | "parallel";
                setRunMode(rm);
                saveBehavior({ trigger_run_mode: rm });
              }}
            >
              <SelectTrigger size="sm" className="h-8 w-40">
                <SelectValue />
              </SelectTrigger>
              <SelectContent position="popper">
                <SelectItem value="serial">直列（キューに並べ、1 件ずつ）</SelectItem>
                <SelectItem value="parallel">並列（それぞれ独立したセッションで並行実行）</SelectItem>
              </SelectContent>
            </Select>
          </div>

          <div className="grid gap-1">
            <Label className="text-xs">統合モード</Label>
            <Select
              value={mergeMode}
              disabled={runMode === "parallel"}
              onValueChange={(v) => {
                const mm = v as "by_task" | "all" | "none";
                setMergeMode(mm);
                saveBehavior({ trigger_merge_mode: mm });
              }}
            >
              <SelectTrigger size="sm" className="h-8 w-44">
                <SelectValue />
              </SelectTrigger>
              <SelectContent position="popper">
                <SelectItem value="by_task">タスク単位で統合</SelectItem>
                <SelectItem value="all">すべて 1 件に統合</SelectItem>
                <SelectItem value="none">統合しない</SelectItem>
              </SelectContent>
            </Select>
          </div>

          {runMode === "parallel" && (
            <div className="grid gap-1">
              <Label htmlFor="tr-maxpar" className="text-xs">
                最大同時実行数（0=無制限）
              </Label>
              <Input
                id="tr-maxpar"
                type="number"
                min={0}
                className="h-8 w-28"
                value={maxParallel}
                onChange={(e) => setMaxParallel(e.target.value)}
                onBlur={() => {
                  const n = Math.max(0, Math.floor(Number(maxParallel) || 0));
                  setMaxParallel(String(n));
                  saveBehavior({ trigger_max_parallel: n });
                }}
              />
            </div>
          )}
        </div>
        <p className="text-muted-foreground text-xs">
          {runMode === "parallel"
            ? "並列：トリガーごとに即座にセッションを開いて並行実行し、統合しない。最大同時実行数を超えたトリガーは空きを待ってキューに並ぶ。"
            : mergeMode === "by_task"
              ? "直列・タスク単位で統合：同じ agent は 1 回に 1 つ実行。キュー内の同一タスクのイベントトリガーは 1 つのセッションに統合される。"
              : mergeMode === "all"
                ? "直列・すべて統合：同じ agent は 1 回に 1 つ実行。キュー取り出し時に、現在キューにあるすべてのトリガーを 1 つのセッションに統合する。"
                : "直列・統合なし：同じ agent は 1 回に 1 つ実行。各トリガーがそれぞれ 1 つのセッションになる。"}
        </p>
      </div>

      {/* トリガーの新規作成 / 編集 */}
      <div className="grid gap-3 rounded-md border p-3">
        <Label className="text-muted-foreground text-xs">
          {editingId != null
            ? `トリガー #${editingId} を編集中（変更後「変更を保存」をクリック）`
            : "トリガーを追加（条件ごとに個別のユーザーメッセージを設定可能）"}
        </Label>

        {/* 定期実行 */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onInterval} onCheckedChange={(v) => setOnInterval(!!v)} /> 定期トリガー
          </label>
          {onInterval && (
            <div className="grid gap-1.5">
              <div className="flex items-center gap-2">
                <Label htmlFor="tr-interval" className="text-xs">
                  間隔
                </Label>
                <Input
                  id="tr-interval"
                  type="number"
                  min={1}
                  className="h-8 w-24"
                  value={intervalSec}
                  onChange={(e) => setIntervalSec(e.target.value)}
                />
                <span className="text-muted-foreground text-xs">秒</span>
              </div>
              <Textarea
                className="text-xs"
                rows={2}
                value={intervalMsg}
                placeholder="定期トリガー時に agent へ送るメッセージ（例：すべてのタスクを巡回点検する）"
                onChange={(e) => setIntervalMsg(e.target.value)}
              />
            </div>
          )}
        </div>

        {/* finding */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onFinding} onCheckedChange={(v) => setOnFinding(!!v)} /> finding 検出時にトリガー
          </label>
          {onFinding && (
            <Textarea
              className="text-xs"
              rows={2}
              value={findingMsg}
              placeholder="finding 検出時に agent へ送るメッセージ（システムがタスクと finding の詳細を付加する）"
              onChange={(e) => setFindingMsg(e.target.value)}
            />
          )}
        </div>

        {/* ゴール達成 */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onGoalMet} onCheckedChange={(v) => setOnGoalMet(!!v)} /> ゴール達成時にトリガー
          </label>
          {onGoalMet && (
            <Textarea
              className="text-xs"
              rows={2}
              value={goalMsg}
              placeholder="ゴール達成時に agent へ送るメッセージ（システムがタスクと達成したゴールを付加する）"
              onChange={(e) => setGoalMsg(e.target.value)}
            />
          )}
        </div>

        {/* タスクタイムアウト */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onTaskTimeout} onCheckedChange={(v) => setOnTaskTimeout(!!v)} />{" "}
            タスクタイムアウト時にトリガー
          </label>
          {onTaskTimeout && (
            <Textarea
              className="text-xs"
              rows={2}
              value={taskTimeoutMsg}
              placeholder="タスクタイムアウト時に agent へ送るメッセージ（システムがタスク番号とゴールを付加する）"
              onChange={(e) => setTaskTimeoutMsg(e.target.value)}
            />
          )}
        </div>

        {/* ツール呼び出し */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onToolCall} onCheckedChange={(v) => setOnToolCall(!!v)} /> ツール呼び出し時にトリガー
          </label>
          {onToolCall && (
            <div className="grid gap-1.5">
              <div className="text-muted-foreground text-xs">
                監視するツールを選択する（1 つ以上）。タスク実行中、これらのツールの<b>呼び出し完了</b>
                ごとにトリガーされる。選択済み {toolNames.length} 件。
              </div>
              <div className="max-h-40 overflow-y-auto rounded-md border p-2">
                {tools.length === 0 && <span className="text-muted-foreground text-xs">（ツール一覧が空）</span>}
                <div className="grid gap-1">
                  {tools.map((tool) => (
                    <label key={tool.key} className="flex items-start gap-2 text-xs">
                      <Checkbox
                        className="mt-0.5"
                        checked={toolNames.includes(tool.key)}
                        onCheckedChange={() => toggleTool(tool.key)}
                      />
                      <span className="min-w-0">
                        <span className="font-medium">{tool.key}</span>
                        {tool.description && (
                          <span className="text-muted-foreground line-clamp-1"> {tool.description}</span>
                        )}
                      </span>
                    </label>
                  ))}
                </div>
              </div>
              <Textarea
                className="text-xs"
                rows={2}
                value={toolCallMsg}
                placeholder="ツール呼び出し時に agent へ送るメッセージ（システムがタスク情報、ツールの入力と戻り値を付加する）"
                onChange={(e) => setToolCallMsg(e.target.value)}
              />
            </div>
          )}
        </div>

        {/* タスク作成 */}
        <div className="grid gap-1.5">
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={onTaskCreate} onCheckedChange={(v) => setOnTaskCreate(!!v)} /> タスク作成時にトリガー
          </label>
          {onTaskCreate && (
            <Textarea
              className="text-xs"
              rows={2}
              value={taskCreateMsg}
              placeholder="タスク作成時に agent へ送るメッセージ（システムがタスク番号とゴールを付加する）"
              onChange={(e) => setTaskCreateMsg(e.target.value)}
            />
          )}
        </div>

        <div className="flex items-center gap-2">
          <Button size="sm" onClick={submit} disabled={saving}>
            <SaveIcon /> {editingId != null ? "変更を保存" : "トリガーを追加"}
          </Button>
          {editingId != null && (
            <Button size="sm" variant="ghost" onClick={resetForm} disabled={saving}>
              <XIcon /> 編集をキャンセル
            </Button>
          )}
        </div>
      </div>

      {/* 既存のトリガー */}
      <div className="grid gap-2">
        <Label className="text-muted-foreground text-xs">既存のトリガー</Label>
        {triggers.length === 0 && <span className="text-muted-foreground text-xs">（なし）</span>}
        {triggers.map((t) => (
          <div
            key={t.id}
            className={cn(
              "flex items-start gap-2 rounded-md border p-2 text-sm",
              editingId === t.id && "border-primary bg-primary/5",
            )}
          >
            <Switch checked={t.enabled} onCheckedChange={() => toggleEnabled(t)} className="mt-0.5" />
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="font-medium">{condLabel(t)}</span>
                {!t.enabled && (
                  <Badge variant="outline" className="text-destructive px-1 py-0 text-[9px]">
                    無効化済み
                  </Badge>
                )}
              </div>
              <div className="text-muted-foreground grid gap-0.5 text-xs">
                {t.interval_sec > 0 && t.interval_message && (
                  <div className="line-clamp-1">定期：{t.interval_message}</div>
                )}
                {t.on_finding && t.finding_message && <div className="line-clamp-1">finding：{t.finding_message}</div>}
                {t.on_goal_met && t.goal_message && <div className="line-clamp-1">ゴール：{t.goal_message}</div>}
                {t.on_task_timeout && t.task_timeout_message && (
                  <div className="line-clamp-1">タイムアウト：{t.task_timeout_message}</div>
                )}
                {t.on_task_create && t.task_create_message && (
                  <div className="line-clamp-1">タスク作成：{t.task_create_message}</div>
                )}
                {t.on_tool_call && (
                  <>
                    <div className="line-clamp-1">ツール：{t.tool_names.join("、") || "（未選択）"}</div>
                    {t.tool_call_message && <div className="line-clamp-1">メッセージ：{t.tool_call_message}</div>}
                  </>
                )}
              </div>
            </div>
            <Button
              variant="ghost"
              size="icon-sm"
              className="text-muted-foreground hover:text-foreground"
              onClick={() => startEdit(t)}
              title="編集"
            >
              <PencilIcon className="size-3.5" />
            </Button>
            <Button
              variant="ghost"
              size="icon-sm"
              className="text-muted-foreground hover:text-destructive"
              onClick={() => del(t.id)}
              title="削除"
            >
              <Trash2Icon className="size-3.5" />
            </Button>
          </div>
        ))}
      </div>
    </div>
  );
}
