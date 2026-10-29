package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// 本ファイルは「組み込みツール」を純粋なコードから、列挙可能で DB から上書き可能なカタログへ変える：
//   - BuiltinToolSeeds()：3 つの実行 agent の組み込みツールセットを seed レコード（key +
//     説明 + 引数 schema + 既定でバインドする agent）に展開し、サーバー起動時に tools テーブルへ冪等に播種する。
//   - ToolResolve フック：実行時に DB の tools 行に基づき、組み立て済みツールへ「agent によるフィルタ +
//     説明/schema の上書き + 引数の既定値注入」を行う。key/handler はコード層に残り、DB が変えるのは「説明文と既定値」だけ。
// handler（Call の挙動）は常にコード由来——DB では変えられず、変えられるのはモデルが見る説明と既定の入引だけである。

// ToolSeed は組み込みツールの播種可能なスナップショット：key は CoreTool.Name()（handler と固定結合で
// UI からは読み取り専用）、Desc/Schema はコード上のツール定義から取り、Agents はコードが既定でどの agent に与えているかを表す。
type ToolSeed struct {
	Key    string         // = CoreTool.Name()。主キーで変更不可
	Desc   string         // 最上位の説明（UI で上書き可能）
	Schema map[string]any // 引数 JSON-Schema（構造は読み取り専用、description/default は UI で変更可能）
	Agents []string       // 既定でバインドする agent key（worker/planner/mainagent）
}

// builtinToolsByAgent は「読み取り専用の空殻」ToolSet（nil stores）で各実行 agent の
// ドメインツールセットを構築する。ツールのコンストラクタはクロージャを Spec に詰めるだけで、構築時に store を参照しないため nil 安全——
// ここではこれらのツールを Name()/Description()/InputSchema() の読み取りにのみ使い、決して Call しない。
//
// SDK の汎用ツール actool.DefaultTools()（Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash）は意図的に【含めない】：これらは各 agent が常に固定的に持ち、「誰にバインドするか」の取捨がなく、説明の大半は Prompt()
// にある（本表は Description() しか上書きしないため、半端な上書きで誤解を招く）。seed しない → DB 行なし → ToolResolve が
// そのまま通し、上書きしないため、挙動は従来と変わらない。カタログで管理できるのは artex 自身のドメインツールのみである。
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals（ゴール分解器）は既定で set_goals + set_constraints をバインドする：これらで分解したゴールと
		// 抽出した操作制約を DB へ書き込む。mainagent と同じ被管理ツールを共有し、web 側で説明/schema の変更や agent ごとの選択ができる。
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto は既定で脆弱性報告 + 資産管理ツールをバインドし、その他のドメインツールは UI で必要に応じて選択できる。
		// 新規 DB はこの seed で書き込まれ、既存 DB は seedAutoDefaultBindings で移行される。
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest（独立ペネトレーション agent）の既定バインド：資産照会 / 資産挿入 / 脆弱性報告 / 脆弱性照会 / 企業照会。
		// 新規 DB はこの seed で書き込まれ、既存 DB は seedPentestDefaultBindings で移行される。
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound：これらの system ツールは通常どおりカタログに入る（web 側で見え、手動で agent ごとに選択できる）が、
// 既定では【どの agent にもバインドしない】——ToolResolve は空バインドのツールを全 agent に対して一律に破棄するため、明示的な opt-in が必要。
// それでもある agent の base ツールセットに残している理由（例：goal_met は PlannerTools にあり）：1 つは seed が
// それを構築して desc/schema を取得できるようにするため、もう 1 つはユーザーが手動でバインドし直したとき実行時の base に存在し、ToolResolve が残せるようにするため。
//
// goal_met：個別の prove_goal を経ずに、グローバルに【タスク全体の完了】を直接宣言するもので、影響が大きく誤判定のリスクもあるうえ、
// 「prove_goal が最後のゴールをマーク → 自動で締めくくり」と重複するため、既定ではどの agent にも与えず、必要時に手動でバインドする。
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds は各 agent の組み込みツールセットを重複排除して seed リストに統合する：同名ツール（list_assets のように
// 複数 agent が持つもの）は 1 件にまとめ、Agents は和集合を取る；defaultUnbound のツールはバインドを強制的に空にする。
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // カタログには入り手動バインド可能だが、既定ではどの agent にも与えない（null ではなく [] を保存し、他のツールと揃える）
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve は設定されていれば、agent の組み立て済みツールリストを DB の tools テーブルに
// 基づいて後処理する：この agent にバインドされていない（またはグローバルで無効化された）ツールを落とし、
// 残りをラップしてモデルに DB 上書き後の description/schema を見せ、既定の入引を注入する。DB 行が一致しないツール（MCP/skill/ホスト系ツール
// など traffic）はそのまま通す。nil = ツールを変更しない。server/assembly.go で接続される。
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool は t をラップし、Description()/InputSchema() が DB の上書きを返し、
// Call() がモデルの省略時にスカラー引数の既定値（schema の "default" プロパティ由来）を注入するようにする。
// Name/Prompt/権限/スケジューラのフラグは t へ委譲するため、ツールの同一性と handler は変わらない。desc/schema が空なら t のものへフォールバックする。
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool は CoreTool のデコレータである：元のツールを埋め込み（挙動に関するメソッド——Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name——は
// すべて委譲される）、モデル向けの description/schema と既定値注入のみを上書きする。
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults は（編集された可能性のある）schema で宣言されたスカラー引数の既定値を、
// モデルがフィールドを省略したか空/null のままにした場合に入力 JSON へ埋める。構造（名前/型/必須）は変更せず、
// 既定値のみをマージする。
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults はスカラー引数（string/integer/number/boolean）について
// properties[k]["default"] を抽出する。配列/オブジェクトの既定値はスキップする：マージの意味が曖昧で、
// 驚きに見合わないため。
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
