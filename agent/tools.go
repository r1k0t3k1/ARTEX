package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// compactIntents distills intents to {id, summary, state, asset_ids, parents,
// yields} so the planner sees both the direction and its LINEAGE — parents (the
// upstream nodes it derived from: facts/intents/findings) and yields (the facts/
// findings it produced) — without pulling full payloads. parentsOf/yieldsOf are
// built from the exploration edges in graph_overview.
func compactIntents(ns []*db.Node, parentsOf, yieldsOf map[int64][]int64) []map[string]any {
	out := make([]map[string]any, 0, len(ns))
	for _, n := range ns {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		m := map[string]any{"id": n.ID, "summary": p["summary"], "state": n.State}
		if n.Inherited {
			m["source_task_id"] = n.SourceTaskID
			m["inherited"] = true
		}
		// asset_ids is the structured "which assets this direction covers" signal for
		// dedup; fall back to legacy payload keys (target_ids plural, then target_id
		// single) so intents stored before the rename still surface their anchors.
		if tg, ok := p["asset_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_id"]; ok && tg != nil && tg != "" {
			m["asset_ids"] = []any{tg}
		}
		if ps := parentsOf[n.ID]; len(ps) > 0 {
			m["parents"] = ps // 上流：本意図がどのノードから派生したか（複数の事実が共同で 1 つの意図を生むことがある）
		}
		if ys := yieldsOf[n.ID]; len(ys) > 0 {
			m["yields"] = ys // 下流：本意図が生み出した事実/検出結果
		}
		out = append(out, m)
	}
	return out
}

// ToolSet exposes the PG-backed dual graph (asset + exploration) to an LLM agent.
// One ToolSet is created per planner/worker run; per-run signals live here.
type ToolSet struct {
	findingRecorder FindingRecorder
	as              *db.AssetStore   // asset store (optional; nil = asset tools not available)
	cs              *db.CompanyStore // company store (optional)
	ts              *db.ExplorationStore
	worker          string
	taskID          int64 // PG tasks.id; 0 when unknown (tests / orchestrator cross-task reads)
	// coverageDisabled mirrors tasks.coverage_enabled=false. Stored inverted so the
	// zero value (all existing ToolSet constructions) means ENABLED — matching the
	// DB default (true). When true: graphOverviewData drops the coverage block, the
	// auto-scope hook (insertAssets) is skipped, and add_task_scope/list_untested_assets
	// are filtered out of the agent's tool list. The scope field stays regardless.
	coverageDisabled bool
	// ownerNode is the exploration node that writes attach to: assets this run
	// touches get anchored to it as lineage/provenance (NOT visibility — the asset
	// graph is global and shared). Worker = its claimed intent; planner = begin root.
	ownerNode int64
	GoalMet   bool
	Reason    string
	writes    WriteCounts
	// killWork, if set, terminates a running work by intent id (engine callback,
	// wired by the planner). nil = the kill_work tool reports unavailable.
	killWork func(intentID int64) error
	// steerWork, if set, queues a mid-run course-correction for the work running an
	// intent id (engine callback, wired by the planner): the worker injects it before
	// its next tool call and re-plans, without being killed. nil = tool unavailable.
	steerWork func(intentID int64, msg string) error
	// enrich, if set, receives async auto-completion triggers (DNS resolve for a
	// domain, HTTP probe for a site). nil = no engine enrichment.
	enrich EnrichTrigger
	// notify, if set, wakes the task's planner after a graph change that should be
	// re-planned promptly (currently: a new hint). nil = no wake (the hint is still
	// stored and read on the next round triggered by other events). debounced.
	notify func()
	// notifyFinding, if set, wakes the task's planner when this run reports a finding,
	// carrying (intentID, summary) so the round can spell out which intent found what.
	// Wired for workers; nil elsewhere → falls back to notify (bare wake).
	notifyFinding func(intentID int64, summary string)
	// resumeTask, if set, revives the task after a graph change that should make a
	// stopped task run again (currently: set_goals adds a goal). It flips a terminal/
	// paused task back to running and (re)starts the engine loops — a plain notify()
	// can't, because the planner's terminal gate swallows wakes. Wired ONLY for the
	// main agent (human steering); nil for the goals decomposer and workers.
	resumeTask func()
	// notifyGoal, if set, wakes the planner AND records ONE "人が N 個のゴールを追加した：…" trigger
	// for a whole set_goals call (batch-aware — one call, one trigger, not one per goal)
	// so the next round spells out the added goals (instead of the planner having to
	// spot new open goals in the overview). Wired ONLY for the main agent; nil for the
	// goals decomposer (round-0 has no running planner to inform) and workers → those
	// fall back to the bare notify.
	notifyGoal func(texts []string)
	// notifyHint, if set, wakes the planner AND records ONE "人が N 件の戦略ヒントを追加した：…"
	// trigger for a whole add_hint call (batch-aware — one call, one trigger) so the next
	// round is told the round was fired by a new hint and spells the hint out, instead of
	// the planner having to spot it folded into the graph overview. Wired for the main
	// agent + cross-task orchestration; nil elsewhere → falls back to the bare notify.
	notifyHint func(texts []string)
}

// SetNotifyGoal wires the goal-add trigger callback (see ToolSet.notifyGoal). Set only
// by the main-agent chat, so runtime-added goals are announced to the planner by name.
func (t *ToolSet) SetNotifyGoal(fn func([]string)) { t.notifyGoal = fn }

// SetNotifyHint wires the hint-add trigger callback (see ToolSet.notifyHint). Set by
// the main-agent chat and cross-task orchestration, so a runtime-added hint fires a
// planner round announced by name instead of a bare wake.
func (t *ToolSet) SetNotifyHint(fn func([]string)) { t.notifyHint = fn }

// SetResumeTask wires the task-revive callback (see ToolSet.resumeTask). Set only by
// the main-agent chat, so runtime-added goals can pull a finished task back to running.
func (t *ToolSet) SetResumeTask(fn func()) { t.resumeTask = fn }

// SetNotify wires the planner-wake callback (see ToolSet.notify). Set by callers
// that hold the task handle (main-agent chat, cross-task orchestration).
func (t *ToolSet) SetNotify(fn func()) { t.notify = fn }

// SetNotifyFinding wires the finding-wake callback (see ToolSet.notifyFinding).
func (t *ToolSet) SetNotifyFinding(fn func(int64, string)) { t.notifyFinding = fn }

// EnrichTrigger is the enrichment engine seen from the tool layer (see package
// enrich). Kept as an interface here to avoid coupling agent → enrich.
type EnrichTrigger interface {
	ResolveDomain(id int64, host string)
	ProbeSite(id int64, url string)
}

// WriteCounts breaks down what a worker persisted this run, by node kind, so the
// engine can log an accurate "wrote back" summary instead of lumping assets and
// findings under "facts" (record_fact → Facts, insert_assets → Assets,
// report_finding → Findings; each element of a batch counts once).
type WriteCounts struct {
	Facts    int
	Assets   int
	Findings int
}

// Total is every node persisted this run, regardless of kind — the
// "explored but persisted nothing" signal (Total == 0).
func (w WriteCounts) Total() int { return w.Facts + w.Assets + w.Findings }

// String renders the per-kind breakdown for logs, e.g. "事実1 資産25 脆弱性0".
func (w WriteCounts) String() string {
	return fmt.Sprintf("事実%d 資産%d 脆弱性%d", w.Facts, w.Assets, w.Findings)
}

// Writes reports what this run wrote back, split by node kind (so the engine can
// tell "explored but persisted nothing" apart from a completed intent, and log an
// honest breakdown instead of calling assets/findings "facts").
func (t *ToolSet) Writes() WriteCounts { return t.writes }

func NewToolSet(ts *db.ExplorationStore, worker string) *ToolSet {
	return &ToolSet{ts: ts, worker: worker}
}

// SetTaskID sets the PG task id on this ToolSet so that report_finding can
// dual-write to the standalone findings table (which survives task deletion).
func (t *ToolSet) SetTaskID(id int64) { t.taskID = id }

// SetCoverageEnabled records whether this task has the asset-coverage feature on
// (default enabled). Passing false makes graphOverviewData omit the coverage block
// and DropCoverageTools filter the two coverage-only tools out of the agent's tool
// list. It does NOT stop scope accumulation: insertAssets' auto-scope hook runs
// either way, because task_scope is the task's range boundary (the filter basis for
// asset queries), not merely a coverage denominator.
func (t *ToolSet) SetCoverageEnabled(enabled bool) { t.coverageDisabled = !enabled }

// CoverageDisabled reports whether the coverage feature is off for this task.
func (t *ToolSet) CoverageDisabled() bool { return t.coverageDisabled }

// coverageOnlyTools are the LLM tools that only make sense when asset coverage is
// on. When the feature is off they are filtered out of the agent's tool list so
// they neither pollute the prompt nor let the model build a disabled denominator.
// add_task_scope is deliberately NOT here: task_scope is the task's range boundary
// (the filter basis for asset queries), not merely a coverage denominator, so the
// agents that own the scope definition keep it either way — in lockstep with
// insertAssets' auto-scope hook, which also runs regardless of the switch.
var coverageOnlyTools = map[string]bool{"list_untested_assets": true}

// DropCoverageTools returns tools with the coverage-only ones removed when this
// task has the feature disabled; otherwise it returns tools unchanged.
func (t *ToolSet) DropCoverageTools(tools []actool.CoreTool) []actool.CoreTool {
	if !t.coverageDisabled {
		return tools
	}
	out := tools[:0:0]
	for _, tool := range tools {
		if coverageOnlyTools[tool.Name()] {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// Cross-task reuse: exported accessors returning the per-task tool logic bound to
// THIS ToolSet's store. Host-side orchestration tools build a ToolSet for an
// arbitrary task, then Call these — so cross-task reads/hint reuse the exact
// same logic as the in-task tools. (readTool ignores ToolContext, so Call(…,nil)
// is safe; add_hint is a writeTool but also doesn't deref the context here.)
func (t *ToolSet) GraphOverviewTool() actool.CoreTool      { return t.graphOverview() }
func (t *ToolSet) ListFindingsTool() actool.CoreTool       { return t.listFindings() }
func (t *ToolSet) GetWorkerTraceTool() actool.CoreTool     { return t.getWorkerTrace() }
func (t *ToolSet) ListWorkerTracesTool() actool.CoreTool   { return t.listWorkerTraces() }
func (t *ToolSet) SearchWorkerTracesTool() actool.CoreTool { return t.searchAllWorkerTraces() }
func (t *ToolSet) NodeDetailTool() actool.CoreTool         { return t.nodeDetail() }
func (t *ToolSet) AddHintTool() actool.CoreTool            { return t.addHint() }

// SetEnrich wires the async enrichment engine (DNS/HTTP auto-completion).
func (t *ToolSet) SetEnrich(e EnrichTrigger) { t.enrich = e }

// SetOwnerNode sets the exploration node that writes anchor to (worker: its
// intent node; planner/main: the begin root). Assets created/referenced while
// ownerNode is set are anchored to it as lineage (not visibility).
func (t *ToolSet) SetOwnerNode(id int64) { t.ownerNode = id }

// anchorOwner records a lineage edge from this run's owner node to an asset
// (no-op if unset). Provenance only — the asset graph is global and shared, so
// this no longer affects which assets a task can read.
func (t *ToolSet) anchorOwner(assetID int64) {
	if t.ts != nil && t.ownerNode > 0 && assetID > 0 {
		_ = t.ts.Anchor(t.ownerNode, assetID)
	}
}

// pid parses an id that may arrive as a JSON number or string ("" / 0 → 0).
func pid(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		return v
	}
	return 0
}

// pidList parses a list of ids (number|string), dropping zeros/invalids.
func pidList(raw []json.RawMessage) []int64 {
	var out []int64
	for _, r := range raw {
		if v := pid(r); v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func intp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func idp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func readTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func writeTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// readExpTool / writeExpTool build a domain tool whose handler dereferences the
// task-bound ExplorationStore. Two ToolSets carry a nil store: the catalog's
// seed-only shell (never called) and the server-level one behind buildDomainReg,
// which the tools table can bind to ANY agent — including ones that never run
// inside a task (auto/pentest/reporter/custom agent/side questions). Refusing there
// keeps a mis-bound tool a bad tool call; without the guard it was a nil deref,
// and tool handlers run on the harness's own goroutine, so the panic is out of
// reach of every recover() in the server and kills the whole process.
func (t *ToolSet) readExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return readTool(name, desc, schema, t.needExploration(name, run))
}

func (t *ToolSet) writeExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return writeTool(name, desc, schema, t.needExploration(name, run))
}

// needExploration wraps a handler so it only runs with an exploration store.
// Tools that degrade more usefully than "unavailable" (report_finding points at
// add_task_hint, set_goals/set_constraints at the task itself) keep their own
// bespoke guard instead.
func (t *ToolSet) needExploration(name string, run func(context.Context, json.RawMessage) (actool.Result, error)) func(context.Context, json.RawMessage) (actool.Result, error) {
	return func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		if t.ts == nil {
			return actool.Errorf(name + " はタスクコンテキスト（探索グラフ）が必要：現在の agent はどのタスク内でも動作していないため、タスクの探索グラフを取得できず、このツールは利用できない。タスク内で使用するか、task_id 付きのクロスタスク読み取りツール（get_task_node_detail / list_task_findings / get_task_graph など）に切り替えること。"), nil
		}
		return run(ctx, in)
	}
}

func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// --- read tools (planner + worker) ---

func (t *ToolSet) graphOverview() actool.CoreTool {
	return t.readExpTool("graph_overview",
		"(探索チェーングラフ)探索状況の蒸留サマリー：資産数、エンドポイントのないサイト、frontier、検出結果、ヒント(人間/主 agent の戦略ヒント。意図の生成時に必ず取り込む)。計画時はまずこれを呼ぶ。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			return jsonResult(t.graphOverviewData())
		})
}

// graphOverviewData computes the distilled situational snapshot shared by the
// graph_overview tool and the planner's wake-up prompt (which pre-injects it so
// the model needn't spend a turn calling the tool — every plan round starts with
// an empty context and always needs this first).
func (t *ToolSet) graphOverviewData() map[string]any {
	out := map[string]any{}
	// goals summary folded in so the planner needn't call list_goals each round.
	goals, _ := t.ts.ListByKind(db.KindGoal, 100)
	gsum := make([]map[string]any, 0, len(goals))
	for _, g := range goals {
		var p map[string]any
		_ = json.Unmarshal(g.Payload, &p)
		gsum = append(gsum, map[string]any{"id": g.ID, "state": g.State, "text": p["text"]})
	}
	out["goals"] = gsum
	// hints: 人間/主 agent が add_hint でグラフに載せた戦略ヒント；folded in so the
	// planner reads them every round when generating intents (そうしないと書くだけで読まれない).
	hints, _ := t.ts.ListByKind(db.KindHint, 50)
	hsum := make([]map[string]any, 0, len(hints))
	for _, h := range hints {
		var p map[string]any
		_ = json.Unmarshal(h.Payload, &p)
		hint := map[string]any{"id": h.ID, "state": h.State, "text": p["text"]}
		if findingTrafficBindingEnabled() && p["traffic_refs"] != nil {
			hint["traffic_refs"] = p["traffic_refs"]
		}
		hsum = append(hsum, hint)
	}
	out["hints"] = hsum
	// lineage from the exploration edges: an intent's parents (what it
	// derived_from — possibly several facts combined) and its yields (the
	// facts/findings it produced). factFrom maps a fact → the intent that
	// produced it. This is the relationship layer the flat lists lacked.
	edges, _ := t.ts.Edges(5000)
	parentsOf := map[int64][]int64{}
	yieldsOf := map[int64][]int64{}
	factFrom := map[int64]int64{}
	for _, e := range edges {
		switch e.Rel {
		case db.RelDerivedFrom, db.RelSpawns: // upstream: derived_from (fact/finding/intent→intent) or spawns (origin fact→goal, legacy begin→intent)
			parentsOf[e.To] = append(parentsOf[e.To], e.From)
		case db.RelYields: // intent --yields--> fact/finding
			yieldsOf[e.From] = append(yieldsOf[e.From], e.To)
			factFrom[e.To] = e.From
		}
	}
	// cold-digest §6: members folded into an active digest are shown via cold_digests
	// (below), not the flat recent_* lists. `covered` maps member id → its digest id.
	// §6 render-time revival check: a covered member that has become hot again (a new
	// intent derived from it) must reappear this round — so `hidden` folds a member out
	// only when it is covered AND still cold.
	covered, _ := t.ts.CoveredMembers()
	// Render-time hot set (ancestor of a live intent / fact under a live intent).
	// §6 revival check: a covered member that revived (now hot) must NOT stay folded
	// — hidden() only folds a member out when it is covered AND still cold. Computed
	// every round (cheap for real graph sizes); nil map degrades safely.
	var hotAtRender map[int64]bool
	if cg, _, err := loadColdGraph(t.ts); err == nil {
		hotAtRender = cg.hotSet()
	}
	hidden := func(id int64) bool { _, c := covered[id]; return c && !hotAtRender[id] }
	const openIntentsCap = 30
	fr, _ := t.ts.Frontier(openIntentsCap) // priority DESC, id ASC —— 優先度の高い先頭 N 件；実際の総数は frontier_open を参照
	out["open_intents"] = compactIntents(fr, parentsOf, yieldsOf)
	all, _ := t.ts.ListByKind(db.KindIntent, 300)
	var running, recentDone []*db.Node
	for _, n := range all {
		switch n.State {
		case "running":
			running = append(running, n)
		case "done", "blocked", "exhausted":
			if hidden(n.ID) {
				continue // in a cold_digest and still cold — shown via cold_digests (§6.2)
			}
			recentDone = append(recentDone, n) // 新しい順（all は id 降順）；折り畳み済みは除外し、出力時に最新 N 件へ切り詰める
		}
	}
	out["running_intents"] = compactIntents(running, parentsOf, yieldsOf)
	// done_intents_total：終了済み意図（done/blocked/exhausted）の総数。recent_done_intents と
	// 並行に命名——後者はその最新ウィンドウを切り詰めたビューにすぎない。2 つのキーを並べるだけで自己記述的になる：「見えているのは N/総数」。
	// planner が重複排除するとき「表示されていない」を「まだ出していない」と誤解しないようにし、プロンプト内で別途説明する必要をなくす。
	if dt, err := t.ts.CountFinishedIntents(); err == nil {
		out["done_intents_total"] = dt
	}
	// frontier_open：開放意図の実総数（open_intents はそのうち優先度の高い先頭 N 件を切り詰めたビューにすぎない）。
	if fo, err := t.ts.CountOpenIntents(); err == nil {
		out["frontier_open"] = fo
	} else {
		out["frontier_open"] = len(fr)
	}
	// findings (confirmed vulns) and facts (worker exploration results) are
	// now distinct node kinds. recent_facts surfaces fact summaries (esp.
	// negative results) so the planner sees them in one call; full content
	// via node_detail(id).
	vulnNodes, _ := t.ts.ListByKind(db.KindFinding, 1000)
	factNodes, _ := t.ts.ListByKind(db.KindFact, 1000) // newest first
	out["findings_total"] = len(vulnNodes)             // 確認済み脆弱性の総数（ゴール判定はこれを見る）；明細は finding_list（最新ウィンドウ）
	out["facts"] = len(factNodes)                      // 探索事実/結論の数（否定結論を含む）
	// findings はタスクで最も価値の高い産物 → 概観には最新ウィンドウを載せる（≤10 件、vulnNodes は id 降順なので最新が先頭）。
	// planner が毎ラウンドのゴール判定で最近確認された脆弱性を一目で見られるようにする；全量/より古いものは list_findings で取得。
	// 各項目は {id, summary, from_intent?} のみ：from_intent は本脆弱性を生んだ意図。
	// evidence/assets/vulnclass/severity/state などは引き続き list_findings / node_detail(id) で取得できる。
	const findingListCap = 10
	findingList := make([]map[string]any, 0, findingListCap)
	for _, n := range vulnNodes {
		if len(findingList) >= findingListCap {
			break
		}
		var fp map[string]any
		_ = json.Unmarshal(n.Payload, &fp)
		m := map[string]any{"id": n.ID, "summary": fp["summary"]}
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // この脆弱性を生んだ意図
		}
		findingList = append(findingList, m)
	}
	out["finding_list"] = findingList
	// recent_facts：折り畳まれていない事実のうち最新ウィンドウ（≤N、factNodes は id 降順なので最新が先頭）。既に
	// digest へ折り畳まれ、まだ冷たいもの（hidden）は cold_digests を参照し、ここでは重複させない。各項目は {id, summary, from_intent?,
	// confidence?}；evidence などの詳細は node_detail(id)。より古いものは list_facts で遡る。
	const recentFactsCap = 20
	recentFacts := make([]map[string]any, 0, recentFactsCap)
	for _, n := range factNodes {
		if len(recentFacts) >= recentFactsCap {
			break
		}
		if hidden(n.ID) {
			continue // 既に digest へ折り畳まれ、まだ冷たい —— cold_digests を参照
		}
		m := compactNode(n)
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // この事実を生んだ意図
		}
		// confidence を概観に載せる：planner がどの結論が inferred にすぎないかを一目で分かるようにする（特に否定結論を
		// 確定事実として扱わない）；evidence は長いので node_detail(id) に委ねる。
		var fp map[string]any
		if json.Unmarshal(n.Payload, &fp) == nil {
			if c, ok := fp["confidence"].(string); ok && c != "" {
				m["confidence"] = c
			}
		}
		recentFacts = append(recentFacts, m)
	}
	out["recent_facts"] = recentFacts
	// recent_done_intents：折り畳まれていない終了済み意図のうち最新ウィンドウ（≤N、recentDone は id 降順）。
	// より古いものは done_intents_total の件数 + node_detail(id) を参照。
	const recentDoneCap = 12
	if len(recentDone) > recentDoneCap {
		recentDone = recentDone[:recentDoneCap]
	}
	out["recent_done_intents"] = compactIntents(recentDone, parentsOf, yieldsOf)
	// cold-digest §6.1: 折り畳まれた冷領域の digest body を、最新メンバーの時刻の降順で先頭 N 件取得；切り詰められたより古い digest
	// は裸の id のみ渡す（expand_digest で展開可能）。冷領域の唯一の出口が無限に長くならないようにする。
	const coldDigestsCap = 15
	if cds, more := coldDigestsRecent(t.ts, coldDigestsCap); len(cds) > 0 {
		out["cold_digests"] = cds // [{id, body, member_count}] —— body を直接読む (§6.1)
		if len(more) > 0 {
			out["cold_digests_more"] = more // 切り詰められたより古い digest の id；expand_digest(id) で展開
		}
	}
	// the original task (root) so the planner always has it, not just the
	// decomposed goals.
	if description, goal, err := t.ts.Root(); err == nil {
		out["task"] = map[string]any{"description": description, "goal": goal}
	}
	// Direct source tasks are a live, read-only blackboard view. Keep their
	// summaries in a separate field so their intents never enter this task's
	// frontier or get mistaken for locally claimable work.
	out["related_tasks"] = t.relatedTaskOverviews()
	// coverage：粗い資産テストカバレッジの参考値——範囲(task_scope)内の資産のうち、fact に触れた
	// 割合 + by_type(種類別の 総数/テスト済み)。未テストの具体的な資産を見るかは agent が list_untested_assets を必要に応じて呼び判断する。タスクコンテキストのみ。
	// 資産カバレッジ機能が無効時(coverageDisabled)：host_count(対象ホスト数の把握情報)だけを残し、
	// denominator/tested/pct/by_type/note などのカバレッジ指標を捨て、コンテキストを汚さず、隠された
	// add_task_scope/list_untested_assets を誘導しないようにする。
	if t.as != nil && t.ts != nil && t.taskID > 0 {
		{
			m := map[string]any{}
			if !t.coverageDisabled {
				if cov, err := t.as.TaskCoverageWithSources(t.taskID); err == nil {
					m["denominator"] = cov.Denominator
					m["tested"] = cov.Tested
					m["by_type"] = cov.ByType
					m["note"] = "coverage資産テストカバレッジ（エンドポイントなど各種関連資産を含む）。粗い推定で参考値にすぎない：現在のタスクと直接関連タスクの scope、事実アンカーを含む；関連 scope は読み取り専用。コンテナ型資産/大量の列挙では低く出るので、これでテスト完了と判断しないこと；add_task_scope で本タスクの範囲を補い、list_untested_assets で未テスト資産を確認できる【通常は list_untested_assets を呼ばず、タスクを進めるだけでよい】；"
					if cov.Denominator == 0 {
						m["pct"] = nil
						m["status"] = "範囲が未アンカー"
					} else {
						m["pct"] = cov.Pct
					}
				}
			}
			if hosts, err := t.as.HostsByTaskWithSources(t.taskID); err == nil {
				// ホスト総数のみを渡し、host 一覧を graph_overview に並べない（大規模タスクでは毎ラウンド
				// 大量の文字列を重複して運ぶことになり、計画判断への価値は限定的）；具体的なホストは必要に応じて list_assets で調べる。
				m["host_count"] = len(hosts)
			}
			if len(m) > 0 {
				out["coverage"] = m
			}
		}
	}
	return out
}

func inheritedMap(m map[string]any, sourceTaskID int64) map[string]any {
	m["source_task_id"] = sourceTaskID
	m["inherited"] = true
	return m
}

const (
	relatedOverviewTotalTextRunes      = 48_000
	relatedOverviewMaxTextPerSource    = 8_000
	relatedOverviewMaxGoalsPerSource   = 8
	relatedOverviewMaxHintsPerSource   = 6
	relatedOverviewMaxFactsPerSource   = 12
	relatedOverviewMaxFindingsPerTask  = 6
	relatedOverviewMaxIntentsPerTask   = 8
	relatedOverviewMaxScopePerSource   = 12
	relatedOverviewMaxDigestsPerSource = 6
)

// overviewTextBudget bounds inherited prompt text while preserving a fair slice
// for every direct source. Full evidence remains available through the on-demand
// read tools, so truncation here does not discard persisted blackboard data.
type overviewTextBudget struct {
	remaining int
	truncated bool
}

func relatedOverviewBudgetForSources(sourceCount int) int {
	if sourceCount <= 0 {
		return 0
	}
	if sourceCount > db.MaxTaskSourceCount {
		sourceCount = db.MaxTaskSourceCount
	}
	perSource := relatedOverviewTotalTextRunes / sourceCount
	if perSource > relatedOverviewMaxTextPerSource {
		perSource = relatedOverviewMaxTextPerSource
	}
	return perSource
}

func (b *overviewTextBudget) take(value any, fieldLimit int) string {
	var text string
	switch value := value.(type) {
	case string:
		text = strings.TrimSpace(value)
	case nil:
		return ""
	default:
		text = strings.TrimSpace(fmt.Sprint(value))
	}
	if text == "" {
		return ""
	}
	if b.remaining <= 0 || fieldLimit <= 0 {
		b.truncated = true
		return ""
	}
	runes := []rune(text)
	limit := fieldLimit
	if limit > b.remaining {
		limit = b.remaining
	}
	if len(runes) > limit {
		b.truncated = true
		if limit == 1 {
			text = "…"
		} else {
			text = string(runes[:limit-1]) + "…"
		}
		runes = []rune(text)
	}
	b.remaining -= len(runes)
	return text
}

func recentTerminalIntents(store *db.ExplorationStore, limit int) []*db.Node {
	if limit <= 0 {
		return []*db.Node{}
	}
	const batch = 300
	cursor := int64(0)
	out := make([]*db.Node, 0, limit)
	for len(out) < limit {
		page, more, err := store.ListByKindPage(db.KindIntent, cursor, batch)
		if err != nil || len(page) == 0 {
			break
		}
		for _, intent := range page {
			switch intent.State {
			case "done", "blocked", "exhausted", "stopped":
				out = append(out, intent)
			}
			if len(out) >= limit {
				break
			}
		}
		if !more {
			break
		}
		cursor = page[len(page)-1].ID
	}
	return out
}

// relatedTaskOverviews distills persistent blackboard state from direct source
// tasks. It intentionally reads each source's local store methods, never its own
// related sources, so inheritance is one level only.
func (t *ToolSet) relatedTaskOverviews() []map[string]any {
	sources, err := t.ts.DirectSourceStores()
	if err != nil {
		return []map[string]any{}
	}
	if len(sources) > db.MaxTaskSourceCount {
		sources = sources[:db.MaxTaskSourceCount]
	}
	perSourceTextBudget := relatedOverviewBudgetForSources(len(sources))
	out := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		ts := source.Store
		// §2 cross-task: render the source task's OWN folded view — fold out the
		// members it has already folded, and surface its cold_digests read-only.
		hidden := hiddenMembersFor(ts)
		budget := overviewTextBudget{remaining: perSourceTextBudget}
		item := map[string]any{
			"source_task_id": source.Task.TaskID,
			"inherited":      true,
			"task": map[string]any{
				"description": budget.take(source.Task.Description, 800),
				"goal":        budget.take(source.Task.Goal, 800),
				"status":      source.Task.Status,
			},
		}
		stats, statsErr := ts.Stats()

		edges, _ := ts.Edges(5000)
		parentsOf := map[int64][]int64{}
		yieldsOf := map[int64][]int64{}
		factFrom := map[int64]int64{}
		for _, edge := range edges {
			switch edge.Rel {
			case db.RelDerivedFrom, db.RelSpawns:
				parentsOf[edge.To] = append(parentsOf[edge.To], edge.From)
			case db.RelYields:
				yieldsOf[edge.From] = append(yieldsOf[edge.From], edge.To)
				factFrom[edge.To] = edge.From
			}
		}

		goals, _ := ts.ListByKind(db.KindGoal, relatedOverviewMaxGoalsPerSource)
		goalSummary := make([]map[string]any, 0, len(goals))
		for _, goal := range goals {
			var payload map[string]any
			_ = json.Unmarshal(goal.Payload, &payload)
			goalSummary = append(goalSummary, inheritedMap(map[string]any{
				"id": goal.ID, "state": goal.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["goals"] = goalSummary

		hints, _ := ts.ListByKind(db.KindHint, relatedOverviewMaxHintsPerSource)
		hintSummary := make([]map[string]any, 0, len(hints))
		for _, hint := range hints {
			var payload map[string]any
			_ = json.Unmarshal(hint.Payload, &payload)
			hintSummary = append(hintSummary, inheritedMap(map[string]any{
				"id": hint.ID, "state": hint.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["hints"] = hintSummary

		facts, _ := ts.ListByKind(db.KindFact, relatedOverviewMaxFactsPerSource)
		findings, _ := ts.ListByKind(db.KindFinding, relatedOverviewMaxFindingsPerTask)
		intentNodes, _ := ts.ListByKind(db.KindIntent, 300)
		terminalIntent := make(map[int64]bool, len(intentNodes))
		for _, intent := range intentNodes {
			terminalIntent[intent.ID] = inheritedIntentSummaryState(intent.State)
		}
		item["facts"] = len(facts)
		item["findings"] = len(findings)
		if statsErr == nil {
			item["facts"] = stats[db.KindFact]
			item["findings"] = stats[db.KindFinding]
			if stats[db.KindGoal] > len(goals) || stats[db.KindHint] > len(hints) ||
				stats[db.KindFact] > len(facts) || stats[db.KindFinding] > len(findings) {
				budget.truncated = true
			}
		}
		recentFindings := make([]map[string]any, 0, len(findings))
		for _, finding := range findings {
			entry := inheritedMap(compactFinding(finding), source.Task.TaskID)
			entry["summary"] = budget.take(entry["summary"], 400)
			recentFindings = append(recentFindings, entry)
		}
		item["recent_findings"] = recentFindings
		recentFacts := make([]map[string]any, 0, len(facts))
		for _, fact := range facts {
			if hidden(fact.ID) {
				continue // folded into this source's cold_digests — shown there (§2/§6.2)
			}
			m := inheritedMap(compactNode(fact), source.Task.TaskID)
			m["summary"] = budget.take(m["summary"], 400)
			if from := factFrom[fact.ID]; from > 0 && terminalIntent[from] {
				m["from_intent"] = from
			}
			var payload map[string]any
			if json.Unmarshal(fact.Payload, &payload) == nil {
				if confidence, ok := payload["confidence"].(string); ok && confidence != "" {
					m["confidence"] = confidence
				}
			}
			recentFacts = append(recentFacts, m)
		}
		item["recent_facts"] = recentFacts

		recentDoneRaw := recentTerminalIntents(ts, relatedOverviewMaxIntentsPerTask)
		recentDone := recentDoneRaw[:0] // in-place filter: drop this source's folded intents (§2)
		for _, intent := range recentDoneRaw {
			if hidden(intent.ID) {
				continue
			}
			recentDone = append(recentDone, intent)
		}
		for _, intent := range recentDone {
			intent.Inherited = true
			intent.SourceTaskID = source.Task.TaskID
		}
		intentResults := compactIntents(recentDone, parentsOf, yieldsOf)
		for i, intent := range recentDone {
			intentResults[i]["summary"] = budget.take(intentResults[i]["summary"], 400)
			acts, _, err := ts.ActivityPageForTerminalIntent(intent.ID, 0, 20)
			if err != nil {
				continue
			}
			var resultSummary, textFallback string
			for _, activity := range acts {
				switch activity.Kind {
				case "result":
					resultSummary = activity.Summary
				case "text":
					textFallback = activity.Summary
				}
			}
			if resultSummary == "" {
				resultSummary = textFallback
			}
			if resultSummary != "" {
				intentResults[i]["result_summary"] = budget.take(resultSummary, 800)
			}
		}
		item["recent_intent_results"] = intentResults
		// §2 cross-task: the source task's folded cold region, read-only, newest-member
		// first & capped like the current task's. Members (and overflow digests) are
		// resolvable via expand_digest(id)/node_detail(id), which search source tasks.
		if cds, more := coldDigestsRecent(ts, relatedOverviewMaxDigestsPerSource); len(cds) > 0 {
			for _, cd := range cds {
				cd["inherited"] = true
				cd["source_task_id"] = source.Task.TaskID
			}
			item["cold_digests"] = cds
			if len(more) > 0 {
				item["cold_digests_more"] = more // 切り詰められたより古い digest の id；expand_digest(id) で展開
			}
		}
		if statsErr == nil {
			item["node_stats"] = stats
		}

		if t.as != nil {
			if scopeRows, err := t.as.ListTaskScope(source.Task.TaskID); err == nil && len(scopeRows) > 0 {
				scopeCount := len(scopeRows)
				if len(scopeRows) > relatedOverviewMaxScopePerSource {
					scopeRows = scopeRows[:relatedOverviewMaxScopePerSource]
					budget.truncated = true
				}
				scope := make([]map[string]any, 0, len(scopeRows))
				for _, row := range scopeRows {
					entry := map[string]any{"kind": row.Kind, "source": budget.take(row.Source, 300)}
					switch {
					case row.Domain != "":
						entry["value"] = budget.take(row.Domain, 400)
					case row.Net != "":
						entry["value"] = budget.take(row.Net, 400)
					case row.Value != "":
						entry["value"] = budget.take(row.Value, 400)
					case row.CompanyID != nil:
						entry["company_id"] = *row.CompanyID
					}
					scope = append(scope, entry)
				}
				item["asset_scope"] = scope
				item["asset_scope_count"] = scopeCount
			}
			if coverage, err := t.as.TaskCoverage(source.Task.TaskID, source.Task.ExplorationID); err == nil {
				item["asset_coverage"] = map[string]any{
					"denominator": coverage.Denominator,
					"tested":      coverage.Tested,
					"pct":         coverage.Pct,
					"by_type":     coverage.ByType,
				}
			}
		}
		if budget.truncated {
			item["summary_truncated"] = true
		}
		out = append(out, item)
	}
	return out
}

func inheritedIntentSummaryState(state string) bool {
	switch state {
	case "done", "blocked", "exhausted", "stopped":
		return true
	default:
		return false
	}
}

// compactNode distills any exploration node to id + summary + state, dropping the
// big detail/evidence (fetch that on demand via node_detail).
func compactNode(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	return m
}

// compactFinding is compactNode plus the vuln-specific vulnclass/severity.
func compactFinding(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	if vc, ok := p["vulnclass"]; ok && vc != nil && vc != "" {
		m["vulnclass"] = vc
	}
	if sv, ok := p["severity"]; ok && sv != nil && sv != "" {
		m["severity"] = sv
	}
	return m
}

func (t *ToolSet) listFindings() actool.CoreTool {
	return t.readExpTool("list_findings", "本タスクおよび直接関連タスクの【確認済み脆弱性】を一覧する(コンパクト：id+task_id+intent_id+vulnclass+severity+要約+状態)。関連タスクの項目は source_task_id/inherited=true を持ち読み取り専用。ここには脆弱性のみ。通常の探索事実は list_facts、詳細は node_detail(id)。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			f, _ := t.ts.ListByKindWithSources(db.KindFinding, 500)
			if err := t.ts.PopulateFindingTrafficIDs(f); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			intentOf, _ := t.ts.FindingIntentsWithSources() // finding id -> それを生んだ intent id
			taskID := t.taskID
			if taskID <= 0 {
				taskID, _ = t.ts.TaskID()
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				m := compactFinding(n)
				if n.FindingID > 0 {
					m["finding_id"], m["finding_node_id"], m["traffic_count"] = n.FindingID, n.ID, n.TrafficCount
				}
				if n.Inherited {
					m["task_id"] = n.SourceTaskID
				} else {
					m["task_id"] = taskID
				}
				if iid, ok := intentOf[n.ID]; ok {
					m["intent_id"] = iid
				}
				out = append(out, m)
			}
			return jsonResult(out)
		})
}

// factsPageSize is the default page size for list_facts. Facts pile up on long
// tasks; returning all of them at once (the old behaviour) could blow up the
// context, so default to the newest page and let the agent page/filter for more.
const factsPageSize = 20

func (t *ToolSet) listFacts() actool.CoreTool {
	return t.readExpTool("list_facts", "本タスクおよび直接関連タスクの【探索事実/結論】をページングで列挙する。新しい順(コンパクト：id+要約+状態。要約が長い場合は切り詰め、全文は node_detail(id))。パラメータはすべて任意：limit(既定 20、上限 100)、before(カーソル。前のページが返した next_before を渡すとより古いページを取得；省略/0=最新ページ)、q(要約キーワードで絞り込み)。戻り値 {facts, total, has_more, next_before}：total は絞り込み後の総数、has_more=true なら next_before でページを続ける。関連タスクの項目は source_task_id/inherited=true を持ち読み取り専用。脆弱性は list_findings を参照。",
		obj(map[string]any{
			"limit":  intp("返却件数。既定 20、上限 100"),
			"before": intp("ページングカーソル：id がこの値より小さい、より古い事実のみ返す；省略または 0 = 最新ページ"),
			"q":      str("事実の要約キーワードで絞り込み（大文字小文字を区別しない）；省略 = 絞り込みなし"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Limit  int    `json:"limit"`
				Before int64  `json:"before"`
				Q      string `json:"q"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = factsPageSize
			}
			if limit > 100 {
				limit = 100
			}
			f, hasMore, total, err := t.ts.ListByKindPageWithSources(db.KindFact, a.Before, limit, strings.TrimSpace(a.Q))
			if err != nil {
				return actool.Result{}, err
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				out = append(out, compactFact(n))
			}
			res := map[string]any{"facts": out, "total": total, "has_more": hasMore}
			if hasMore && len(f) > 0 {
				res["next_before"] = f[len(f)-1].ID // これを渡して次のページ(より古い)を取得
			}
			return jsonResult(res)
		})
}

// factSummaryMax caps a fact summary in list_facts output. Facts carry one-line
// conclusions, but nothing enforces brevity; a runaway summary must not bloat a
// whole page. Full text stays available via node_detail(id).
const factSummaryMax = 160

// compactFact is compactNode with the summary rune-capped for list_facts, so a
// page of facts stays bounded regardless of how long any single summary grew.
func compactFact(n *db.Node) map[string]any {
	m := compactNode(n)
	if s, ok := m["summary"].(string); ok && len([]rune(s)) > factSummaryMax {
		m["summary"] = string([]rune(s)[:factSummaryMax]) + "…"
		m["summary_truncated"] = true
	}
	return m
}

func (t *ToolSet) nodeDetail() actool.CoreTool {
	return t.readExpTool("node_detail", "id で本タスクまたは直接関連タスクの【探索グラフノード】の完全な内容を取得する。継承ノードは source_task_id/inherited=true を持ち読み取り専用。list_facts/list_findings/graph_overview が返した探索ノード id に限る；資産は list_assets/asset_neighbors を使う。",
		obj(map[string]any{"id": idp("探索グラフノード id（資産 id ではない）")}, "id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.ID)
			if id <= 0 {
				return actool.Errorf("id は必須"), nil
			}
			n, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == nil {
				return actool.Errorf(fmt.Sprintf("探索ノード %d が見つからない。資産を調べたい場合は list_assets / asset_neighbors を使うこと（資産と探索ノードは別の id 空間であり、資産 id を node_detail に渡すことはできない）。", id)), nil
			}
			if err := t.ts.PopulateFindingTrafficIDs([]*db.Node{n}); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(n) // full payload incl. detail / evidence, plus explicit finding IDs
		})
}

// --- planner write tools ---

// intentItem は add_intent のバッチ/単条 1 件分の探索方向。
type intentItem struct {
	Summary   string            `json:"summary"`
	AssetIDs  []json.RawMessage `json:"asset_ids"`
	ParentIDs []json.RawMessage `json:"parent_ids"`
	Priority  int               `json:"priority"`
}

// addOneIntent は意図ノードを 1 件作成し、上流の血縁を結んで id を返す。
// 制約：意図は確認済み知識にのみアンカーできる——各 parent_id は既存の fact/finding
// ノードでなければならない（他の意図/ゴール/ヒントには付けられない）。トップレベルの全新方向は parent_ids を空にし、フォールバックとして origin fact に接続する。
// これにより「各意図が必ず fact ノードへ接続し、発見駆動であって空想の計画ではない」ことが作成経路で強制される。
func (t *ToolSet) addOneIntent(it intentItem) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary は必須")
	}
	// まずアンカーを検証する（ノード作成前に、壊れたアンカーが孤児意図を残さないように）。
	parents := pidList(it.ParentIDs)
	for _, pidv := range parents {
		n, err := t.ts.GetNodeWithSources(pidv)
		if err != nil || n == nil {
			return 0, fmt.Errorf("parent_id %d は本タスクまたは直接関連タスクに存在しない：parent_ids は既存の【事実(fact)/検出結果(finding)】ノード id でなければならない；トップレベルの全新方向は parent_ids を空にすること", pidv)
		}
		if n.Kind != db.KindFact && n.Kind != db.KindFinding {
			return 0, fmt.Errorf("parent_id %d は %q ノードであり、意図のアンカーにはできない：意図は確認済みの【事実(fact)/検出結果(finding)】にのみアンカーでき、意図/ゴール/ヒントには付けられない；トップレベルの全新方向は parent_ids を空にすること", pidv, n.Kind)
		}
	}
	priority := it.Priority
	if priority == 0 {
		priority = 5
	}
	anchors := pidList(it.AssetIDs)
	// 資産インターセプト：意図に紐付く資産がシステムの資産インターセプト規則に該当する場合、その意図の投入を禁止する。
	if t.as != nil && len(anchors) > 0 {
		hits, err := t.as.CheckAssetsIntercept(t.taskID, anchors)
		if err != nil {
			return 0, fmt.Errorf("資産インターセプトの検証に失敗：%w", err)
		}
		if len(hits) > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "意図「%s」に紐付く資産がテスト範囲の検証に通りませんでした。関連資産へのテストを停止してください：", it.Summary)
			for _, h := range hits {
				fmt.Fprintf(&b, "\n - %s", h.Describe())
			}
			return 0, fmt.Errorf("%s", b.String())
		}
	}
	payload := map[string]any{"summary": it.Summary}
	if len(anchors) > 0 {
		payload["asset_ids"] = anchors
	}
	id, err := t.ts.AddIntent(payload, priority, anchors, "planner")
	if err != nil {
		return 0, err
	}
	// upstream lineage: link each (validated) fact/finding parent → this intent, so
	// "multiple facts combine into one new intent" is expressible.
	for _, parent := range parents {
		_ = t.ts.Link(parent, db.RelDerivedFrom, id)
	}
	// a top-level intent (no explicit parent) connects to the origin fact, so every
	// intent still traces back to a fact node — at task start the only fact is the
	// origin, and the first intents derive from it.
	if len(parents) == 0 {
		if origin, _ := t.ts.OriginFactID(); origin > 0 {
			_ = t.ts.Link(origin, db.RelDerivedFrom, id)
		}
	}
	return id, nil
}

func (t *ToolSet) addIntent() actool.CoreTool {
	return t.writeExpTool("add_intent", "【探索方向】を生成して frontier に書き込み、探索チェーンに接続する。意図はオープンな探索方向であり固定の類型ではない——summary で「何を探索/検証/エクスプロイトするか」を 1 文で自由に記述する。\n"+
		"★バッチ優先：1 ラウンドで絞り出した複数の新方向は intents 配列に入れて 1 回で投入する（1 件ずつ呼ぶより往復を節約できる）。戻り値は ids 配列で、intents と同じ長さ・同じ順序（失敗項目は id=0、詳細は errors）。単条なら intents を省略し、トップレベルの summary を直接渡す。",
		obj(map[string]any{
			"intents":    map[string]any{"type": "array", "description": "【まずこれを使う】追加する探索方向の配列。順番に処理する。各要素のフィールドは下記のトップレベルと同じ（summary/asset_ids/parent_ids/priority）。戻り値 ids は本配列と同じ長さ・同じ順序。", "items": map[string]any{"type": "object"}},
			"summary":    str("[単条] この探索方向を 1 文で記述する：何をするか+なぜか。方向が明確ならよく、資産 id に依存しない。"),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "本方向でテスト/攻撃する【対象資産 id】（**できるだけ渡す**。0/1/複数可；list_assets が返す資産 id であり探索ノード id ではない）：この探索方向がどの資産（サイト/エンドポイント/パラメータ/ホストなど）を対象とするか。方向が特定の具体的資産を中心とするなら必ず渡すこと——「この探索がどのターゲットを打つか」の構造化マーカーであり、カバレッジの重複排除と意図の資産チェーンへの接続に使う。純粋なグローバル偵察で、具体的な対象資産が本当にない場合のみ空にする。"},
			"parent_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "上流アンカー id（任意、0/1/複数）：本方向がどの【確認済みの事実(fact)/検出結果(finding)】から総合されたか。**既存の fact/finding ノード id のみを入れ、意図/ゴール/ヒントは入れない**——意図は確認済み知識にアンカーする必要があり、発見駆動であって空想の計画ではない。複数の事実が共同で 1 つの新意図を生むなら複数渡す；トップレベルの全新偵察方向は空にする（自動でタスク起点の origin fact に接続される）。"},
			"priority":   intp("優先度 0-10、既定 5"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Intents    []intentItem `json:"intents"`
				intentItem              // 単条モード：トップレベルの summary/asset_ids/parent_ids/priority
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Intents) > 0
			items := a.Intents
			if !batch {
				items = []intentItem{a.intentItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			createdAny := false
			for i, it := range items {
				id, err := t.addOneIntent(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				createdAny = true
			}

			// 人が主 agent 経由で意図を直接投入 → タスクが done 済み（open ゴールのない goalless 分岐）なら
			// running に戻し、ワーカーがこの意図を引き取って実行できるようにする。resumeTask は主 agent の Chat だけが接続する
			// (SetResumeTask)；planner の ToolSet は nil なので、planner 自身が add_intent を呼ぶときこの部分は
			// no-op となり、通常の意図生成に影響しない。意図ノードは上で作成済み(open)なので、復活時に誤って枯渇判定されることはない。
			if createdAny && t.resumeTask != nil {
				t.resumeTask()
			}

			if !batch { // 単条：元の戻り値を保持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("intent created: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) listGoals() actool.CoreTool {
	return t.readExpTool("list_goals", "本タスクのゴールノードとその状態（open/met）を列挙し、達成判定に使う。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			g, _ := t.ts.ListByKind(db.KindGoal, 100)
			return jsonResult(g)
		})
}

func (t *ToolSet) proveGoal() actool.CoreTool {
	return t.writeExpTool("prove_goal", "ある検出結果/事実がゴール達成を証明すると判断したときに呼ぶ：証拠ノードをゴールノードに接続し、ゴールを met にする。",
		obj(map[string]any{
			"goal_id":     idp("ゴールノード id"),
			"evidence_id": idp("それを証明する検出結果/事実ノード id"),
			"reason":      str("この証拠がなぜそのゴールを満たすか"),
		}, "goal_id", "evidence_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				GoalID     json.RawMessage `json:"goal_id"`
				EvidenceID json.RawMessage `json:"evidence_id"`
				Reason     string          `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			goal, ev := pid(a.GoalID), pid(a.EvidenceID)
			if goal == 0 || ev == 0 {
				return actool.Errorf("goal_id と evidence_id は必須"), nil
			}
			goalNode, err := t.ts.GetNode(goal)
			if err != nil || goalNode == nil || goalNode.Kind != db.KindGoal {
				return actool.Errorf("goal_id は本タスクのゴールノードでなければならない（関連タスクのゴールは読み取り専用）"), nil
			}
			evidenceNode, err := t.ts.GetNodeWithSources(ev)
			if err != nil || evidenceNode == nil || (evidenceNode.Kind != db.KindFact && evidenceNode.Kind != db.KindFinding) {
				return actool.Errorf("evidence_id は本タスクまたは直接関連タスクの事実/脆弱性ノードでなければならない"), nil
			}
			_ = t.ts.Link(ev, db.RelProves, goal)
			_ = t.ts.SetNodeState(goal, "met")
			// ゴールを 1 つ met にするたびに、本タスクの【すべてのゴール】が met か確認する；そうなら
			// タスク完了を自動判定（GoalMet を設定）し、モデルが明示的に goal_met を呼ぶ必要をなくす。
			if goals, err := t.ts.ListByKind(db.KindGoal, 1000); err == nil && len(goals) > 0 {
				allMet := true
				for _, g := range goals {
					if g.State != "met" {
						allMet = false
						break
					}
				}
				if allMet {
					t.GoalMet = true
					t.Reason = fmt.Sprintf("すべての %d 個のゴールが met になった（最後に goal %d がトリガー）", len(goals), goal)
					return actool.Text(fmt.Sprintf("goal %d marked met；本タスクのすべてのゴールが達成され、タスクは自動的に完了判定された", goal)), nil
				}
			}
			return actool.Text(fmt.Sprintf("goal %d marked met", goal)), nil
		})
}

func (t *ToolSet) goalMet() actool.CoreTool {
	return writeTool("goal_met", "【タスク全体を即時終了】——タスクの【すべてのゴールが真に達成され、全体が締めくくられた】と確認したときだけ呼ぶ（タスク【全体】の完了であることに注意；そのうち 1 つのゴール/1 つの flag/1 つの脆弱性を達成しただけでは【不十分】——その場合は prove_goal でそのゴールをマークすればよい）。⚠️「今回の計画ラウンドを終える」ためのものではない：今回新たに出す意図がなくても、ワーカーの産出を待っているだけでも、【そのままラウンドを終えればよく、本ツールは呼ばない】（意図 0 個はまったく正常）。通常の判定は prove_goal でゴールを 1 つずつ証明するのを優先する；goal_met は 1 つずつの証明を飛ばして全体を直接締めくくる手段にすぎない。",
		obj(map[string]any{"reason": str("達成理由（ゴールが真に達成された証拠でなければならず、「今回新方向なし」のようなラウンド終了理由は不可）")}, "reason"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Reason string }
			_ = json.Unmarshal(in, &a)
			t.GoalMet = true
			t.Reason = a.Reason
			return actool.Text("acknowledged: goal marked met"), nil
		})
}

// --- worker write tools ---

func (t *ToolSet) addFinding() actool.CoreTool {
	return writeTool("report_finding", "確認済みの脆弱性を記録する。evidence にコマンド出力・ログなど検証可能な証拠を渡す。タスクコンテキストでは現在の intent_id を渡す。戻り値の finding_id は独立した脆弱性レコード ID、finding_node_id は探索ノード ID（1 行目にはそのノード番号を保持）。", obj(map[string]any{
		"vulnclass": str("脆弱性の分類"), "name": str("脆弱性名"), "severity": str("critical|high|medium|low"), "summary": str("検出結果の要約"),
		"intent_id": idp("現在のタスクの意図 id"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "影響を受ける資産 id"},
		"evidence":         str("証拠/PoC テキスト"),
		"evidence_hint_id": idp("任意：本タスクでこの脆弱性に対応するヒントノード ID。その構造化 traffic_refs を自動的に引き継ぐ；継承ヒントや他脆弱性のヒントは参照できない"),
		"traffic_refs": map[string]any{"type": "array", "description": "任意；HTTP/HTTPS の脆弱性では、まず検索してリクエスト/レスポンスが脆弱性の結論を実際に裏付けることを 1 件ずつ確認し、再現順に実 ID を記入する。TCP など非 HTTP の脆弱性、未取得、または確実な記録が見つからない場合は省略または [] を渡し、報告を妨げない；理由を evidence に記し、他の検証可能な証拠を提供してよい。ID を推測したり、ドメイン/時刻から関連を推定したり、パケット補完のためだけに再探査したりしない。用途は baseline 正常時の対照 / proof 脆弱性の証明 / verification 補足検証 / supporting 補助証拠。",
			"items": obj(map[string]any{"traffic_id": str("traffic_search が返した実際のトラフィック ID"), "role": map[string]any{"type": "string", "enum": []string{"baseline", "proof", "verification", "supporting"}}, "note": str("そのトラフィックが脆弱性の結論をどう支えるか")}, "traffic_id")},
	}, "vulnclass", "severity", "summary"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			VulnClass, Name, Severity, Summary, Evidence string
			IntentID                                     json.RawMessage   `json:"intent_id"`
			AssetIDs                                     []json.RawMessage `json:"asset_ids"`
			TrafficRefs                                  []db.TrafficRef   `json:"traffic_refs"`
			EvidenceHintID                               json.RawMessage   `json:"evidence_hint_id"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.ts == nil {
			return actool.Errorf("report_finding にはタスクコンテキストが必要。プラットフォームの対話では add_task_hint で対応するタスクへ脆弱性を引き継ぎ、ヒントに既存の traffic_refs を携帯させ、タスクの Agent に登録させること。登録済みの脆弱性は bind_finding_traffic で追加バインドできる。"), nil
		}
		// 自動バインド無効：evidence 系パラメータは呼び出しを拒否せず無視する。
		// stripTrafficParameters が広告スキーマから既に除去しているが、モデルは往々にして
		// それでもフィールドを出力する——ここで失敗させると、余計なパラメータのせいで確認済みの
		// 脆弱性を捨ててしまう。下の成功パスは evidence_status
		// "not_bound" と「クローズ済み、ページで手動関連付け可能」の注記を返す。これが呼び出し側に必要な情報である。
		if !findingTrafficBindingEnabled() {
			a.TrafficRefs, a.EvidenceHintID = nil, nil
		}
		if len(a.EvidenceHintID) > 0 && pid(a.EvidenceHintID) <= 0 {
			return actool.Errorf("evidence_hint_id は有効なヒントノード ID でなければならない；引き継ぎヒントがない場合は省略"), nil
		}
		refs, err := t.findingRefsFromHint(pid(a.EvidenceHintID), a.TrafficRefs)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		input := db.RecordFindingInput{TaskID: t.taskID, ExplorationID: t.ts.ID(), IntentID: pid(a.IntentID), VulnClass: a.VulnClass, Name: a.Name, Severity: a.Severity, Summary: a.Summary, Evidence: a.Evidence, Worker: t.worker, AssetIDs: pidList(a.AssetIDs)}
		var recorded *db.RecordedFinding
		if t.findingRecorder != nil {
			recorded, err = t.findingRecorder.Record(ctx, input, refs)
		} else if len(refs) > 0 {
			return actool.Errorf("トラフィック証拠ストアが利用不可；脆弱性を未登録"), nil
		} else {
			recorded, err = t.ts.RecordFinding(ctx, input)
		}
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.notifyFinding != nil {
			iid := input.IntentID
			if iid <= 0 {
				iid = t.ownerNode
			}
			t.notifyFinding(iid, a.Summary)
		} else if t.notify != nil {
			t.notify()
		}
		t.writes.Findings++
		// Keep the first line's node-ID contract for existing reporter triggers.
		for i := range recorded.Traffic.Bindings {
			recorded.Traffic.Bindings[i].Snapshot.ReqHead = ""
			recorded.Traffic.Bindings[i].Snapshot.RespHead = ""
		}
		result := struct {
			*db.RecordedFinding
			EvidenceStatus string `json:"evidence_status"`
			EvidenceNote   string `json:"evidence_note,omitempty"`
		}{RecordedFinding: recorded, EvidenceStatus: "bound"}
		if len(recorded.Traffic.Bindings) == 0 {
			result.EvidenceStatus = "not_bound"
			result.EvidenceNote = "脆弱性は保存済み、トラフィック未紐付け。TCP/パケットなしの場合はそのまま続行してよい；確認済みの HTTP トラフィックがある場合は、利用可能な bind_finding_traffic または脆弱性ページで追加紐付けを行い、その後で証拠の引き継ぎを完了すること。脆弱性を重複作成しないこと。"
			if !findingTrafficBindingEnabled() {
				result.EvidenceNote = "脆弱性は保存済み。Agent による自動トラフィック紐付けはクローズ済みのため、ページで手動関連付けできる。"
			}
		}
		raw, _ := json.Marshal(result)
		return actool.Text(fmt.Sprintf("finding recorded: %d\n%s", recorded.NodeID, raw)), nil
	})
}

// recordFact writes a general exploration RESULT/conclusion (not a vuln, not a
// new asset) into the EXPLORATION graph, chained to the intent that produced it.
// This is the home for observations and — importantly — negative results
// ("port closed", "param not injectable", "no login found"). Such conclusions
// must NOT be stuffed into the asset graph via upsert_asset.
// factItem は record_fact のバッチ/単条 1 件分の事実。
type factItem struct {
	Summary    string            `json:"summary"`
	Detail     string            `json:"detail"`
	Evidence   string            `json:"evidence"`   // 1 行の重要な証拠（コマンド+要点となる出力行）。結論を支え、事後の照合を容易にする
	Confidence string            `json:"confidence"` // observed（直接見た）| inferred（現象からの推測）
	IntentID   json.RawMessage   `json:"intent_id"`
	AssetIDs   []json.RawMessage `json:"asset_ids"`
}

// recordOneFact は fact ノードを 1 件書き込み、意図に接続する（intent→yields→fact）。defaultIntent は
// バッチ時の既定意図（本件で intent_id が未指定のときに使う）。
func (t *ToolSet) recordOneFact(it factItem, defaultIntent int64) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary は必須")
	}
	payload := map[string]any{"summary": it.Summary}
	if it.Detail != "" {
		payload["detail"] = it.Detail
	}
	if e := strings.TrimSpace(it.Evidence); e != "" {
		payload["evidence"] = e
	}
	if c := strings.TrimSpace(it.Confidence); c != "" {
		payload["confidence"] = c
	}
	intent := pid(it.IntentID)
	if intent <= 0 {
		intent = defaultIntent
	}
	if intent > 0 {
		node, err := t.ts.GetNode(intent)
		if err != nil || node == nil || node.Kind != db.KindIntent {
			return 0, fmt.Errorf("intent_id は本タスクの意図でなければならない（関連タスクの意図は読み取り専用）")
		}
	}
	// fact は独自のノード種別（脆弱性 finding とは別）。
	id, err := t.ts.AddNode(db.KindFact, payload, 5, "confirmed", t.worker, pidList(it.AssetIDs))
	if err != nil {
		return 0, err
	}
	if intent > 0 {
		_ = t.ts.Link(intent, db.RelYields, id) // chain: intent -> fact（意図→事実）
	}
	t.writes.Facts++
	return id, nil
}

func (t *ToolSet) recordFact() actool.CoreTool {
	return t.writeExpTool("record_fact", "探索の【事実/結論】を探索グラフへ書き込み、それを生んだ意図（intent_id）に接続する。探索結果の記録に使う——フィンガープリント/列挙などの【肯定的結論】と、'ポート閉鎖'/'パラメータ注入不可'/'ログイン入口が見つからない'などの【否定結論】の両方を含む。\n"+
		"⚠️1 回の探索で得た複数の観察は【1 つの事実に集約】し、複数に分割しない。1 つの事実にまとめられるなら極力 1 件で表す：summary=今回の結論を要約した 1 文、detail=関連する詳細（複数の具体的項目を含めてよい）。例：フィンガープリント意図→1 件の事実 {summary:'X サイトの技術スタックと応答特性を識別した', detail:'nginx 1.25 / Vue3 / 200 / title=.. / body_len=..'} とし、ステータスコード・フィンガープリント・タイトルをそれぞれ 1 件ずつ記録しない。1 つの意図は通常 1 件の事実しか生まない。細かく分割しすぎるとグラフが無限に膨張する。\n"+
		"★facts 配列は【互いに異なる】結論を一度に複数書くためのもの（各件で intent_id を省略でき、既定ではトップレベルの intent_id を使う）。戻り値は ids 配列で、facts と同じ長さ・同じ順序。\n"+
		"⚠️ツール出力で【実際に見た】結論だけを書き、推測で補わない。evidence と confidence は不正確な結論でグラフを汚染しないためのもの：\n"+
		"  · evidence=本結論を支える【1 行】の重要な証拠（コマンド+最も証明力のある 1〜2 行の出力）。**必ず簡潔に**——詳細は既に detail にあるので、ここに長い出力を貼り直さない。\n"+
		"  · confidence=observed（出力で直接見た）| inferred（現象からの推測）。\n"+
		"  · **否定系の結論**（注入不可/ポート閉鎖/入口未発見など）は「観察 + 試行的な読み」だけを書く——実際に見たことを述べ、方向を放棄するかはプランナーが全体を踏まえて決める；必ず evidence を付け、手段を尽くしていない、または証拠が弱い（1 回しか試していない、それらしいだけ等）場合は inferred、確かに尽くして直接見た場合のみ observed にする。",
		obj(map[string]any{
			"facts":      map[string]any{"type": "array", "description": "【異なる結論が複数あるときに使う】事実の配列。要素のフィールドは下記のトップレベルと同じ（summary/detail/evidence/confidence/intent_id/asset_ids）；intent_id を省略するとトップレベルの intent_id を使う。戻り値 ids は本配列と同じ長さ・同じ順序。", "items": map[string]any{"type": "object"}},
			"summary":    str("今回の探索結論の【要約 1 文】（detail の概括）"),
			"intent_id":  idp("本件の事実を生んだ意図 id（あなたが受領した意図；バッチ時は各件の既定値）"),
			"detail":     str("本件の事実の関連詳細：今回の探索で得た複数の観察事実をすべてここに書く"),
			"evidence":   str("【1 行】の重要な証拠：コマンド + 結論を最も証明する 1〜2 行の出力。必ず簡潔にし、長い出力を貼らない（詳細は detail に置く）。"),
			"confidence": str("observed（出力で直接見た）| inferred（現象からの推測）。否定結論は必ず正直に明記する。"),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "関連資産 id（任意、0/1/複数）：この事実が関わる資産"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Facts    []factItem `json:"facts"`
				factItem            // 単条モード + バッチの既定 intent_id
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Facts) > 0
			items := a.Facts
			if !batch {
				items = []factItem{a.factItem}
			}
			defaultIntent := pid(a.factItem.IntentID) // トップレベルの intent_id = バッチの既定値

			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.recordOneFact(it, defaultIntent)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}

			if !batch { // 単条：元の戻り値を保持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("fact recorded: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type hintItem struct {
	Text        string            `json:"text"`
	AssetIDs    []json.RawMessage `json:"asset_ids"`
	TrafficRefs []db.TrafficRef   `json:"traffic_refs"`
}

// addOneHint は hint ノード(active/human)を探索グラフに載せ、資産にアンカーでき、id を返す。
func (t *ToolSet) addOneHint(it hintItem) (int64, error) {
	if len(it.TrafficRefs) > 0 && !findingTrafficBindingEnabled() {
		return 0, fmt.Errorf("Agent による自動トラフィック紐付けがクローズ済みのため、traffic_refs を持つヒントは保存されなかった；システム設定で有効にするか、テキストのみを引き継ぐこと")
	}
	if strings.TrimSpace(it.Text) == "" {
		return 0, fmt.Errorf("text は必須")
	}
	var anchors []int64
	for _, raw := range it.AssetIDs {
		if tid := pid(raw); tid > 0 {
			anchors = append(anchors, tid)
		}
	}
	refs, err := db.NormalizeTrafficRefs(it.TrafficRefs)
	if err != nil {
		return 0, err
	}
	payload := map[string]any{"text": it.Text}
	if len(refs) > 0 {
		payload["traffic_refs"] = refs
	}
	// planner の wake-up はここで 1 件ずつ行わない——addHint がバッチ全体の書き込み後にまとめて 1 回トリガーする（ヒントのテキストを携帯）。
	// 一度の add_hint で複数のヒントを出したときに、planner のトリガー行を 1 件ずつ埋め尽くさないようにする。
	return t.ts.AddNode(db.KindHint, payload, 0, "active", "human", anchors)
}

type goalItem struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass"`
}

// addOneGoal は goal ノード(open)を探索グラフに載せる：タスクの根(origin fact、rel spawns)に接続する。
// origin は t.worker(既定 system)：goals 分解器が書き込むものは "goals"、主 agent 実行時は
// "human" を記録する。planner の wake-up は setGoals がバッチ全体の書き込み後にまとめて行う(下記参照)。ここは永続化のみを担当する。
func (t *ToolSet) addOneGoal(it goalItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text は必須")
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(it.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	origin := t.worker
	if origin == "" {
		origin = "system"
	}
	id, err := t.ts.AddNode(db.KindGoal, payload, 0, "open", origin, nil)
	if err != nil {
		return 0, err
	}
	if of, _ := t.ts.OriginFactID(); of > 0 && id > 0 {
		_ = t.ts.Link(of, db.RelSpawns, id) // goals descend from the task root (origin fact)（ゴールはタスクの根から派生する）
	}
	return id, nil
}

// setGoals は【本タスク】に探索ゴール(goal ノード)を追加する。ゴール分解器の投入ツールであり、主
// agent 実行時にゴールを補うツールでもある——同じ管理対象ツールで、Web 側で説明/schema を変更でき、agent ごとにバインドできる。
func (t *ToolSet) setGoals() actool.CoreTool {
	return writeTool("set_goals",
		"【本タスク】に探索ゴール(goal)を追加する。ゴール=最終的に納品/検証可能な結果であり、攻撃手順や偵察動作ではない。\n"+
			"★バッチ優先：複数のゴールは goals 配列に入れて 1 回で投入し、戻り値 ids は同じ長さ・同じ順序（失敗項目は id=0、詳細は errors）。単条なら goals を省略し、トップレベルの text を直接渡す。\n"+
			"vulnclass は任意：対応する脆弱性クラス（SQLi/IDOR など）。ビジネスロジック系のゴールは空でよい。ゴール達成の判定はシステムが met としてマークする。本ツールは追加のみを担当する。",
		obj(map[string]any{
			"goals":     map[string]any{"type": "array", "description": "【まずこれを使う】追加するゴールの配列。順番に処理する。各要素：text(必須、独立して検証可能な最終ゴール) + vulnclass(任意)。戻り値 ids は本配列と同じ長さ・同じ順序。", "items": map[string]any{"type": "object"}},
			"text":      str("[単条] 独立して検証可能な最終ゴール"),
			"vulnclass": str("[単条] 対応する脆弱性クラス(明確な場合)。SQLi/IDOR など；ビジネスロジックのゴールは空でよい"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_goals は無効：ExplorationStore が初期化されていない"), nil
			}
			var a struct {
				Goals    []goalItem `json:"goals"`
				goalItem            // 単条モード：トップレベルの text/vulnclass
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Goals) > 0
			items := a.Goals
			if !batch {
				items = []goalItem{a.goalItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneGoal(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// planner を起こす(バッチで 1 回)。優先は notifyGoal：1 回の set_goals で「人が
				// N 個のゴールを追加した:…」のトリガーを 1 件記録し、1 件ずつ埋め尽くさない；分解器/worker にこのコールバックがなければ → 素の notify に退避(分解器は
				// round-0 では notify も接続されておらず、つまり no-op。この時点では planner がまだ起動していないため)。
				switch {
				case t.notifyGoal != nil:
					t.notifyGoal(addedTexts)
				case t.notify != nil:
					t.notify()
				}
				// 主 agent 実行時にゴールを追加 → 完了/一時停止したタスクを running に戻して続行させる(終了状態ゲートが
				// 通常の notify を飲み込むため、明示的に復活させる必要がある)。このコールバックを接続しているのは mainagent のみ；分解器/worker では nil。
				if t.resumeTask != nil {
					t.resumeTask()
				}
			}

			if !batch { // 単条：元の戻り値を保持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("goal added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type constraintItem struct {
	Text string `json:"text"`
	Type string `json:"type"` // allow | deny
}

// addOneConstraint は操作制約を 1 件 task_constraints に保存する。origin は t.worker(既定 system)：
// 分解器は "goals"、主 agent は "human" を書き込む。
func (t *ToolSet) addOneConstraint(it constraintItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text は必須")
	}
	kind := strings.TrimSpace(strings.ToLower(it.Type))
	if kind == "" {
		kind = "deny" // 既定は禁止として扱う：種類が未指定のときはより保守的に
	}
	if kind != "allow" && kind != "deny" {
		return 0, fmt.Errorf("type は allow または deny でなければならない")
	}
	return t.ts.AddConstraint(kind, text, t.worker)
}

// setConstraints は【本タスク】に操作制約(allow=許可する操作 / deny=禁止する操作)を追加する。ゴール
// 分解器が round-0 で制約を抽出する投入ツールであり、主 agent 実行時に制約を補うツールでもある——同じ管理対象ツールで、Web
// 側で説明/schema を変更でき、agent ごとにバインドできる。制約は planner/worker のシステムプロンプトに注入され、探索の境界を縛る。
func (t *ToolSet) setConstraints() actool.CoreTool {
	return writeTool("set_constraints",
		"【本タスク】に操作制約を追加し、探索の境界を定める：type=allow(許可する操作) または deny(禁止する操作)。\n"+
			"制約=『どの操作をしてよく/してはいけないか』の規定（例『現在のポートのみテストし、他のポートはスキャンしない』『本番 DB への書き込み操作を禁止』『受動的偵察のみ許可』）であり、ゴールでも攻撃手順でもない。\n"+
			"★バッチ優先：複数は constraints 配列に入れて 1 回で投入し、戻り値 ids は同じ長さ・同じ順序（失敗項目は id=0、詳細は errors）。単条なら constraints を省略し、トップレベルの text/type を直接渡す。\n"+
			"タスクのゴール/説明に【明記されている】制約だけを登録し、勝手に作らない；種類がはっきりしないときは deny(より保守的)を使う。",
		obj(map[string]any{
			"constraints": map[string]any{"type": "array", "description": "【まずこれを使う】追加する制約の配列。順番に処理する。各要素：text(必須、1 つの制約) + type(allow|deny)。戻り値 ids は本配列と同じ長さ・同じ順序。", "items": map[string]any{"type": "object"}},
			"text":        str("[単条] 1 つの操作制約の内容"),
			"type":        str("[単条] allow(許可)または deny(禁止)；省略時は deny として扱う"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_constraints は無効：ExplorationStore が初期化されていない"), nil
			}
			var a struct {
				Constraints    []constraintItem `json:"constraints"`
				constraintItem                  // 単条モード：トップレベルの text/type
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Constraints) > 0
			items := a.Constraints
			if !batch {
				items = []constraintItem{a.constraintItem}
			}
			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.addOneConstraint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}
			if !batch { // 単条：シンプルな戻り値を保持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("constraint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) addHint() actool.CoreTool {
	return t.writeExpTool("add_hint", "人間/主 agent の戦略ヒントを探索グラフに載せる。プランナーが次回意図を生成するときにそれを読む。\n"+
		"★バッチ優先：複数のヒントは hints 配列に入れて 1 回で投入する（1 件ずつ呼ぶより往復を節約できる）。戻り値は ids 配列で、hints と同じ長さ・同じ順序（失敗項目は id=0、詳細は errors）。単条なら hints を省略し、トップレベルの text を直接渡す。",
		obj(map[string]any{
			"hints":        map[string]any{"type": "array", "description": "【まずこれを使う】追加するヒントの配列。順番に処理する。各要素のフィールドは下記のトップレベルと同じ（text/asset_ids/traffic_refs）。戻り値 ids は本配列と同じ長さ・同じ順序。", "items": obj(map[string]any{"text": str("ヒントの内容"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": HintTrafficSchema()})},
			"text":         str("[単条] ヒントの内容。例 '認証後のエンドポイントを重点的に掘る'"),
			"traffic_refs": HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "アンカーする資産 id（任意、0/1/複数）"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Hints    []hintItem `json:"hints"`
				hintItem            // 単条モード：トップレベルの text/asset_ids
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Hints) > 0
			items := a.Hints
			if !batch {
				items = []hintItem{a.hintItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneHint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// planner を起こす（バッチで 1 回）。優先は notifyHint：1 回の add_hint で「人が
				// N 件の戦略ヒントを追加した：…」のトリガーを 1 件記録し、planner が「今回のラウンドは新しいヒントでトリガーされた」と明確に分かるようにし、ヒント内容も見せる；
				// このコールバックが未接続なら素の notify に退避（bare wake。ヒントはグラフに折り込まれたままなので、planner 自身が読める）。
				switch {
				case t.notifyHint != nil:
					t.notifyHint(addedTexts)
				case t.notify != nil:
					t.notify()
				}
			}

			if !batch { // 単条：元の戻り値を保持
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("hint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

// killWorkTool は planner が実行中の work を 1 つ（intent id で）終了させるためのツール。
func (t *ToolSet) killWorkTool() actool.CoreTool {
	return t.writeExpTool("kill_work", "実行中の意図(work)を 1 つ終了する。脱線した/無意味な探索を止めるのに使う；終了された意図は stopped としてマークされ、自動再取得されない。まず get_worker_output で何をしているか見てから決めること。",
		obj(map[string]any{"intent_id": idp("終了する意図 id（= work ハンドル）")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.killWork == nil {
				return actool.Errorf("kill_work は現在利用不可"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id は本タスクの意図でなければならない（関連タスクの意図は読み取り専用）"), nil
			}
			if err := t.killWork(id); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("意図 %d の work に終了シグナルを送信した", id)), nil
		})
}

// steerWorkTool は planner が実行中の work を終了させずに途中で方向修正を注入できるようにする：
// メッセージはワーカーの次のツール呼び出し前に届き、次のステップを再計画する（既に集めたコンテキストは保持）。意図内の
// 微修正（「X をやめて Y に集中」）向け；方向全体が誤りなら kill_work + 新しい意図を使う。
func (t *ToolSet) steerWorkTool() actool.CoreTool {
	return t.writeExpTool("steer_work", "実行中の意図(work)にリアルタイムで方向修正の指示を注入する。中断せず、既存の進捗も失わない：ワーカーは次のアクションの前にあなたの指示を受け取り、それに従って調整する。「X をやめて Y に集中」のような【意図内】の微修正に使う；方向全体が誤っている場合は kill_work に切り替えて新しい意図を出すこと。まず get_worker_output で何をしているか見ることを推奨。",
		obj(map[string]any{
			"intent_id": idp("方向修正する意図 id（= work ハンドル）"),
			"message":   str("ワーカーへの方向修正指示。何をやめて何へ向かうかを明確に伝える"),
		}, "intent_id", "message"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.steerWork == nil {
				return actool.Errorf("steer_work は現在利用不可"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
				Message  string          `json:"message"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id は本タスクの意図でなければならない（関連タスクの意図は読み取り専用）"), nil
			}
			if strings.TrimSpace(a.Message) == "" {
				return actool.Errorf("message は必須"), nil
			}
			if err := t.steerWork(id, a.Message); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("意図 %d の work に方向修正指示を注入した（次のステップから有効）", id)), nil
		})
}

// getWorkerOutput は intent id で work の最終結論（または中止時点までの）テキストを返す。
func (t *ToolSet) getWorkerOutput() actool.CoreTool {
	return t.readExpTool("get_worker_output", "本タスクまたは直接関連タスクの意図(work)の最終出力結論を取得する。関連タスクの結果は source_task_id/inherited=true を持ち読み取り専用。正常終了ならそのまとめを返す；終了(stopped)/異常の work は中止時点までの最後の出力を返す。",
		obj(map[string]any{"intent_id": idp("意図 id（= work ハンドル）")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須"), nil
			}
			intentNode, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id は本タスクまたはその直接関連タスクに属していない"), nil
			}
			acts, _, err := t.ts.ActivityListWithSources(id, 0, 1000)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var chosen, fallback *db.Activity
			for i := range acts {
				switch acts[i].Kind {
				case "result":
					chosen = &acts[i]
					fallback = &acts[i]
				case "text":
					fallback = &acts[i]
				}
			}
			pick := chosen
			if pick == nil {
				pick = fallback
			}
			if pick == nil {
				if intentNode.Inherited {
					return jsonResult(inheritedMap(map[string]any{
						"intent_id": id, "final_text": "（この work にはまだ出力がない）",
					}, intentNode.SourceTaskID))
				}
				return actool.Text("（この work にはまだ出力がない）"), nil
			}
			detail, _ := t.ts.ActivityDetailWithSources(pick.ID)
			if detail == "" {
				detail = pick.Summary
			}
			result := map[string]any{
				"intent_id": id, "final_text": detail,
				"summary": pick.Summary, "is_error": pick.IsError,
			}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// traceSteps renders summary-only trace rows, re-truncating each summary to 100
// chars — the stored summary is capped at 200 for the UI transcript; the trace
// tools want it tighter since a whole work's step list is many rows.
func traceSteps(acts []db.Activity) []map[string]any {
	steps := make([]map[string]any, 0, len(acts))
	for i := range acts {
		step := map[string]any{
			"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
			"is_error": acts[i].IsError, "summary": firstLine(acts[i].Summary, 100),
		}
		if acts[i].Inherited {
			inheritedMap(step, acts[i].SourceTaskID)
		}
		steps = append(steps, step)
	}
	return steps
}

// getWorkerTrace は work の実行プロセスを公開する（最終出力だけでなく）：
// ステップ要約の列挙、1 つの work 内でのキーワード検索、特定ステップの詳細取得。thinking ステップはどこでも除外される。
func (t *ToolSet) getWorkerTrace() actool.CoreTool {
	return t.readExpTool("get_worker_trace",
		"ある意図(work)の【実行過程】を見る（最終結論だけを返す get_worker_output とは異なる）。3 つの使い方：\n"+
			"① intent_id のみ → その work の各ステップの要約ストリームを返す（summary≤100 字、step_id を含む；動作の輪郭のみで、完全な出力は含まない）；\n"+
			"② intent_id + q → キーワードに一致したステップの要約のみ返す（要約と完全な出力の両方を検索；依然として summary のみで、内容を見るには③）；\n"+
			"③ intent_id + step_ids → それらステップの完全な内容(detail)を返す；1 回に最大 5 件、超えた分は先頭 5 件のみ返し、未取得分を notice/omitted_step_ids で通知する。\n"+
			"典型的な流れ：まず①/②で疑わしいステップの step_id を特定し、次に③で完全な出力を取得する。思考(thinking)ステップは含まない。直接関連タスクの履歴 trace に対応；その結果は source_task_id/inherited=true を持ち読み取り専用。",
		obj(map[string]any{
			"intent_id": idp("意図 id（= work ハンドル）"),
			"q":         str("キーワード：要約/完全な出力が一致したステップのみ返す（任意；step_ids と排他）"),
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "完全な内容を取得する step_id（①/②の戻り値由来；1 回に最大 5 件、多く渡しても先頭 5 件のみ返し、残りは omitted_step_ids に列挙）"},
			"limit":     intp("要約ストリーム/検索の返却上限（任意）"),
		}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage   `json:"intent_id"`
				Q        string            `json:"q"`
				StepIDs  []json.RawMessage `json:"step_ids"`
				Limit    int               `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id は必須"), nil
			}
			intentNode, nodeErr := t.ts.GetNodeWithSources(id)
			if nodeErr != nil {
				return actool.Errorf(nodeErr.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id は本タスクまたはその直接関連タスクに属していない"), nil
			}
			// ③ step ids による detail ドリルダウン。thinking はストア側で除外済み。
			if len(a.StepIDs) > 0 {
				// Dedup + drop invalid ids first so garbage/duplicates don't eat into
				// the per-call cap. detail is returned in full (untruncated), so the
				// cap bounds one tool result; over the cap we serve the first N and
				// tell the model exactly which ids were deferred, instead of erroring
				// and forcing it to re-plan the call.（無効/重複 id を先に除去し、1 回の上限を無駄に消費しない）
				const maxStepIDs = 5
				var ids []int64
				seen := make(map[int64]bool)
				for _, raw := range a.StepIDs {
					if v := pid(raw); v > 0 && !seen[v] {
						seen[v] = true
						ids = append(ids, v)
					}
				}
				var omitted []int64
				if len(ids) > maxStepIDs {
					omitted = append(omitted, ids[maxStepIDs:]...)
					ids = ids[:maxStepIDs]
				}
				acts, err := t.ts.ActivityByIDsWithSources(ids)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				steps := make([]map[string]any, 0, len(acts))
				for i := range acts {
					if acts[i].NodeID == nil || *acts[i].NodeID != id || acts[i].Inherited != intentNode.Inherited ||
						(acts[i].Inherited && acts[i].SourceTaskID != intentNode.SourceTaskID) {
						continue
					}
					step := map[string]any{
						"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
						"is_error": acts[i].IsError, "detail": acts[i].Detail,
					}
					if acts[i].Inherited {
						inheritedMap(step, acts[i].SourceTaskID)
					}
					steps = append(steps, step)
				}
				result := map[string]any{"intent_id": id, "steps": steps, "returned_step_ids": ids}
				if len(omitted) > 0 {
					// returned_step_ids/omitted_step_ids により、モデルは追加呼び出しに価値があるかをプログラム的に判断できる；notice も同じ内容を文章で伝える。
					result["omitted_step_ids"] = omitted
					result["notice"] = fmt.Sprintf(
						"1 回に取得できるステップの完全な内容は最大 %d 件。今回は先頭 %d 件（%v）を返し、未取得の %d 件は %v。"+
							"これらの内容で特定できれば残りを取得する必要はない；どうしても続ける場合は、これらの step_id でもう一度呼び出す。",
						maxStepIDs, len(ids), ids, len(omitted), omitted)
				}
				if intentNode.Inherited {
					inheritedMap(result, intentNode.SourceTaskID)
				}
				return jsonResult(result)
			}
			// ①/② summary stream, optionally keyword-filtered; 100-char summaries.（要約ストリーム。任意でキーワード絞り込み）
			var acts []db.Activity
			var err error
			if strings.TrimSpace(a.Q) != "" {
				acts, err = t.ts.ActivityTraceSearchWithSources(id, a.Q, a.Limit)
			} else {
				acts, err = t.ts.ActivityTraceWithSources(id, a.Limit)
			}
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			result := map[string]any{"intent_id": id, "steps": traceSteps(acts)}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// searchAllWorkerTraces は本タスクのすべての work のプロセスをキーワード検索する——ワーカーが見たのに
// fact として書き戻さなかったものを探すため。一致した要約のみを返し（≤100 字）、各件に intent_id を付けて追跡可能にする。
func (t *ToolSet) searchAllWorkerTraces() actool.CoreTool {
	return t.readExpTool("search_all_worker_traces",
		"【通常は非推奨。システムが既に大部分の情報を提供している】本タスクの【他の work の実行過程】をキーワード(q)で検索する——あるワーカーが見たのに fact に書き込まなかったもの（あるパス/token/エラーなど）を探すのに使う。"+
			"自分自身の意図のステップは自動的に除外される（それらは既にあなたのコンテキストにある）。"+
			"一致したステップの要約(summary≤100 字)のみを返し、各件に intent_id を付ける；それに基づき get_worker_trace(intent_id, step_ids=[...]) で完全な内容を取得する。",
		obj(map[string]any{
			"q":     str("キーワード（すべての work ステップの要約+完全な出力を検索）"),
			"limit": intp("返却上限。既定 100（任意）"),
		}, "q"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Q) == "" {
				return actool.Errorf("q は必須"), nil
			}
			// 呼び出し者自身の意図のステップを除外する（ワーカー自身の trace は既にそのコンテキストにある）。
			acts, err := t.ts.ActivityTraceSearchAllWithSources(t.ownerNode, a.Q, a.Limit)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			hits := make([]map[string]any, 0, len(acts))
			for i := range acts {
				var intent int64
				if acts[i].NodeID != nil {
					intent = *acts[i].NodeID
				}
				hit := map[string]any{
					"intent_id": intent, "step_id": acts[i].ID, "worker": acts[i].Worker,
					"kind": acts[i].Kind, "tool": acts[i].Tool, "is_error": acts[i].IsError,
					"summary": firstLine(acts[i].Summary, 100),
				}
				if acts[i].Inherited {
					inheritedMap(hit, acts[i].SourceTaskID)
				}
				hits = append(hits, hit)
			}
			return jsonResult(map[string]any{"query": a.Q, "hits": hits})
		})
}

// listWorkerTraces は（graph_overview を持たず意図グラフも見えない）ワーカーに、本タスクの work の軽量な索引を
// 与える——intent_id + 1 行の要約 + 状態——これにより get_worker_trace で検査すべき work を発見できる。
// これがないと、ワーカーは search_all_worker_traces のヒットから返ってくる intent_id しか知らない。まだ open の意図（未実行 →
// 検査するプロセスがない）は除外する。
func (t *ToolSet) listWorkerTraces() actool.CoreTool {
	return t.readExpTool("list_worker_traces",
		"【通常は非推奨。システムが既に大部分の情報を提供している】本タスクの【実行済みの work（意図）】の索引を列挙する：intent_id + 1 行の方向(summary) + 状態。"+
			"あなた(worker)は探索グラフを見られないので、これを使って見る価値のある work を見つける——さらに get_worker_trace(intent_id) でそのステップを見て、get_worker_trace(intent_id, step_ids=[...]) で詳細を取得する。"+
			"実行済み(running/done/exhausted/blocked/stopped)のみを列挙し、まだ走っていない open は含まない。注意：あなたのタスク境界は依然として受領した意図であり、他の work を見るのは観察を再利用したり重複作業を避けるためだけである。",
		obj(map[string]any{
			"q":     str("summary キーワードで絞り込み（任意）"),
			"limit": intp("返却上限。既定 50（任意）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = 50
			}
			all, err := t.ts.ListByKindWithSources(db.KindIntent, 500)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Q))
			out := make([]map[string]any, 0, limit)
			for _, n := range all {
				if n.Inherited && n.State == "running" {
					continue
				}
				switch n.State {
				case "running", "done", "exhausted", "blocked", "stopped": // has run → has a process
				default:
					continue
				}
				var p map[string]any
				_ = json.Unmarshal(n.Payload, &p)
				summary, _ := p["summary"].(string)
				if q != "" && !strings.Contains(strings.ToLower(summary), q) {
					continue
				}
				item := map[string]any{"intent_id": n.ID, "summary": summary, "state": n.State}
				if n.Inherited {
					inheritedMap(item, n.SourceTaskID)
				}
				out = append(out, item)
				if len(out) >= limit {
					break
				}
			}
			return jsonResult(map[string]any{"works": out})
		})
}

// PlannerTools は読み取り + 意図生成 + ゴール判定のツールセット。
func (t *ToolSet) PlannerTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		// cold-digest §6.1: restore folded cold nodes (digest body → members → detail).（折り畳まれた冷ノードを復元）
		t.expandDigest(),
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.listGoals(), t.addIntent(), t.proveGoal(), t.goalMet(),
		t.killWorkTool(), t.steerWorkTool(),
		// report_finding：計画時の状況判断で自身が脆弱性を確証済みなら、直接登録できる（ワーカーと同じツール）。
		t.addFinding(),
		// list_companies：企業一覧 + scope + 資産数を確認する（company_id の取得 / 帰属範囲の理解）。
		t.listCompanies(),
		// list_assets：計画時に DSL で全資産ライブラリを検索する（list_untested_assets の「範囲内の未テスト」視点と組み合わせ、
		// 「ドメイン/フィンガープリント/ポート/ステータスコードなどの条件でライブラリ全体を検索する」能力を補う）。
		t.listAssets(),
		// add_company_scope：計画時にドメイン/IP/CIDR/ICP/キーワードをある企業の資産範囲に含める（一致した資産を自動で取り込む）。
		t.addCompanyScope(),
		// add_task_scope：ルートドメイン全体/企業全体/あるサブドメイン/IP を能動的に本タスクのテスト範囲（カバレッジ分母）に含める。
		t.addTaskScope(),
		// list_untested_assets：必要に応じて本タスク範囲内の未テスト資産を調べる（種類+ページング）。補完テストは自身で判断する。
		t.listUntestedAssets(),
	}
}
