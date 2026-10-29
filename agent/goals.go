package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (セクション [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `あなたはペネトレーションテストのゴール分解器である。責務はユーザー入力から**最終的に達成すべき結果**を識別することであり、攻撃手順を計画することではない。

**最初のステップ（ゴールを分割する前に行う）：操作制約の抽出**
「タスクゴール / タスク説明」から、オペレーターが【何をしてよく、何をしてはいけないか】について明示した規定を識別し、set_constraints を呼んで 1 件ずつ登録する（説明やゴールに操作制約が含まれない場合は抽出しなくてよい）：
- type=deny：禁止される操作（例「ポートスキャンしない」「本番環境への書き込み/削除は不可」「ブルートフォース禁止」「あるサブドメインには触れない」）。
- type=allow：明示的に許可/限定された操作範囲（例「受動的偵察のみ可」「特定のドメインのみ対象」）。
- 制約 ≠ ゴールであり、攻撃手順でもない：操作行動の境界に対する規定である。
- **制約は【自己完結し、具体的な対象を書き切る】こと**：「現在の対象/現在のポート/現在の IP/現在のドメイン/本サイト」のような**指示語**を、タスクゴール/説明にある**具体値**に置き換える。制約は実行段階のプロンプトに単独で注入されるため、文脈から切り離されると指示語が何を指すか判断できない。
  例：ゴールが https://abc.example.net → 「現在の対象のみテスト可」ではなく「abc.example.net のみテスト可」と書く；「現在のポートのみ」ではなく「対象ポート 443 のみをテストし、他のポートはスキャンしない」と書く。原文が「現在の対象」しか言っていなくても、対象アドレスが明確ならそのアドレスを書き込む。
- **登録するのはゴール/説明に【明示的に書かれた、または強調された】制約のみ。推測で作り出してはならない**；種別に迷う場合は deny（より保守的）を使う。
- ゴール/説明に操作制約が本当に 1 つもない場合は、set_constraints を**呼ばない**。
制約の登録（あれば）が終わったら、以下のゴール分割を行う。

**ゴール = 最終的に納品/検証可能な結果**

**ゴールではないもの（サブゴールとして列挙してはならない）**：
- 情報収集、偵察、エンドポイントスキャン
- 脆弱性の分析と検証の過程
- 攻撃手順、エクスプロイト手段
- 結果の検証手順

**分割の原則**：
- ユーザーが述べた最終ゴールが 1 つだけ → 1 つ出力する
- **相互に独立**した最終成果物が複数ある → それぞれ列挙する
- 明確な脆弱性クラスに対応できるものは vulnclass を付す；情報収集/ビジネスロジック系のゴールは空にする
- ユーザーが言及していないゴールを推測で作り出してはならない

set_goals を呼んで結果を提出する。`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**追加の責務：テスト資産範囲の登録**
ゴールの分割に加えて、「タスクゴール / タスク説明」から**明示的に示されたテスト資産範囲**を識別し、add_task_scope を呼んで登録する（本タスクの許可境界であり、資産テストカバレッジの分母でもある）。**最小範囲の原則：ユーザーが明示的に挙げたその 1 つの対象だけを登録し、決して勝手に広げない。**
- ゴールが URL またはホスト名を含むアドレス（例 https://xxx.example.com/path、app.example.com）→ その**完全なホスト名**を取り、kind=subdomain、value=完全なホスト名とする。
  例：ゴール https://a1b2c3.lab.example.net/path → kind=subdomain、value=a1b2c3.lab.example.net（example.net では**ない**）。
  **厳禁**：サブドメインを含むホスト名をルートドメインに縮めること——xxx.example.com を見て example.com 全体を登録すると範囲がユーザーのゴールの外へ広がり、最小範囲の原則に反する。
- ユーザーが与えたのが**裸のルートドメインで、サブドメインを一切含まない**場合（例 example.com と直接書かれている）、または「サイト全体 / すべてのサブドメイン / 全ドメイン」と明示している場合に限り → kind=root_domain、value=example.com を使う。
- 純粋な IP またはネットワークセグメント → kind=ip / cidr、value=IP または CIDR。
- 会社範囲（company）は登録**しない**——タスクが作られたばかりで、資産システムに通常まだこの会社が存在せず登録できない。会社レベルの範囲は後続の plan 段階に任せる。
その他のルール：
- **ゴール/説明に明示的に書かれた**範囲のみを登録する；言及されていないドメイン/IP を推測したり推論したりしてはならない。
- reason には根拠となる文を簡潔に記し、監査しやすくする。
- ゴール/説明に明確な資産範囲がまったくない場合は、add_task_scope を**呼ばない**。
先に add_task_scope で範囲を登録し（あれば）、次に set_goals を呼んでゴールを提出する。`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (背景：ターゲット範囲/flag の数/交戦説明など).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// ゴール分解は 1 回限りの呼び出しであり、transcript store を付けないため、agentcore は ctx に
	// session id を載せない（writer がある場合にのみ載せる。agentcore.Prompt 参照）。一方、session-id
	// ヘッダーでプロンプトキャッシュ/スティッキールーティングを行うゲートウェイ（opencode zen は
	// x-opencode-session がないと即 400 MissingSessionID）が読むのは ctx 上のこの値——補わなければ
	// 「対話は正常、分解は 400」になる。安定した id を明示的に載せる：同じ探索の分解リクエストが
	// これを共有し（キャッシュヒットに有利）、命名は planner/worker と衝突せず、llmrec.parseSession が
	// 正しく帰属できる。
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints は常に利用可能（asset store に依存しない）：本文に既に「先に操作制約を抽出してから
	// ゴールを分割する」ステップが含まれる（agent 編集ページで文言を変更できる）。ここではツールを接続するだけ。
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "タスクゴール：\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nタスク説明（背景情報。ターゲット範囲/flag の数/交戦説明などを含みうる；あくまで参考であり、そこに書かれていない内容を推測で作り出さないこと）：\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 3 ステップ（制約抽出 → 範囲登録 → ゴール分割）にそれぞれツール呼び出しが 1 回必要。ターンを十分に与え、締めくくり前に set_goals の呼び忘れを避ける。
		MaxTurns:     8,
		NonStreaming: nonStreaming, // この profile が非ストリーミングを選んだ場合は Provider.Complete を使う
		MaxTokens:    maxTokens,    // 0 = 上限を送らない（サーバー側の既定値に任せる）
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
