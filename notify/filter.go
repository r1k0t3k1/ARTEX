package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter は notification_channels.filter という JSONB 列の契約：チャネルインスタンスの
// フィルタ条件。全フィールドが任意で、省略は「フィルタしない」を意味する——これが
// 不正な設定に対するフォールバックの意味論であり、ParseFilter を参照。
type Filter struct {
	// MinSeverity は最低レベルのしきい値（low/medium/high/critical）。空=しきい値なし。
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs が空配列なら制限なし。非空ならイベントと共通部分を持つことを要求する。
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude が空ならすべて受理。非空なら vulnclass がいずれかの
	// キーワードに一致することを要求する。
	// VulnClassExclude はいずれかのキーワードに一致すれば除外（除外は包含より優先）。
	// マッチングは大文字小文字を区別しない部分一致——正規表現より安全：ユーザーが
	// 正規表現を間違えてもチャネルが黙って無効化されない。
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange はそのチャネルが脆弱性ステータス変更イベントを受け取るかを
	// 決める（realtime モードでのみ意味を持つ）。
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter はチャネルのフィルタ設定を解析する。
//
// **決して error を返さない。** これは意図的な設計選択：フィルタ条件の設定が
// 不正なときは一律にゼロ値の Filter（= フィルタしない = すべてヒット）へ退化させる。
// 脆弱性通知システムにとって、**1 件多く配信する方が高危険度を黙って取りこぼすより
// はるかにまし**だから。解析失敗を「配信しない」に変えると、ユーザーには設定済みに
// 見えながら実際には何も配信しないチャネルを渡すことになる——これが最悪の失敗
// モードだ。
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// 解析に失敗したときは f はゼロ値のまま、つまりフィルタしない。
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity は s が有効なレベルのしきい値かどうかを報告する
// （空文字列はしきい値なしを表す）。
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate はフィルタ設定のうち**値が制限される**フィールドを検証し、チャネル保存時に
// 呼ばれる。
//
// なぜ書き込み時に必ず止める必要があるか：Match は未知のしきい値に対して
// `rank >= 0` を判定し、常に真になる——つまり min_severity を 1 文字打ち間違える
// （"hgih"）と、フィルタは**黙って無効化**されて「全部配信」になる。これは本パッケージ
// の「取りこぼすより多く配信する」という取捨の方向（取りこぼさない）とは一致するが、
// 結果としてユーザーはレベル別配信をしているつもりで、実際にはすべての脆弱性を
// グループに流し込んでおり、設定ミスを示す兆候も一切ない。こうした「静かな劣化」こそ
// 入口で止めるべきもの。
//
// 注意：Validate は**書き込み**経路でのみ使う。読み取り経路は引き続き ParseFilter の
// 寛容な意味論を通すことで、履歴データにすでに存在する不正値がチャネル全体を
// 読み出せなくすることを防ぐ。
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("最低レベル %q が無効です。選択肢：low / medium / high / critical、または空欄で制限なし", f.MinSeverity)
	}
	return nil
}

// Match はイベントをそのフィルタ条件を持つチャネルへ配信すべきかを判定する。
//
// **決して error を返さない**。理由は ParseFilter と同じ：内部の異常はすべて
// 「ヒット」として扱う。判定順：イベント種別 → レベルのしきい値 → タスク／資産の
// 範囲 → 脆弱性タイプのキーワード。
func Match(f Filter, s Snapshot) bool {
	// ステータス変更イベントは明示的に有効化したチャネルだけが受け取る。既定は
	// 無効。ほとんどの利用者が「配信」で期待するのは「新しい脆弱性の検出」であって、
	// 各ステータスの遷移を逐一追いかけることではないため。
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// 除外優先：除外キーワードのいずれかに一致すれば、包含リストにも一致していても
	// 対象外とする。
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// 小さな集合なので線形走査で十分。両者の規模は「人がチェックボックスで選ぶ
	// 数十件」であり、map を作るコストが利得を上回る。
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold は s がいずれかのキーワードを含むかを報告する（大文字小文字
// を区別しない）。
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
