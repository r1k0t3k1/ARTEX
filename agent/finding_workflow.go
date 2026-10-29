package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**脆弱性番号の取り決め**：finding_id は独立した脆弱性レコード ID；finding_node_id は探索ノード ID。list_findings / list_task_findings / node_detail / get_task_node_detail の id は探索ノード ID のままで、同じ戻り値の finding_id から独立した番号を読むこと。get_finding_traffic / bind_finding_traffic は独立した finding_id を使う。旧 update_finding_report の finding_id 引数には引き続き finding_node_id を渡す。report_finding の 1 行目の数字を証拠ツールに使わないこと。番号の誤りに遭遇しても他の数字を推測しないこと。"

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\n既定ではレポート Agent がレポート作成前にトラフィックを照合して紐付ける。報告者は evidence に検証コマンド、重要な出力、既存の実トラフィック ID とその用途を残し、レポート Agent が実行記録と突き合わせて検証できるようにする；紐付けのために追加でパケットを調べる必要はない。明示的な即時紐付けにも対応：traffic_refs または evidence_hint_id で検証済みの参照を提出でき、後者は本タスク指定の hint の構造化参照を読み取る；いずれかが無効なら今回の報告全体が失敗する。TCP/パケットなしではこれらの任意引数は不要。戻り値の finding_id と finding_node_id はそれぞれ独立レコードと探索ノードを表す。"
		case "add_hint", "add_task_hint":
			note = "\n確認済みの脆弱性を引き継ぐ際は、対応するヒントの traffic_refs に検証済みトラフィックの ID、用途、説明、順序を残し（単条はトップレベル、バッチは対応する hints 要素に）、text にそれが証明する具体的な脆弱性を記す。呼び出し側は既存のトラフィック参照を捨ててテキストだけを引き継いではならない。未検証の候補を証拠として渡してはならない。"
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**トラフィック証拠の引き継ぎ（任意）**：自動紐付けは既定でレポート Agent が脆弱性の登録後、レポート作成前に完了する。報告者は evidence に検証コマンド、重要な出力、既存の実トラフィック ID とその用途を残し、タスクでは intent_id を付けてレポート Agent が追跡できるようにする；紐付けのために追加でパケットを調べる必要はない。Auto / Planner が代理で報告する場合は実行者の既存の参照を捨てないこと。add_hint / add_task_hint では traffic_refs で引き継げる；明示的な即時紐付けは report_finding の traffic_refs / evidence_hint_id にも引き続き対応する。TCP またはパケットなしでも通常どおり登録し、ID を推測してはならず、紐付けのためだけに再探査してもならない。"
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\nプラットフォームの対話にタスクコンテキストがない場合は、report_finding を直接呼び出さず、add_task_hint で対応する既存タスクへ引き継ぎ、タスク Agent に登録させ、list_task_findings で結果を照合する。"
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\nゴール達成を判定する前に、今回の既存証拠の報告/引き継ぎを完了させること。証拠の引き継ぎが未完了なのに、テキストの脆弱性が登録されただけでタスクを終了し、ワーカーをキャンセルしてはならない；パケットなしでも待機や無理なキャプチャは要求されない。"
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**レポート作成前のトラフィック自動関連付け（有効）**：あなたは今回トリガーされた脆弱性についてトラフィックを照合して紐付け、その後レポートを書く。まず report_finding の戻り値 JSON または get_task_node_detail / list_task_findings から明確な finding_id と finding_node_id を取得する。脆弱性の詳細、対応する意図の実行記録、既存の証拠一覧を読み、報告者が引き継いだ実 ID を優先的に使う。今回の検証が HTTP でトラフィックツールが利用可能なら、traffic_search で候補を絞り込み、traffic_get でリクエスト/レスポンスを 1 件ずつ照合して実際にその脆弱性を裏付けることを確認する；ドメインと時刻は絞り込みにのみ使い、帰属を証明しない。確認した証拠を再現順に bind_finding_traffic(finding_id, traffic_refs) で関連付け、baseline / proof / verification / supporting を選択して用途を説明する。操作できるのは今回の脆弱性のみで、脆弱性を重複作成したりターゲットを再探査したりしない。バインド成功後に再呼び出し、get_finding_traffic で最新の version を取得し、必要な本文を読み、実際に読んだ version を evidence_version として update_finding_report に渡す（その finding_id 引数は引き続き finding_node_id を使う）。既存の紐付けは重複して追加する必要はない。TCP、未取得、ツール利用不可、または確実な一致がない場合は自動紐付けをスキップし、テキスト/コマンドの証拠に基づいて通常どおりレポートを書き、理由を説明する。トラフィックを揃えるために推測してはならない。紐付けに失敗したら成功を宣言しない；既存の証拠を保持し、レポートに未紐付けの理由を記す。"
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "任意：検証済みで、このヒント内の特定の脆弱性に対応するトラフィック参照。順序を保持する；引き継ぎ後、report_finding は evidence_hint_id を渡してこれらの参照を携帯できる。", "items": obj(map[string]any{"traffic_id": str("実トラフィック ID"), "role": str("baseline / proof / verification / supporting"), "note": str("このトラフィックがどの結論を支えるか")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d は本タスクのヒントノードでなければならない（継承ヒントは直接紐付けに使えない）", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
