// Package notify は脆弱性検出結果の IM / メール配信チャネルアダプタ層を実装する。
//
// 階層：本パッケージは**リーフパッケージ**で、標準ライブラリにしか依存しない。
// データベースも server も知らない。チャネル設定は map[string]any で渡され
// （notification_channels.config という JSONB 列に対応）、配信対象の内容は
// Message で渡される。こうして分けておく利点は、署名計算・UTF-8 切り詰め・
// フィルタマッチングといった本当に間違えやすい箇所を PostgreSQL なしで単体
// テストでき、ホスト側は server 側でオーケストレーションするだけで済むこと。
//
// 並行性の約束：Channel の実装は**ステートレス**でなければならない。同じ
// Channel インスタンスは複数のチャネル設定（さらには同一チャネルの複数の
// ボットインスタンス）で並行に再利用される。資格情報はすべて cfg 引数から
// 渡し、webhook URL のようなものを実装自身のフィールドにキャッシュしてはならない。
package notify

// チャネル種別の識別子。値は notification_channels.kind の有効な集合でもあり、
// server 側のホワイトリストで検証される（findings.status と同様、DB CHECK は
// 使わない。後からチャネルを追加しやすくするため）。
const (
	KindDingTalk = "dingtalk" // DingTalk カスタムボット
	KindFeishu   = "feishu"   // Lark カスタムボット
	KindWeCom    = "wecom"    // WeCom グループボット
	KindWebhook  = "webhook"  // 汎用 Webhook：カスタムメソッド／ヘッダー／JSON テンプレート
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP メール
)

// イベント種別。notification_events.kind に対応する。
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind は config の kind が空のときのフォールバック値。
const InitKind = KindDingTalk

// severityRank は脆弱性レベルを比較可能な序数に写像する。未知のレベルは 0 を
// 返すため、min_severity が設定されていれば未知のレベルは必ず弾かれる——
// 疑わしいときは配信しない。誤検知で埋め尽くされるのを避けるため。
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank はレベルの序数を返す。未知のレベルは 0。
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel は emoji 付きの日本語レベル名を返す。メッセージのタイトルと
// カードの配色に使う。未知のレベルはそのまま返し、勝手にこしらえない。
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 深刻"
	case "high":
		return "🟠 高"
	case "medium":
		return "🟡 中"
	case "low":
		return "🔵 低"
	default:
		return severity
	}
}

// StatusLabel は対応ステータスを日本語に翻訳する。ステータス変更メッセージに使う。
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "未対応"
	case "in_progress":
		return "対応中"
	case "confirmed":
		return "確認済み"
	case "resolved":
		return "対応済み"
	case "fixed":
		return "修正済み"
	case "false_positive":
		return "誤検知"
	case "ignored":
		return "無視"
	case "duplicate":
		return "重複"
	case "risk_accepted":
		return "リスク受容"
	default:
		return status
	}
}

// AtLeast は severity が min のしきい値に達しているかを判定する。min が空なら
// しきい値なしで、一律に通過させる。未知の severity の序数は 0 なので、空でない
// min には必ず弾かれる（severityRank のコメント参照）。
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
