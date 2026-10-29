package notify

// Snapshot は notification_events.snapshot という JSONB 列の契約。書き手は db 層の
// 脆弱性保存トランザクションで、読み手は server 層の配信エンジンとフィルタ
// マッチング。本パッケージに定義を置くのは、これが「通知ドメイン」のペイロード
// だから：db はシリアライズだけを担い、フィールドの意味は理解しない。
//
// なぜ脆弱性のフィールドを冗長に保存し、レンダリング時に引き直さないのか：
// 脆弱性は後から名前・レベル・ステータスを変更されうるが、配信内容は**事象発生
// 当時**の結論を反映すべきである——引き直すと「後から low に書き換えられた」
// 危険な誤情報を得ることになる。加えて fan-out とレンダリングが findings/tasks/
// assets の 3 テーブルを JOIN せずに済む。
type Snapshot struct {
	// イベント種別：finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// kind=finding_status_changed のときのみ非空。
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item は配信待ちの脆弱性 1 件で、チャネルによるレンダリングに供する。
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets は解決済みの資産表示名（ドメイン／IP など）。server 層が埋める——
	// 本パッケージはデータベースに触れないので、名前を取得できない。
	Assets []string
	// DetailURL は脆弱性詳細へのバックリンク。空は public_base_url 未設定を意味し、
	// レンダリング時に省略する。
	DetailURL string
	// ステータス変更イベント専用。2 つとも非空のとき「未対応 → 修正済み」のように
	// レンダリングする。
	FromStatus string
	ToStatus   string
}

// IsStatusChange はその項目がステータス変更イベントかどうかを報告する。
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title は項目の表示タイトルを返す：人の命名による name を優先し、脆弱性タイプ
// vulnclass にフォールバックし、どちらも空ならプレースホルダーを使う——空の
// タイトルは決して出力しない。
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(名称未設定の脆弱性)"
}

// Message はチャネル 1 回の送信内容の全体。
type Message struct {
	// 単発配信時は長さ 1。集約配信（digest）時はバッチ全体。
	// 空スライスは不正であり、呼び出し側が少なくとも 1 件を保証すること。
	Items []Item
	// Batch=true のときは集約メッセージとしてレンダリングする（タイトルを変え、
	// 時間窓と件数を持たせる）。
	Batch bool
	// WindowMinutes は集約周期（分）。Batch=true のときのみ「直近 N 分」という
	// 文言に使う。レンダリング時に time.Since で計算せず設定から明示的に渡すのは
	// 意図的：レンダリングを決定的に保ち、テストしやすくするため。
	WindowMinutes int
	// HomeURL はプラットフォームのパネルアドレス（グローバル public_base_url）。
	// 空ならパネルへの入口を持たない。
	HomeURL string
}
