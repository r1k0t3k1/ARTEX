package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// 本ファイルは IM 配信のチャネル設定とイベント層である。配信タスクの取得と
// 状態遷移は db/notification_delivery.go を参照。
//
// このファイルを変更する際は、次の 2 つの不変条件を必ず守ること：
//
//  1. 脆弱性を書き込むトランザクション（RecordFindingTx）は
//     InsertNotificationEventTx を一度呼ぶだけで、通知関連のテーブルを読まず、
//     フィルタ照合もしない。ここで読み取りを導入すると、ユーザーの誤設定に
//     よって脆弱性書き込みトランザクションが汚染・中断される恐れがある。
//  2. フィルタ照合は決してエラーを返さない。設定が不正な場合は「一致」と
//     みなす（notify.Match 参照）。取りこぼすより、多く配信する方がよい。

// ErrNotificationChannelNotFound はチャネルが存在しないことを示す。
var ErrNotificationChannelNotFound = errors.New("通知チャネルが存在しません")

// 配信状態。
const (
	NotifyStatePending = "pending" // 送信待ち
	NotifyStateSending = "sending" // いずれかの dispatcher が取得済みで、リースが未失効
	NotifyStateSent    = "sent"    // 送信済み
	NotifyStateFailed  = "failed"  // 再試行を使い切った、または恒久的な失敗。手動で再送可能
	NotifyStateSkipped = "skipped" // チャネルが無効化済みで、送信しない
)

// 配信モード。
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode は配信モードをホワイトリスト検証する（findings.status と同様に
// DB CHECK を使わず、将来の拡張を容易にする）。
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel は 1 つのチャネルインスタンス設定である。Config と Filter は
// 生の JSON のまま保持し、解釈は notify パッケージに委ねる。db 層はそのフィールドの
// 意味を理解しない。
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled をポインタにしているのは「フィールド未指定」と「明示的な false」を
	// 区別するため。フロントのトグルは変更されたフィールドだけを送信する。
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled はチャネルが有効か返す。Enabled が nil（未ロード）のときは有効として扱う。
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent は 1 件のイベント事実である。
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels は全チャネルインスタンスを返す。有効なものが先頭、
// 同順位は id 順。並び順を SQL に置くのは、UI と dispatcher が同じ安定した順序を
// 見られるようにするため。
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID は単一のチャネルを取得する。
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel はチャネルを新規作成または更新する。
//
// 更新時は呼び出し側が明示したフィールド（非 nil / 非空）だけを上書きする。これにより
// フロントは部分編集のドロワーフォームを送信でき、表示していない config の
// フィールドを送り返さずに済む。送り返すと「マスク値が実際の鍵を上書きする」
// 事故につながる。
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// ここでは 0 に**あえて**手を加えない。0 は正当な設定であり、「レート制限なし」を
	// 意味する。
	//
	// かつて `if c.RatePerMin <= 0 { c.RatePerMin = 既定値 }` と書いていた。意図は
	// 「未指定時に安全な既定値を入れる」だったが、「明示的な 0」も一緒に飲み込んで
	// しまった。ドキュメント・UI のヒント・takeTokens はいずれも 0 をレート制限なしと
	// 解釈するのに、ここだけが黙って 20（DingTalk/WeCom/Telegram）や 100（Feishu）に
	// 変えていたため、操作者は制限を解除したつもりでも実際は毎分 20 件で
	// 止められ、しかも何の表示もなかった。
	//
	// 「未指定」と「明示的な 0」の区別は呼び出し側しか知らない（リクエストボディで
	// フィールドを省略したか、明示的に 0 を渡したか）ため、既定値は server 層が
	// フィールド省略時に埋める。notifyCreateChannel 参照。
	if c.RatePerMin < 0 {
		return 0, errors.New("レート制限値は負にできません")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled は有効/無効を切り替える。
//
// チャネルを無効化するときは、未送信の配信もまとめて skipped にする。そうしないと
// 再有効化した際に「無効化期間中に積み上がった」古い脆弱性が一度に届き、鮮度が
// 失われているうえ新規と誤認されやすい。
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "チャネルが無効化されました", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel はチャネルを削除する。配信履歴は外部キーの
// カスケードで削除される（チャネル設定がなくなれば履歴を解釈できない）。
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx は呼び出し側のトランザクション内で、配信イベントを
// **ベストエフォート**で書き込む。
//
// 脆弱性書き込み経路における通知関連の変更はこれだけである。INSERT 1 回のみで、
// テーブルを読まず、チャネルも認識せず、フィルタも実行しない。トランザクションが
// コミットされれば「脆弱性の永続化」と「配信タスクの存在」が原子的に一致し、
// コミット成功なのにキューに入らずメッセージが永久に失われる窓は存在しない。
//
// 重要な設計が 2 つあり、いずれも場当たりではない：
//
//  1. **なぜ SAVEPOINT を使うか**：PostgreSQL ではトランザクション内でいずれかの
//     文がエラーになるとトランザクション全体が aborted 状態に入り、以降のすべての
//     文（COMMIT を含む）が失敗する。したがって「この INSERT のエラーを無視して
//     呼び出し側にコミットさせる」ことは PG では不可能で、保存点でエラーをこの
//     1 文に隔離するしかない。保存点がなければ「全体ロールバック」しか選択肢が
//     ない。
//  2. **なぜ全体ロールバックが誤りか**：配信は便利機能にすぎず、脆弱性の記録こそが
//     製品そのものである。通知テーブルの問題（旧 DB の未マイグレーション、ディスクの
//     一時障害）で高危険度の脆弱性が保存できなくなってはならない。そこでここで
//     エラーを隔離し、ログを記録して false を返し、脆弱性の書き込みは通常どおり
//     コミットさせる。代償はこの配信 1 件を失うことである。
//     error ではなく bool を返すのは意図的で、呼び出し側はこれを書き込み成否に
//     影響するエラーとして扱うべきではない。
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] 配信イベントのシリアライズに失敗 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] 保存点の作成に失敗 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] 配信イベントの書き込みに失敗 finding=%d（脆弱性の記録には影響なし）: %v", findingID, err)
		// 保存点までロールバックし、トランザクションを aborted 状態から救済する。
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] 保存点へのロールバックに失敗 finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// 保存点を解放し、長いトランザクションに無用な保存点が溜まるのを防ぐ。
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent は InsertNotificationEventTx の独立トランザクション版で、
// 既存トランザクション外の呼び出し箇所（チャネルの「テストメッセージ送信」など、
// 実在する finding を持たないもの）で使う。
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("通知イベントスナップショットのシリアライズに失敗しました: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents は未割り当ての脆弱性イベントを、現在有効なチャネルごとに
// 配信タスクへ展開し、今回処理したイベント数と作成した配信数を返す。
//
// 一連の処理は 1 つのトランザクション内で行う。イベントは FOR UPDATE SKIP LOCKED で
// 取得するため、複数プロセスが同時に実行してもそれぞれ異なる行を取得する
// （プロジェクト内のアーカイブキューの取得も同じ手法を使っている。
// db/task_archives.go の completeNextArchiveJob 参照）。
//
// フィルタ照合はあえて SQL ではなく Go 側に置いている。チャネルのフィルタ条件は
// 任意フィールドの JSONB であり、6 通りの組み合わせを SQL で表現するとクエリが
// 保守しにくくなる。一方チャネル数は「手で設定する数件」なので、全件ロードして
// メモリ上で 1 件ずつ照合する方が速く、テストもしやすい。
//
// どのチャネルにも一致しなかったイベントも fanned_out を立てる。そうしないと
// 割り当て待ちの集合に永遠に残り、tick ごとに再走査されてしまう。
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // コミット成功後は no-op

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// スナップショットは自分たちで書いたものなので理論上は必ず解析できる。解析失敗は
		// 配信フローを止めないが、このイベントは全フィールドが空になり、フィルタ条件を
		// 持つチャネルすべてでスキップされる。1 件配信が減る方が、壊れた行 1 つで
		// キュー全体が詰まるよりましである。
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind は行の値を優先する。スナップショット内のものはレンダリング用の複製で、旧バージョンが書いた可能性がある。
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// 今回のイベントを割り当て済みにする。どのチャネルにも一致しなかったイベントも同様にマークする（関数コメント参照）。
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx はトランザクション内で有効なチャネルを取得する。
// 件数が少ないためページングもキャッシュもしない。キャッシュは「設定変更がいつ
// 反映されるか」という余計なタイミング問題を持ち込む。
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames は資産 id を短い表示名に解決し、配信メッセージで使う。
//
// 返却順は引数と同じで、長さは引数より短くなることがある（存在しない id は
// スキップされる）。引数の順序を保つのは、同じ脆弱性のメッセージが複数回の配信で
// 資産順を保つため。そうしないと再試行後に届いたメッセージで資産の順序が変わり、
// 「資産が変わった」と誤読される。
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName は資産タイプに応じて最も識別しやすい識別子を選ぶ。
// フォールバックは空文字で、「名前が取れない資産」をどう表示するかは呼び出し側が
// 決める。本関数はプレースホルダを捏造しない。さもないと「資産#42」のようなノイズが
// 配信メッセージに混じり、読者は実在のドメインだと勘違いしてしまう。
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify は脆弱性の対応状態を更新し、同じトランザクションで
// 状態変更の配信イベントを登録する。
//
// 戻り値：from=変更前の状態、found=脆弱性が存在するか、notified=イベント登録の
// 成否。
//
// 意図した挙動が 3 つある：
//   - 状態が実際に変わらないときはイベントを登録しない。フロントのドロワーが同じ値を
//     重複送信したり、自動化スクリプトが冪等に再生したりしても、配信ノイズを
//     生むべきではない。
//   - 脆弱性が存在しないときは found=false を返し、一切書き込まない。呼び出し側が
//     404 に変換する。
//   - イベント登録の失敗は状態更新に影響しない（RecordNotificationEventTx の保存点の
//     説明を参照）。したがって notified=false でも状態は更新済みであり、呼び出し側は
//     それをエラーにしてはならない。
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx は**呼び出し側のトランザクション**内で脆弱性の状態を更新し、
// 状態変更の配信イベントを登録する。
//
// トランザクション関数に切り出したのは、状態を変更するすべての経路で同じ意味論を
// 共有するため。以前は patchFinding だけが通知付きの版を通っていたが、**再検証の
// 結論が「修正済み」のとき**（finding_retests にある `UPDATE findings SET status=...`）
// は直接 DB に書いていたため、`on_status_change` を設定したチャネルはこの種の
// 状態遷移をまったく受信できなかった。画面上では状態が静かに変わり、運用者が
// プラットフォームを開いて初めて気づく状態だった。
//
// 戻り値：from=変更前の状態、found=脆弱性が存在するか、changed=状態が実際に
// 変わったか、notified=イベント登録の成否（登録失敗は状態更新に影響しない。
// RecordNotificationEventTx 参照）。
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// 状態が実際に変わっていなければイベントを登録しない。同じ値の重複送信や
		// 冪等な再生が配信ノイズを生むべきではない。
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats は通知ページ上部の概要カウントである。
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // 最も古い送信待ち配信の経過ミリ秒数
}

// NotificationStatsSnapshot は通知システムの健全性を集計する。
// BacklogAgeMS は「配信が詰まっているか」を最も直接示す指標で、pending 件数より
// はるかに有用である。同じ 3 件の滞留でも、3 秒のこともあれば 3 時間のこともある。
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
