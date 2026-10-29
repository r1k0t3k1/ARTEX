package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【あなたの計画待ち（ウェイクアップをまたいで保持、前回あなたが書いたもの）】：\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("これに沿って進める：意図を出すのは【前段の手順が完了済み / 依存する fact が既に存在する】次のステップだけにする；TodoWrite でリストを更新する（fact で満たされたステップを completed にする）。リスト内で pending/in_progress のステップを重複して出さないこと。")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = 要約).
//	"goal"    — the human (via the main agent's set_goals) added one OR MORE goals in a
//	            single call (Goals = 今回追加したゴールのテキスト、1 件以上；set_goals はバッチ対応).
//	"goal_deleted" — the human deleted a goal from 概要のゴール管理 (Detail = 削除されたゴールのテキスト).
//	"goal_edited"  — the human edited a goal from 概要のゴール管理 (OldGoal→NewGoal のテキスト).
//	"cancelled" — the human deleted intent IntentID (Detail = 削除理由). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 専用：削除前に捕捉した意図の要約（物理削除後はノードが存在せず再取得できない）
	Goals    []string // Kind=="goal" 専用：今回の set_goals で追加されたゴールのテキスト（1 件または複数）
	OldGoal  string   // Kind=="goal_edited" 専用：変更前のゴールのテキスト
	NewGoal  string   // Kind=="goal_edited" 専用：変更後のゴールのテキスト
	Hints    []string // Kind=="hint" 専用：今回の add_hint で追加されたヒントのテキスト（1 件または複数）
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【今回このラウンドを引き起こした実際の変動（まずここを見てから、方向を補うか決める）】：")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）がゴールを 1 件追加した：%s —— 新たに達成すべきゴールである。これに基づいて探索方向を補うこと（まだ対応する意図がなければ）。", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）がゴールを %d 件追加した：%s —— いずれも新たに達成すべきゴールである。まだ対応する意図がないゴールについて、1 件ずつ探索方向を補うこと。", len(ev.Goals), strings.Join(ev.Goals, "；")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）が戦略ヒントを 1 件追加した：%s —— 探索グラフに追加済みである。これに基づいて探索方向を調整/補充すること（まだ対応する意図がなければ）。", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）が戦略ヒントを %d 件追加した：%s —— すべて探索グラフに追加済みである。1 件ずつこれに基づいて探索方向を調整/補充すること。", len(ev.Hints), strings.Join(ev.Hints, "；")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- 人がこのゴールを削除した：%s —— このゴールは削除された。これに基づいて残りのゴール/方向を再判定すること（もうこのために意図を出さなくてよい）。", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- 人がゴールを「%s」から「%s」に変更した —— 新しいゴールに基づいて探索方向を調整すること（元の方向が不要になったなら出すのを止める）。", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- 意図 #%d（%s）の worker が finding を報告した：%s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// 意図の内容は削除時に捕捉した Summary を優先する（物理削除後はノードが存在せず intentSummary で引けない）。
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- 意図 #%d がユーザーにより削除された。意図の内容は：%s、削除理由は：%s。この意図は削除済み（もう実行されない）；これに基づいて再計画すること。", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- 意図 #%d（%s）の worker が終了した。出力された結論：%s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("；この意図が新たに生み出した事実 id：%s ", fids))
			}
		}
	}
	b.WriteString("\n（完全な詳細は node_detail / get_worker_output / list_findings で再確認できる。）")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(出力の取得に失敗)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(この work にはまだ出力記録がない)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …（切り詰め済み。全文は get_worker_output を参照）"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n【今回の状況（graph_overview のプリフェッチ。あなたがこのツールを呼んだ戻り値と同等；詳細が必要なら node_detail/list_facts などを必要に応じて呼ぶ）】：\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (セクション [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the 中間成果物出力規約
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `あなたはネットワークセキュリティプラットフォームの許可済みペネトレーションテストシステムにおける「プランナー」であり、頻繁にウェイクアップされる（グラフが変わるたびに起動する）。責務：状況を読む → ゴールを判定する → **未カバーの新しい方向が確かにある場合にのみ**探索意図を補充する。あなたはプランナーであり実行者ではない：今回のすべての成果物は【意図の生成/明確化】か【ゴール判定】のいずれかであり、plan の中で作業を自分でやってはならない。

タスクのゴール：{{.Goal}}

**今回のラウンドで意図をいくつ出すべきか（まずこれを考える）**：
- **絶対下限（最優先）**：【ゴール未達成】かつ【現在 open または running の意図が 1 つもない】（frontier_open=0 かつ running_intents が空）なら、今回のラウンドで【必ず】ゴールへ前進する意図を少なくとも 1 つ出す——走っている work もなく、待機中の方向もない状態で意図 0 個＝タスクの停止である；既知の方向が recent_done にしかない場合でも、下記の done/exhausted/blocked の判断に従って別の 1 本を開くか、1 本を続けて出すこと。
- 絶対下限を除けば、**意図 0 個は正常な結果である。ただし正当な理由が必要**（「少なく出すほうが安全」という既定値ではない）：①**カバー済み**——思いついた方向がすべて、まだ open/running の意図で処理されている（言い換えて既存の意図を重複生成するのは重大な誤り）；②**依存待ち**——次のステップが現在走っている work の産出に依存しており、まだ出ていない（このとき無理に出しても下流が前提を得られず空転するので、次回ウェイクアップでグラフが更新されてから出せばよい）。
- 逆に：【未カバーで、走っている work に依存しない】新しい方向がある、またはゴール未達成で範囲内にまだ未テストの面があるなら、出すべきである——意図 0 個を怠惰の既定値にしてはならない。

**ウェイクアップごとの意思決定フロー**：

1. **完全な状況は本プロンプトの下に添付済み**（graph_overview の戻り値そのもの。再度呼ぶ必要はない）：task（元のタイトル＋ゴール/ルートノード）、資産数、goals＋状態、open/running/recent_done の意図、sites_without_endpoints（エンドポイントのないサイト。まだ探るべき方向のヒント）、facts（探索事実の数。脆弱性とは別種）、recent_facts（{id,summary,confidence?}）。
   - **範囲**：探索ノード（goals/意図/facts/findings）は本タスクのみを含む；**資産グラフはグローバル共有**（複数タスクで同一のもの。資産数は範囲内全体の数であり、本タスク固有ではない）——本タスクと無関係な資産が現れたら無視する。
   - **血縁**：各意図には parents（上流：どの事実/意図から派生したか）と yields（下流：どの事実/検出結果を生んだか）があり、recent_facts の各項目には from_intent が付く；これに基づいて「どの事実がどの方向から来たか、新しい方向を統合できるか」を理解する。
   - **否定的/疑わしい観察**（recent_facts の「ポート閉塞/注入不可」など）は worker の観察であり確定ではない：採用する前に node_detail(id) で evidence を見る——evidence が堅牢で、confidence=observed かつ手段を尽くしている場合にのみ、その方向は当面封じられたとみなす；evidence が欠けている、単に「それらしい/一度試しただけ」、または confidence=inferred の場合は【未解明】として扱い、範囲内で他にカバーする意図がなければ、既定で確認用の意図を 1 本出して裏付けまたは反証する（**同じ否定方向の再確認は最大 1 回**；再確認後も否定で証拠が妥当なら、その結論を尊重し、もう出さない）。
   - **より深い詳細が必要なときだけ必要に応じて呼ぶ**：list_facts（ページング、新しい順、既定 20、q でフィルタ、before でページ送り、total/has_more 付き）、list_findings（すべての脆弱性）、node_detail(id)（完全な証拠/詳細；一覧/recent_facts は要約のみ）、list_assets（pull：q 検索、type/company_id/task_id フィルタ、ページング、または id/ids で直接取得）、asset_neighbors。資産はグローバル共有なので、既定で全量を取得しないこと。

2. **ゴール判定（中核の責務）**：goals フィールドにはゴールと状態が既に含まれる；ある検出結果/事実で証明済みの未達成ゴールに対しては、prove_goal(goal_id, evidence_id, reason) を呼んで met にする。**あなたがマークしたものがちょうど最後の未完了ゴールだった場合、システムはタスク全体の完了を自動判定する**——締めくくりは prove_goal を 1 つずつ実行することでのみ駆動され、他に「ワンクリック完了」の手段はない。
   - ⚠️ **定量的な受け入れ確認（早期の判子押しは厳禁）**：ゴールに定量条件（カバレッジ X% 到達、flag を N 個取得、ある権限の獲得）が含まれる場合、prove_goal の前に【必ず】上記 graph_overview の実測値（coverage.pct、findings_total のカウントなど）を照合する：未達なら prove_goal は【禁止】、意図を出して不足を補う；「概ね達成/核心は取った」を理由に早めに met にしてはならない。例：カバレッジ 100% を要求しているのに実測 coverage.pct=40% → 未達成、引き続き補測の意図を出す。

3. **（任意、序盤のみ、ごく軽量）理解のための探査**：グラフにまだ fact がほとんどない（recent_facts がほぼ空、タスクが始まったばかり）場合で、状況だけでは初期意図を具体的に述べられないときに限り、Bash などで対象にごく少量の読み取り専用の探査を行う（例：1〜2 回の curl でトップページ/フィンガープリントを見る）。**唯一の正当な成果物は、より精緻な意図の記述 1 文**——決して脆弱性の発見/検証/エクスプロイトではなく、エンドポイント/ディレクトリ/パラメータの列挙結果でもない（それらは worker の仕事であり、意図として書いて出せ）。3 つの絶対的な境界：
   - グラフに既に worker が産出した fact がある（facts>0 / recent_facts 非空）→【禁止】自分で探査すること。すべての判断は既存の fact に基づき、今回の成果物は「新しい意図を出す」か「終了する」のみ；ある手がかりを深掘りしたい → 意図を出して worker に調べさせる。自分で curl しない。
   - 序盤でも最大 ≤3 回で切り上げ、初期意図を明確にするためだけに行う；「深く検証している」のであって「素早く方向を決めている」のではないと気づいたら（エンドポイント/ディレクトリを 1 つずつ列挙する、id を 1 つずつ試す、デコードチェーン、同じエンドポイントを繰り返し探る、あらゆるインジェクション/権限昇格/脆弱性のテスト検証——いずれも worker の重い仕事）、即座に手を止めて意図として書く。
   - 既存の事実/状況から判断できるなら、探査はまったく不要。

4. **どの新方向を補うかを決める**：**ここでの「自制」は【既存の意図を重複させない】ことだけを意味し、「出せるだけ少なく出す」ではない**——ゴール未達成のときの既定の問いは「ゴールに近づくために、さらに深く、さらに厳しい、未カバーの打法は何か」であり、「そろそろ締めてよいか」ではない。意図は【オープンな探索方向】（固定の類型/メニューではない）。既知の事実、資産、ゴールを踏まえて方向を自ら判断し、open + running + recent_done と 1 つずつ照合する：
   - 既存の open/running がカバーしている → 生成しない（処理中）。
   - recent_done に現れている → **まずその意図の state（各項目に付く）を見て、どう止まったかを見極めてから決める**：
     · **done（正常に走り切った）**：カバー済み → そのまま再派遣しない；行き止まりかどうかは、その意図が yields した fact の結論で判断し、state では判断しない；【本質的な新機構】（新しい事実/資産/パラメータ/明らかに異なる打法）が現れたときだけ再派遣し、summary に前回との違いを明記する；言い換えや「もう一度試せばいけるかも」は不可、再試行は禁止。
     · **exhausted（予算切れで途中で打ち切られ、部分しか書き戻せなかった）/ blocked（モデルまたはネットワークの失敗で、ほぼ探せていない）**：いずれも途中で終われず情報が不完全——まず get_worker_trace / get_worker_output で実際にどこまでやり、どこで詰まったかを見てから、次のいずれかを選ぶ：突破目前で予算に切られた → 「前回の進捗から続ける」意図を出す；純粋な外部障害で走れなかった（blocked に多い）→ 同じ方向をそのまま再派遣；毎回同じ箇所で詰まる → 打法/方向を変える。根拠は常に trace の実際の進捗であり、state そのものではない。
   - どの意図もカバーしていない全新の方向 → 生成する。
   - 既知の方向がすべて open/running の意図でカバーされている → 生成せず即終了（走っている/待機中の work があり、その進展を待つ）；ただし recent_done しかカバーがなく、open/running がなく、ゴール未達成なら → 冒頭の絶対下限に従い、別の 1 本を開くか続けて出すこと。
   - **深さはカバレッジより優先**：coverage は下限/受け入れ項目であり、探索の目的そのものではない；高価値の入口（RCE/権限昇格/データ流出につながりうる）を見つけたら、カバレッジを平準化するために広げて資産を 1 つずつ浅く試すのではなく、その道を【深く突き抜ける】意図を優先して出す。
   - **路線の多様性を保ち、早すぎる収束を避ける**：ゴール未達成のとき、既存の意図がすべて同じ路線/入口に集中している一方で、【本質的に異なる】未カバー方向（別の入口面/別種の資産/別のエクスプロイトチェーン）があるなら、同じ線上に同義の意図を足すのではなく、その分岐方向を優先して補う（見るのは実質的な差異であり、言葉遣いではない）；その分岐方向が既存の意図でカバーされているなら、やはり生成しない。理想は機構の異なる 2〜3 本の路線が併存すること（例：「アップロード経路から攻める」と「認証バイパスから攻める」）。ある路線が【ゴールへの接近】の証拠を出してから、資源をそこへ集中させる。**ただし多様性は常に冒頭の【操作制約】に服する**：制約で排除された入口面/ポート/ホスト/操作は、本質的に異なっていても決して意図を生成しない。

   **直列エクスプロイトチェーン：段階ごとに出し、並列に分解しない。** 強い依存のある直列チェーン（①→②→③、後のステップが前のステップの実際の産出に依存する）：一度に並列で下ろさない（下流はまだ存在しない前提を得られず、重複/空転するだけ）；TodoWrite でチェーン全体を待ち項目として記録し（各ステップ 1 件）、今回のラウンドでは「前提が満たされた」ステップ（通常は最初のステップ）だけを出し、それが fact を産出してから次のウェイクアップ（プロンプトに待ち項目一覧が付く）で次のステップを出し、満たされたものを completed にする。「同じこと」を 2 本に分けない（「トリガー箇所の確認」と「トリガー箇所の起動」は同じステップ）；多意図の並列は【並行で相互依存しない】次元（無関係な複数エンドポイントの列挙など）にのみ使う。

5. **提出**：篩い出した新方向を【1 回の】add_intent でバッチ提出する（intents 配列、最大 4 個の最高価値のもの。1 件ずつ何度も呼ばない）：
   - **summary**：その方向を 1 文の自然言語で記述する（テスト対象の完全なアドレス + 何をするか + なぜか）。固定の分類に当てはめない；重複排除は主にこれと既存の意図との比較で行う。
   - **asset_ids**：この方向でテスト/攻撃する対象資産の id（できるだけ渡す。0/1/複数、list_assets 由来）——方向が具体的な資産（サイト/エンドポイント/パラメータ/ホスト）を中心とするなら必ず渡す。カバレッジの重複排除と資産チェーンへの連結に使う；複数資産にまたがるならすべて渡す；純粋なグローバル偵察で具体的な資産がない場合のみ空にする。
   - **parent_ids**：この方向がどの上流ノードから統合されたか（任意、0/1/複数）——複数の事実を組み合わせて 1 つの意図を生むならすべて渡し、ある上流の意図/検出結果から派生したならその id も渡す。トップレベルの全新方向は空にする。

重複させず、無理に膨らませない；ただしゴール未達成で、未カバーかつより深い打法があるなら、出すべきときは出す。簡潔に、焦点を絞り、効率的に。`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// ドメインツール + 基本の既定ツールセット（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// 資産カバレッジ機能が無効なら add_task_scope/list_untested_assets を除外する（プロンプトに入れない）。
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// 重要な状況（直前に完了した意図 + プリフェッチした完全なグラフ）は【今回の user 入力】
	// （下記 input）へ移し、system には静的な計画本文だけを残す。move-out により system は毎回
	// 安定しキャッシュ効率が上がる；代償は単一ラウンドが長引くと状況が compaction で圧縮されうる
	// こと（planner の単一ラウンドは通常短く、リスクは低い）。situational は下記 input に連結される。
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// タスクレベルの deadline / 終局モード（ctx 経由で注入、taskclock.go 参照）。終局のラウンドでは
	// タスクタイムアウト時の planner 締めくくり文言を【今回の操作指令】として今回の user 入力に
	// 連結し（situational とともに）、最後のゴール判定だけを行い新たな意図を出さないようにする。
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【タスク終局の締めくくり（今回の特別指令。上記の通常の計画フローを上書きする）】：" + resolveTaskTimeoutWrapup("planner")
	}
	// 本タスクの作業ディレクトリ <workDir>/tasks/<taskID>。先に作成しておく。
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 操作制約（あれば）をシステムプロンプトに注入し、探索境界を画定する
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner 自身には実時間予算がない；deadline がある場合は MaxDuration を残り時間にクランプし、
	// 実行中の計画ラウンドがタスク到達時に締めくくりへ入るようにする（タイムアウト→タスク側の文言、
	// ステップ数→per-run の文言）。
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 記録プロキシ経由で足跡を残す；プロキシ CA を読み込み MITM 再署名の HTTPS 証明書を検証する
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// Web 検索（任意）。ddgs は key 不要；brave-free は BraveKey、tavily は TavilyKey が必要。
		// WebSearchProxy は独立した出口プロキシ（http/https/socks5）で、トラフィック記録用の MITM プロキシとは
		// 無関係；空なら直接接続。
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash サブコマンドは既定でプロキシ経由+CA を信頼
		WorkingDir:            taskDir,                              // 本タスクの作業ディレクトリ <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=無制限；deadline がある場合は deadline までの残り
		Compaction:            compactionConfig(p.compactionWindow()),
		// ウェイクアップをまたいで共有する計画待ち：直列チェーンを複数ラウンド間で保持する（session は新しくても store は同じ）。
		Todos: p.todoFor(ts.ID()),
		// 【今回】のステップ予算に達したら SDK が締めくくりを実行する：今回すでに固まった結論を
		// 着地させる（出すべき add_intent、証明できる prove_goal、直列チェーンの TodoWrite 記録）のであって、
		// 計画を止めるのではない——planner はその後も繰り返しウェイクアップされる。
		// clamped（タスク deadline にクランプされた）場合は PromptByReason に切り替える（wrapupSettlementForTask 参照）。
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // この profile が非ストリーミングを選んだ場合は Provider.Complete を使う
		MaxTokens:    p.maxTokens(),    // 0 = 上限を送らない（サーバー側の既定値に任せる）
	}
	if p.tx != nil { // 生の LLM 会話を永続化；タスクの planner ごとに 1 つの追記ファイル
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 実験機能：有効化すると noa がコンテキスト圧縮を引き継ぐ（アーカイブは <workDir>/noa/<SessionID> 配下に集約、永続）。
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// 状況（直前に完了した意図 + 完全なグラフ）は今回の user 入力に連結する（下記 input）。user には
	// 指令 + ウェイクアップをまたぐ待ち項目も含む（todo はモデル自身の計画メモであり再生成可能なので user でよい）。
	// 冒頭文は「今回固有の変動があるか」で 2 種類に分ける：変動あり → 下の【実際の変動】ブロックを指す；
	// 変動なし（ハートビートの定期巡回 / hint / 復旧など）→ 「グラフが変化した」と偽らず、実行中の意図の
	// ついでの再確認を促す。
	lead := "たった今、具体的な変動があった（下の【今回このラウンドを引き起こした実際の変動】を参照）。これに基づいて次の一歩を計画する："
	if len(triggers) == 0 {
		lead = "今回のラウンドは**定期巡回（ハートビート到達）/具体的な変動シグナルなし**のウェイクアップである——グラフに新しい変動があるとは限らない。ついでに実行中の意図を再確認する：長時間進展がない、または逸脱しているものは steer_work で軌道修正し、方向が完全に誤っているものは kill_work で損切りする；その後ゴールを判定し、方向を補うか決める："
		// ハートビート/変動なしのウェイクアップ時、グラフ全体にもう open または running の意図が
		// 1 つもなければ → 探索は停止している（走っている worker もなく、待機中の方向もない）。
		// planner に明示して今回必ず新しい方向を補わせる。実行中の意図を再確認しただけで空転するな。
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "今回のラウンドは**定期巡回（ハートビート到達）**のウェイクアップであり、かつ現在**open または running の意図が 1 つもない**——走っている worker もなく、待機中の方向もなく、探索は停止している。あなたは今回、ゴールへ前進し、かつグラフ内の既存意図と**互いに重複しない**新しい意図を**必ず** 1 つ以上出すこと（意図 0 個は不可）；まず下の状況に基づいてゴール達成済みかを判定し、未達成なら直ちに方向を補う："
		}
	}
	input := lead + situational + "\n\n上記の状況に基づいてゴールを判定する。ゴールが【真に達成済み】（目標成果を得た/目標の脆弱性を確認済み）なら prove_goal で 1 つずつマークする。**絶対下限：ゴールが未達成で、かつ現在 open または running の意図が 1 つもない（frontier_open=0 かつ running_intents が空）限り、今回のラウンドでゴールへ前進する意図を少なくとも 1 つ出さなければならない——このとき走っている work もなく、待機中の方向もなく、意図 0 個＝タスクの停止である。新たな意図を出さなくてよいのは、既に open/running の意図が進展しているか、ゴールが達成済みの場合のみ。**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration は現在、実時間の到達時に実行中のツールを中断しその場で締めくくりに入る（生きている ctx 上）。
	// 単一ラウンドのハングが締めくくりを迂回することはもうなく、外部のハード ctx による保険も不要。
	// ctx は pause / kill / shutdown のみを担う。
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
