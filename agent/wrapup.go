package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// 締めくくりプロンプト(wrap-up / settlement prompt)：agent が【ステップ数上限(MaxTurns)】または
// 【タイムアウト(run_seconds/MaxDuration)】で終了させられたとき、SDK の settlement 段階がこのプロンプトを注入し、
// agent は識別済みでまだ書き戻していない内容を先に保存し、その後に 1 文のまとめを出力する。途中で放り出さないためである。
//
// 各 agent の締めくくりプロンプトは管理画面から必要に応じて上書きできる（agents.wrapup_prompt に保存）。空ならここの
// 組み込み既定を使う。編集できるのは【プロンプト本文】のみ；どのツールを無効にするか、締めくくり自体に何ターンの予算を与えるかはコード固定のポリシーである。

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// 組み込み既定の締めくくりプロンプト。agent key で索引する。worker は従来ハードコードだった settleWrapUpPrompt
// (worker.go で定義)を再利用し、planner/mainagent はそれぞれ専用版を持つ。一致しない場合（カスタム agent）は汎用フォールバックを使う。
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults：各 agent の締めくくり段階【自身】のターン予算の組み込み既定（管理画面で >0 に上書き可能）。
// いずれも 10 ターンとし、締めくくり段階で保存に十分なステップ数を確保する。一致しない場合は genericWrapupTurns を使う。
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "今回の計画ステップの予算がまもなく尽きる——注意すべきは【このラウンド】が終わるだけであり、システムはその後も状況の変化に応じてあなたを再び呼び出して計画を続けさせる。タスクの終了ではないので、ここで計画全体を締める必要はない。今回すでに考えがまとまった結論は書き出し、このラウンドを無駄にしないこと。ただし【締めくくりのために無理に意図をひねり出さない】こと（今回 0 個の意図でもまったく正常な結果である）：(1) 【今すぐ投入すべき】探索方向をすでに判断しているなら、add_intent を 1 回使って一括投入する（考えたものを溜め込まない）；(2) ある検出結果/事実によって達成が証明されたゴールは、prove_goal を呼んで met とマークする（見落とさない）；(3) 段階を分ける必要のある連続エクスプロイトチェーンを識別したなら、TodoWrite で書き留め、次回の呼び出しで続けて投入できるようにする。終わったらそのままこのラウンドを終了してよい。まとめのテキストは不要。"

const mainAgentWrapUpDefault = "ステップ数がまもなく尽き、今回の対話は終了する。これ以上新しい探索/操作を開始しないこと。**1 文だけのプレーンテキスト**で、現在の進捗、重要な結論、推奨する次のステップをユーザーにまとめること。"

const genericWrapUpDefault = "予算の枯渇によりまもなく終了される。まず完了済みでまだ保存していない結果を書き戻し、その後に**1 文だけのプレーンテキスト**で、何をしたか、どんな重要な結論を得たかをまとめること（この文が今回の実行結果として表示される）。"

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- タスクレベルタイムアウトの締めくくりプロンプト ----------
//
// per-run の締めくくりプロンプトとは【別の 2 系統】である：per-run は「あなたの今回の run の予算が尽きた」；タスクタイムアウトは
// 「タスク全体が期限に達し、まもなく終了する」。意味はしばしば正反対になる（特に planner：per-run は「止まらず計画を続けよ」、
// タスクタイムアウトは「期限なので計画を止めて最終判定を行え」）。worker/planner にのみ設定する。

// WrapupTaskTimeoutOverride / …TurnsOverride：タスクタイムアウトの締めくくりプロンプトとターン数の DB 上書き
// （agents.task_timeout_wrapup_prompt / _max_turns に接続。worker/planner のみ）。
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**タスク全体がタイムアウト上限に達し、まもなく終了する**（今回の run の予算ではなく、探索全体の期限が来た）。これが最後の機会である：(1) 識別済みでまだ書き戻していない内容を【すべて】保存する——新規資産は insert_assets、探索の結論/事実は record_fact、確認済みの脆弱性は report_finding；(2) 新しいコマンド/探索を一切開始しない；(3) **最後に 1 文だけのプレーンテキスト**で、この意図における重要な結論をまとめる。"

const plannerTaskTimeoutDefault = "**タスク全体がタイムアウト上限に達し、まもなく終了する**（今回のラウンドではなく、タスク全体の終了である）。現在の【すべての】事実と検出結果に基づき、最後のゴール判定を行うこと：証拠によって達成が証明されたゴールは prove_goal を呼んで met とマークする（見落とさない）。**新しい意図は一切生成しない**（この時点で投入しても実行されない）。判定が終わったら締めくくる。まとめのテキストは不要。"

// TaskTimeoutWrapupDefault は、ある agent のタスクタイムアウト用の組み込み既定締めくくりプロンプトを返す（管理画面のプレースホルダ/既定への復元用）。
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // 未設定(mainagent/chat)なら空文字列を返す
}

// resolveTaskTimeoutWrapup：DB 上書き（非空）> 組み込み既定。空文字列はその agent にタスクタイムアウト用プロンプトがないこと
// （worker/planner 以外）を意味し、その場合呼び出し元は per-run のプロンプトへフォールバックすべきである。
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // 既定では per-run のターン数を流用する
}

// wrapupSettlementForTask は、タスクの deadline を認識した worker/planner の run 用に settlement を構築する。設計ドキュメントの §5 を参照：
//   - clamped=true  → 今回の run がタスク deadline に挟まれている：Timeout による締めくくり=タスクの期限到達→タスクタイムアウト用プロンプト；
//     MaxTurns による締めくくり=挟まれた窓内でステップ数が先に尽きただけでタスクにはまだ数分残っている→per-run 用プロンプトへフォールバック。
//   - clamped=false → タスクにはまだ余裕がある：どちらの reason でも per-run 用プロンプトを使う（すなわち wrapupSettlement へ退化する）。
//
// harness に渡す PromptByReason は締めくくり時に【実際の】reason でその場で選ぶため、構築時の取り違えは起きない。
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // フォールバック（非 clamped 時に 2 種類の reason で使う値でもある）
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // タスクの期限到達
				harness.ReasonMaxTurns: perRun, // ステップ数が先に尽きたが、タスクにはまだ時間が残っている
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
