package agent

import (
	"strings"

	"github.com/Autumn-27/artex/db"
)

// constraintBlock renders this task's operation constraints (task_constraints) as a
// high-priority block appended to the planner/worker system prompt. allow/deny are
// grouped; empty string when there are no constraints (or ts is nil). The framing
// deliberately puts these ABOVE the exploration/expansion heuristics so a declared
// boundary wins the tug-of-war against "chase another entry surface".
func constraintBlock(ts *db.ExplorationStore) string {
	if ts == nil {
		return ""
	}
	rows, err := ts.ListConstraints()
	if err != nil || len(rows) == 0 {
		return ""
	}
	var allow, deny []string
	for _, c := range rows {
		text := strings.TrimSpace(c.Text)
		if text == "" {
			continue
		}
		if c.Kind == "allow" {
			allow = append(allow, "- "+text)
		} else {
			deny = append(deny, "- "+text)
		}
	}
	if len(allow) == 0 && len(deny) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【操作制約（最優先。以下すべての探索/面拡大のヒューリスティクスより上位。意図を 1 つ生成するたび、動作を 1 つ実行するたびに、違反していないか必ず自己検査し、違反するなら実行してはならない）】：")
	if len(allow) > 0 {
		b.WriteString("\n許可される操作：\n")
		b.WriteString(strings.Join(allow, "\n"))
	}
	if len(deny) > 0 {
		b.WriteString("\n禁止される操作：\n")
		b.WriteString(strings.Join(deny, "\n"))
	}
	b.WriteString("\n（制約外の新しいターゲット/ポート/ホストを発見しても、それは許可を得たことを意味しない：上述の許可範囲に含まれない限り out-of-scope の事実として記録してスキップし、それに対して意図を派生させたり動作を実行したりしてはならない。）")
	return b.String()
}
