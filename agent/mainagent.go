package agent

import (
	"context"
	"fmt"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// MainAgent is the thin human-interface orchestrator (docs §4.2 / §7). The human
// chats with it; it observes (read tools), and steers by injecting hints
// (→planner) or direct high-priority intents (→frontier). It does NOT run the
// autonomous intent-generation loop (that is the planner's job).
type MainAgent struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	tx              *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window          int                                    // context window in tokens (for compaction)
	windowFn        func() int                             // optional dynamic task-chain minimum
	maxTurns        int                                    // max agent turns per run (0 = unlimited)
	proxyAddr       string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert     string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch       WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir         string                                 // shared work dir (surfaced in prompt as artifact-output target)
	steerWork       func(intentID int64, msg string) error // engine callback: steer a running work (nil = off)
	nonStreamingFn  func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn    func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn     func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
}

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (m *MainAgent) SetNoaEnabled(fn func() bool) { m.noaEnabledFn = fn }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (m *MainAgent) SetNonStreaming(fn func() bool) { m.nonStreamingFn = fn }

func (m *MainAgent) nonStreaming() bool { return m.nonStreamingFn != nil && m.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (m *MainAgent) SetMaxTokens(fn func() int) { m.maxTokensFn = fn }

func (m *MainAgent) maxTokens() int {
	if m.maxTokensFn == nil {
		return 0
	}
	return m.maxTokensFn()
}

func NewMainAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *MainAgent {
	return &MainAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns}
}

func (m *MainAgent) SetCompactionWindowResolver(fn func() int) { m.windowFn = fn }

func (m *MainAgent) compactionWindow() int {
	if m.windowFn != nil {
		return m.windowFn()
	}
	return m.window
}

// SetProxy points the main agent's WebFetch at the recording proxy plus the CA
// cert it trusts to verify HTTPS through it (empty addr = direct).
func (m *MainAgent) SetProxy(addr, caCert string) { m.proxyAddr, m.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the main agent (off by default).
func (m *MainAgent) SetWebSearch(o WebSearchOpts) { m.webSearch = o }

// SetSteerWork wires the engine callback that lets the main agent's steer_work
// tool inject a mid-run course-correction into a running work (nil = tool off).
func (m *MainAgent) SetSteerWork(fn func(intentID int64, msg string) error) { m.steerWork = fn }

// mainAgentDefaultTmpl is the built-in EDITABLE body (セクション [A]) of the main agent
// prompt, seeded into agent_prompts. Goal is a {{.Goal}} template var; the
// 中間成果物出力規約 tail is code-owned (artifactSpec), appended after rendering.
const mainAgentDefaultTmpl = `あなたは許可済みペネトレーションテストシステムの「主 agent」であり、人間のオペレーターのインターフェースである。自分では探索せず、意図を自律的に連続生成もしない（それはプランナーの仕事）。あなたの責務：

1. 観察：graph_overview / list_findings / list_facts / list_assets / get_worker_output で、現在の進捗に関する人の質問に答える。
2. 操舵（人の意図をシステムに落とす）：
   - 人が「方向を変えたい/ある種の脆弱性を強調したい/ある領域を重点にしたい」→ add_hint でヒントを書く（プランナーが次回読む）。
   - 人が「ある具体的な対象をすぐテストしたい」→ add_intent で高優先度の意図（priority 8-10）を直接注入する。システムは完了済みのタスクを自動で実行状態へ戻し、worker がこの意図を受け取って実行し、終われば完了状態に戻る。
     **タスクゴールがすべて達成済みの場合**（graph_overview で goals がすべて met）：下ろす前に、この意図の背後に「新たに達成すべき結果」が含まれていないかを判断する。含まれるなら、推測したゴールを 1 文で人に復唱し、**正式なゴールとして登録するかどうかを問い返す**——人が要る → set_goals で登録する（タスクはその後通常の計画に入り、プランナーが自律的に進める）；人が不要 / 一時的に探ってみたいだけ → add_intent だけでこの 1 件を下ろし、worker が実行し終えればタスクは完了状態に戻る（自律的に続行しない）。この意図が明らかに一度きりの確認で新たなゴールを含まないなら、そのまま add_intent でよく、毎回問い返す必要はない。
   - 人が「実行中の意図(work)に対してリアルタイムに軌道修正したい（もう X をやらず Y に集中）」→ steer_work を使う（中断せず、既存の進捗も失わず、worker の次の動作前に効く）；まず get_worker_output で何をしているか見る。方向が完全に誤っているなら、代わりに add_intent で新しい意図を下ろす。
   - 人が「達成すべき最終ゴールを追加したい」→ set_goals でゴールを追加する。システムはそのゴールをタスクグラフに書き込み、**完了/一時停止中のタスクを自動で実行状態へ戻して続行させる**（プランナーがその後これを踏まえて達成判定をやり直す）。人手で再開を押す必要はない。
   - 人が「テスト制約を追加/変更したい（ある種の操作を許可/禁止。例『現在のポートのみテスト』『ブルートフォース禁止』『受動的偵察のみ』）」→ set_constraints で登録する（type=allow 許可 / type=deny 禁止）。制約は次回の計画時に planner/worker のプロンプトへ注入され、探索境界を画定する。概要の「制約管理」で追加・削除・変更もできる。
3. 平易な言葉で簡潔に返答し、何をしたかを説明する。

現在のタスクゴール：{{.Goal}}

発見を捏造しないこと；ツールが返す実際のデータだけに基づいて答える。`

func mainAgentSystem(goal, dataDir, workDir string) string {
	body := renderSystem("mainagent", mainAgentDefaultTmpl, MainVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Chat handles one human message and returns the assistant reply. emit, if
// non-nil, receives each execution step (thinking / tool_use / tool_result /
// text / result) so the main-agent session shows its work — exactly like the
// worker/planner sessions — not just the final answer.
func (m *MainAgent) Chat(ctx context.Context, taskID int64, mainSeg int, as *db.AssetStore, ts *db.ExplorationStore, goal, message string, emit func(db.Activity), notify, resume func(), notifyGoal, notifyHint func([]string)) (string, error) {
	tsx := NewToolSet(ts, "human")
	tsx.SetFindingRecorder(m.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.SetNotify(notify)         // 汎用ウェイクアップ（専用コールバックのない書き込み操作はこれを使う。debounce あり）
	tsx.SetResumeTask(resume)     // set_goals でゴール追加 → 完了/一時停止中のタスクを running に戻す
	tsx.SetNotifyGoal(notifyGoal) // set_goals でゴール追加 → planner に「人がゴールを追加：…」のトリガーを記録
	tsx.SetNotifyHint(notifyHint) // add_hint でヒント追加 → planner に「人が戦略ヒントを N 件追加：…」のトリガーを記録
	tsx.steerWork = m.steerWork   // enable steer_work tool (nil = unavailable)
	// ドメインツール + 基本の既定ツールセット（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// 資産カバレッジ機能が無効なら add_task_scope/list_untested_assets を除外する（プロンプトに入れない）。
	base := append(tsx.DropCoverageTools(tsx.MainAgentTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "mainagent", base)
	defer cleanup()
	// 本タスクの作業ディレクトリ <workDir>/tasks/<taskID>。先に作成しておく。
	mainDir := ensureRunDir(m.workDir, taskID, 0)
	ctx = intercept.WithReviewWorkingDirectory(ctx, mainDir)
	system, boundary := deferredSystem(mainAgentSystem(goal, m.workDir, mainDir), def)
	opts := agentcore.Options{
		Provider:        m.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 記録プロキシ経由で足跡を残す；プロキシ CA を読み込み MITM 再署名の HTTPS 証明書を検証する
		WebFetchProxy:   m.proxyAddr,
		WebFetchCACert:  m.proxyCACert,
		// Web 検索（任意）。ddgs は key 不要；brave-free は BraveKey、tavily は TavilyKey が必要。
		// WebSearchProxy は独立した出口プロキシ（http/https/socks5）で、トラフィック記録用の MITM プロキシとは
		// 無関係；空なら直接接続。
		EnableWebSearch:       m.webSearch.Enabled,
		WebSearchBackend:      m.webSearch.Backend,
		BraveSearchAPIKey:     m.webSearch.BraveKey,
		TavilySearchAPIKey:    m.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: m.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  m.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   m.webSearch.DeepSeekModel,
		WebSearchProxy:        m.webSearch.Proxy,
		BashEnv:               proxyEnv(m.proxyAddr, m.proxyCACert), // Bash サブコマンドは既定でプロキシ経由+CA を信頼
		WorkingDir:            mainDir,                              // 本タスクの作業ディレクトリ <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(mainDir),
		MaxTurns:              m.maxTurns,                             // 0 = unlimited (configurable in agent management)
		Compaction:            compactionConfig(m.compactionWindow()), // long chats stay within the window
		Todos:                 actool.NewTodoStore(),                  // セッション単位の一時的な待ち項目（TodoWrite）。計画専用で、終了時に破棄。
		// 予算（ステップ数）到達→ SDK が締めくくりを実行：ユーザーへ進捗のまとめを 1 文出力する。プロンプトと締めくくりのターン数はバックエンドで編集可能（既定 10 ターン）。
		Settlement:   wrapupSettlement("mainagent", nil),
		NonStreaming: m.nonStreaming(), // この profile が非ストリーミングを選んだ場合は Provider.Complete を使う
		MaxTokens:    m.maxTokens(),    // 0 = 上限を送らない（サーバー側の既定値に任せる）
	}
	if m.tx != nil { // persist raw human↔AI conversation; one accumulating file per segment
		opts.Transcript = m.tx
		// Segment 0 keeps the legacy "exp%d-main" name so existing transcripts still
		// load; each new session (seg>=1) gets its own file for a clean context.
		opts.SessionID = fmt.Sprintf("exp%d-main", ts.ID())
		if mainSeg > 0 {
			opts.SessionID = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
		}
	}
	// 実験機能：有効化すると noa がコンテキスト圧縮を引き継ぐ（アーカイブは <workDir>/noa/<SessionID> 配下に集約、永続）。
	// session id は transcript と同じ規則（セグメント対応）にし、アーカイブと復元を揃える。
	noaSession := fmt.Sprintf("exp%d-main", ts.ID())
	if mainSeg > 0 {
		noaSession = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
	}
	enableNoa(&opts, m.noaEnabledFn, m.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close()
	// reload the prior conversation from the transcript so the agent has context
	// across turns (each Chat is a fresh session; without this it can't see earlier
	// messages). First turn: no file yet → Resume loads nothing and proceeds.
	if m.tx != nil {
		_ = s.Resume(opts.SessionID)
	}
	// C2: this session is fresh each turn; re-unlock skill-gated MCPs from prior
	// Skill() calls in the reloaded history so revealed tools stay callable.
	seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
	text, _, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = "mainagent"
			emit(r)
		}
	})
	return text, err
}
