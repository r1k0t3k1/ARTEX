package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), "tavily"
// (TavilyKey required), or "deepseek" (DeepSeek* required, filled from the
// active LLM profile). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy — set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
//
// 注意：deepseek バックエンドは他の 3 つと性質が異なる。DeepSeek には直接呼び出せる検索 API がなく、
// 検索は Anthropic 互換 messages API の内部（web_search_20250305 server tool）にのみ存在する。
// そのため検索のたびにモデル呼び出しを 1 回消費し、検索リクエストは DeepSeek 側から発行される——
// 本機の Proxy を通らず、トラフィックにも記録されない。
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek* は現在有効な LLM 設定（anthropic 形式の DeepSeek 公式エンドポイントのみ）由来で、
	// 個別設定はなく、LLM 設定の切り替えに伴って変わる。
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch       WebSearchOpts     // web_search tool backend selection (off by default)
	tx              *transcript.Store // raw LLM conversation persistence (nil = off)
	window          int               // context window in tokens (for compaction)
	windowFn        func() int        // optional dynamic task-chain minimum
	maxTurns        int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// noaEnabledFn resolves whether this run uses the experimental noa context-
	// compression mechanism. Read per run, like nonStreaming. nil = off (built-in
	// compaction).
	noaEnabledFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop probing, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
const settleWrapUpPrompt = "予算切れによりまもなく終了する。これ以上コマンド/探査を実行しないこと。次の順で行う：(1) 上で識別したがまだ書き戻していない内容を 1 件ずつ書き戻す——新規資産は insert_assets、探索結論/事実は record_fact、確認済みの脆弱性は report_finding；(2) **最後に単独の 1 文のプレーンテキストで**、何をしたか・得られた重要な結論は何かをまとめる（この文が今回の実行結果として表示されるので、必ず出力すること）。"

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept returns actool.DefaultTools() minus the named tools (by
// CoreTool.Name()). Used to trim SDK default tools an agent shouldn't have.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it — so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr → nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl is the built-in EDITABLE body (セクション [A]) of the worker system
// prompt, seeded into agent_prompts. The trafficTool block and the 中間成果物出力規約
// are NOT here — they are code-owned and appended by workerSystem after rendering
// (セクション [B]/[C]), so editing the DB body can never drop them.
const workerDefaultTmpl = `あなたはネットワークセキュリティプラットフォームの許可済みペネトレーションテストシステムにおける「実行者」（work agent）である。あなたは【1 つの意図】（1 文の探索方向）を受領しており、唯一の責務は：**この意図を完了し、発見を知識グラフに書き戻し、その後停止して戻る。**

**境界（レッドライン）**：
1. **受領したこの 1 つの意図だけを行う**。**本意図を探る際に、本意図の外で深掘りに値する手がかり**（エラーから漏れたパス、他の資産と連動しうる点、別のエクスプロイトチェーンの入口の疑い）**を見かけたら、fact の summary に一言記してプランナーに渡す**。
2. 最初に阻まれても（payload がフィルタされる / 404 / インジェクションに応答がない）、探し尽くしたことにはならない——本意図の回避手段をすべて試してから結論を出す。
3. 許可された範囲内でのみ操作する。システムプロンプトの冒頭に【操作制約】があれば、それは最優先のレッドラインである：各コマンド/探査の実行前に自己検査し、違反するなら行わない（たとえそれが受領した意図に含まれていても）。

**発見しながら書き戻す**（グラフに書いて初めて意味を持つ。頭の中や文章だけでは数えない；結果が得られるたびに即書き、最後まで溜めてステップ数切れで失わない）。書き戻しは 3 種類。グラフを混同しないこと：
- **新規資産/リソース → insert_assets（資産グラフ）**：サブドメイン / service / endpoint / フィンガープリント / 認証情報など、資産【そのもの】。**ここには資産だけを登録する；探索結論/判断はここに書かず、record_fact を使う。**
- **探索結論/事実 → record_fact（探索グラフ、intent_id を渡す）**：すべてこれを使う。**複数の観察を【1 件】の事実にまとめる**（summary に 1 文の要約 + detail に要約の展開を書く。実際の実行過程に基づくこと）。属性ごとに 1 件、意図ごとに通常 1 件とし、細かく割るとグラフが無限に膨張する——**既定では 1 件だけ書き、detail に併合できるものはすべて併合する**；【互いに完全に独立し、併合できない】結論が確かにある場合にのみ facts 配列で分割する。これはごく少数の例外であり、通常ではない。**増分だけを書く**：今回【新たに得た】ものだけを記録し、既存の事実を言い換えて再記録しない（既存の裏付けだけで新規性がなければ記録不要）。**実際に見たものだけを書く**：evidence を付け（1 行：コマンド + 最も証明力のある出力 1〜2 行。簡潔に。詳細は detail へ）、confidence を付ける（observed=直接見た / inferred=現象からの推測）。
- **確認済みの脆弱性 → report_finding（探索グラフ、PoC を含む、intent_id を渡す）**：**今回自分が実際にトリガーし、再現可能な証拠（リクエスト/レスポンスまたはコマンド出力）を得た場合にのみ使う**。「バージョン/フィンガープリントが CVE に一致」「パラメータがインジェクション可能に見える」「外部の脆弱性 DB/更新履歴/コード diff からの推測」を確認済みとして扱うのは厳禁。CVE データベースの検索やパッチバージョンの比較で実際のトリガーを代替してもならない。トリガーできないが疑わしい → record_fact で inferred の事実を 1 件記録し（疑いの点+なぜトリガーできなかったか）プランナーに渡す。無理に finding として記録しない。


本意図を完了したら、何をしたか、どの事実を書き戻したかを 1 文でまとめる。`

// workerTrafficBlock is セクション [B]: the traffic-tool note, code-injected only when
// traffic capture (recording) is on — i.e. the traffic_* tools actually exist.
// Gated on recording, NOT on the egress proxy: a global proxy with capture off
// routes traffic but records nothing, so the tools would not be there. Not stored,
// not editable.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**トラフィックツール**：\n- traffic_search / traffic_get / traffic_blob：応答を見返し、既にアクセスしたリソースを探す。**まずトラフィックを調べ、同じ URL を重複して curl しない**。traffic_search は **host の指定が必須**で、既定ではごく軽量な索引を 3 件だけ返す（id/method/url/status/resp_len、応答内容なし）。もっと必要な場合は limit を明示的に増やす；body_contains でリクエスト/レスポンス本文の全文検索ができる（最低 3 文字、部分文字列と中国語に対応。パスワード/鍵/エラー/内部アドレスの探索など）。ある 1 件の原文を見るには traffic_get(id) を使い、巨大な本文は @blob sha256:<hash> として表示されるので、traffic_blob(hash) で分割取得して全文を読む。"
}

// artifactSpec is セクション [C]: the code-owned, non-editable tail appended to every
// pentest agent's prompt — intermediate artifacts must land in the shared work
// dir, never /tmp. Guaranteed present regardless of how the DB body is edited.
func artifactSpec(dir string) string {
	return "\n\n**中間成果物出力規約**：スクリプト、payload、取得したレスポンスボディ、一時データなど、あらゆる中間成果物は、**必ず本タスクの作業ディレクトリ " + dir + " に書き出す**（相対パスはここに解決される。この絶対パスを使ってもよい）——**/tmp やその他の絶対パスには書かないこと**。"
}

// workerArtifactSpec is the worker's セクション [C]: its per-intent run dir is pre-created
// by the engine (ensureRunDir), so it just writes relative paths there — no manual
// mkdir, no cross-worker name collisions.
func workerArtifactSpec(runDir string) string {
	return "\n\n**中間成果物出力規約**：スクリプト、payload、取得したレスポンスボディ、一時データなど、あらゆる中間成果物は、**必ず今回の意図専用の作業ディレクトリ " + runDir + " に書き出す**（自動で作成済み。相対パスでそのままここに書けばよく、手動でディレクトリを作る必要はない）——**/tmp やその他の絶対パスには書かないこと**。"
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 → task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir — on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered — so it gates the traffic-tool note.
	// Optional finding guidance is added for every role after tool resolution.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir)
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only — same move as the planner's situational block.
// intentAssetIDs pulls the intent's target asset ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n【あなたが受領した意図（今回の唯一のタスク：これだけを行い、事実のみを生み出し、終わったら即停止）】：\n%s\n意図 id: %d（record_fact / report_finding へ書き戻すときに渡す）", string(intent.Payload), intent.ID)
}

// renderWorkerGraphOverview folds the global situational snapshot into the worker's
// launch USER message for AWARENESS ONLY. The framing is deliberately strong: the overview
// must NOT widen the worker's job — it still does only its assigned intent. Its sole
// purpose is letting the worker read context (existing facts/assets/hints)
// so it avoids redundant work and doesn't re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// coverage はプランナーが「どの種類のテストが少ない / 範囲を広げるか」を判断するシグナルであり、
	// worker の「受領した意図だけを行い、未カバーの点を追わない」という責務境界に反する →
	// worker のビューから除去する。data は今回の worker 専用の新しい map であり、キーを削っても planner には影響しない。
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n\n【グローバル探索状況（読み取り専用。自分が受領した意図を大局に位置づけるためのもの）】：\n" +
		"以下はタスク全体の現在の探索概況である。用途は 2 つ：1 つは他の人が何を発見したかを知り、重複しないこと；もう 1 つは自分が受領した意図を探るときに、それが全体とどう関係するかを連想できるようにすること。\n" +
		"**発散は良いこと**：本意図を探るときは大いに深く考え、多く連想してよい。唯一の境界線は——本当に手を動かして他の意図を実行しないこと（それは他の worker の仕事であり、プランナーがアサインする）。価値ある手がかり（資産をまたぐ連動、別のエクスプロイトチェーンの入口の疑い、全体レベルの不審点）を連想したら、**必ず fact に書いてプランナーに渡す**——これはあなたの重要な産出であり、有っても無くてもよいものではない。プランナーに判断させるため 1 件多く報告するほうが、自分で飲み込むよりよい。\n" +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight — with context
// on which intent found what — instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent → visible to the task
	tsx.SetEnrich(enr)                  // async DNS/HTTP auto-completion for assets this worker writes
	tsx.SetNotifyFinding(notifyFinding) // report_finding の保存時にその場で planner をウェイクアップする（「どの意図+どの finding」を付けて）
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)
	// worker には意図的に MultiEdit/Glob/Grep を与えない：ファイルの精密編集は Edit、検索は Bash（grep/find）で行う。
	// ツール面を絞り、低価値な呼び出しを減らす。その他の SDK 既定ツール（Read/Write/Edit/LS/Bash/Sleep）は通常どおり。
	base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()

	// 意図は worker の【唯一の責務であり、run 全体を通した不変条件】→ 起動指令、意図がアンカーする
	// 対象資産の生データとともに system prompt に入れる：system は run ごとに組み直され、compaction で
	// 圧縮されることは決してないため、長い run でも意図は常に存在し、続行時も transcript 履歴に最初の
	// メッセージが残っているかどうかに依存しない。代償は system に per-intent の可変データが混ざり、
	// 意図をまたいだキャッシュ再利用が失われること；これは意図的なトレードオフ（意図の喪失は token 節約より
	// はるかに重大）。planner の「状況ブロックを user turn に置く」方式と分岐しているのは意図的：planner は
	// 意図を生む側で単一の mandate を持たないが、worker は持つ。ただし【グローバル状況 overview】だけは
	// 起動 user メッセージに残す——これは劣化可能で stale を許容し、圧縮しても問題ない。
	// 今回の意図専用の作業ディレクトリ <workDir>/tasks/<taskID>/i<intentID>。エンジン側で先に作成する。
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// The run-wide intent is not the current tool action. Do not forward it or
	// inherit a parent run's background into the action reviewer.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // 操作制約（あれば）をシステムプロンプトに注入し、worker は実行時に厳守する
	}
	// 意図ブロック → 意図がアンカーする資産ブロック → 起動指令の順に system の末尾へ追加する（constraintBlock と同じ追記方式）。
	sysBody += renderIntentTask(intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					sysBody += "\n\n本意図の asset_ids に対応する対象資産：\n" + string(b)
				}
				// 意図が明確に対象とするこれらの資産 → 自動的にタスクのテスト範囲に取り込む（insertAssets と
				// 同じ保守的な粒度）。upsertTaskScope の ON CONFLICT DO NOTHING と uq_task_scope
				// 一意インデックスにより重複追加は起きない；再実行/再試行も同様に冪等な no-op。
				// 資産カバレッジ機能が無効なときはテスト範囲（分母）を積み増さない。
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	sysBody += "\n\n上記の意図の実行を開始する：それだけを行い、事実・assets・finding のみを生み出し、終わったら即停止する。"
	system, boundary := deferredSystem(sysBody, def)
	// タスクレベルの deadline（ctx 経由で注入）で本 run の実時間予算をクランプし、締めくくり文言を決める（taskclock.go 参照）。
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch は記録プロキシ経由で、その HTTP は curl と同様に記録される；プロキシ CA を読み込むことで
		// MITM で再署名された HTTPS 証明書が【正常に検証を通る】（検証を切るのではない）。proxy が空なら直接接続。
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// Web 検索（任意）。ddgs は key 不要；brave-free は BraveKey、tavily は TavilyKey が必要。
		// WebSearchProxy は独立した出口プロキシ（http/https/socks5）で、トラフィック記録用の MITM プロキシとは
		// 無関係；空なら直接接続。
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// Bash サブコマンドの HTTP は既定で記録プロキシ経由 + その CA を信頼する（ツール側で -x/-k が不要）。
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// 実時間予算。ターン境界で判定し、途中では中断しない；0 = 無制限。タスクレベルの deadline がある場合は
		// min(自身の予算, deadline までの残り) にクランプし、本 run がタスク到達時に自然と締めくくりへ入るようにする（taskclock.go 参照）。
		MaxDuration: maxDur,
		// 予算到達（ターン数 OR 時間）→ SDK が締めくくりターンを実行する（Bash を隠す）。識別済みの内容を書き戻し、
		// 未完了のまま終わらせない。clamped（タスク deadline にクランプ）時は PromptByReason を使う：タイムアウト=
		// タスク到達→タスクのタイムアウト文言、ステップ数=クランプされた窓内でステップが先に尽きた→per-run の文言に
		// 落下する。非 clamped なら純粋な per-run を維持。
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK tool.Capture);
		// full output preserved on disk. 切り詰め上限は SDK の既定値（30000 文字）。
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // セッション単位の一時的な待ち項目（TodoWrite）。計画専用で、終了時に破棄。
		NonStreaming:  w.nonStreaming(),                       // この profile が非ストリーミングを選んだ場合は Provider.Complete を使う
		MaxTokens:     w.maxTokens(),                          // 0 = 上限を送らない（サーバー側の既定値に任せる）
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// 意図 / 起動指令 / 意図がアンカーする資産は system prompt で既に下ろしてある（上記 sysBody の組み立てを参照）。
	// この起動 user メッセージは【グローバル状況 overview】だけを運ぶ——劣化可能な大局情報であり、圧縮しても問題ない。
	// overview が稀に marshal 失敗して空になった場合は、起動の一言にフォールバックし、初回ターンで空の user メッセージが出ないようにする。
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "system で受領した意図の実行を開始する：それだけを行い、事実・assets・finding のみを生み出し、終わったら即停止する。"
	}

	// 実験機能：有効化すると noa がコンテキスト圧縮を引き継ぐ（アーカイブは <workDir>/noa/<SessionID> 配下に集約、永続）。
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		_ = s.Resume(opts.SessionID)
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "実行を続ける。"
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "前回の人工対話で入力された新しい意図の実行を続ける。既に完了した動作を繰り返さないこと。"
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n【人工対話で入力された新しい意図】\n" + message +
				"\n\nこの人工入力に従って直ちに実行し、完了後にコンテキストに基づいて元のタスクを続ける必要があるか判断すること。"
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n【人工対話で入力された新しい意図】\n" + message +
				"\n\nこの人工入力を優先して実行すること。"
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}
