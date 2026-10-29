package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// FindingRecorder is injected by the host; agents never synthesize or copy
// evidence bodies themselves. Its implementation owns the atomic write.
type FindingRecorder interface {
	Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error)
}

// Tool-use guidance is appended without replacing the user's editable prompt.
// It does not require capture or claim that unavailable traffic tools exist.
const findingTrafficGuidance = "\n\n**脆弱性トラフィック証拠（任意）**：report_finding で脆弱性を報告する際、脆弱性の結論を支持すると確認済みの HTTP リクエスト/レスポンスを閲覧している場合は、traffic_refs で再現順に実 ID を紐付けられる；ドメインと時刻は候補の絞り込みにのみ使い、関連を推定しない。TCP など非 HTTP の脆弱性、未取得、または確実な一致がない場合は省略するか [] を渡し、evidence にコマンド出力・ログなど他の検証可能な証拠を残し、未紐付けの理由を説明するのが望ましい。ID を推測してはならず、紐付けのためだけに再探査してもならない。"

func (t *ToolSet) SetFindingRecorder(r FindingRecorder)   { t.findingRecorder = r }
func (w *Worker) SetFindingRecorder(r FindingRecorder)    { w.findingRecorder = r }
func (p *Planner) SetFindingRecorder(r FindingRecorder)   { p.findingRecorder = r }
func (m *MainAgent) SetFindingRecorder(r FindingRecorder) { m.findingRecorder = r }
