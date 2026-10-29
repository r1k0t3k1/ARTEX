package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 本ファイルは配信タスクの取得と状態遷移を扱う。
//
// 取得には長いトランザクションではなく「リース」を使う。行を sending にし、
// next_attempt_at を未来へ押し出してリース失効時刻とする。トランザクションを
// コミットしてからネットワーク配信を行う。これにより配信中の DB ロック保持を
// 避けられる。ネットワーク要求は数秒かかることがあり（クライアントのタイムアウトは
// 15 秒）、行ロックを握り続けると同じ DB の他の書き込みを圧迫する。
//
// 代償として、配信中にプロセスがクラッシュすると行は sending のまま残る。これは
// **自己修復可能**である。リースが失効すると next_attempt_at が過去になり、次の
// 取得で同じ行を再び拾える（取得条件の state IN ('pending','sending') 参照）。
// 再試行回数は取得時点で既に +1 されているため、クラッシュしても無限再試行には
// ならない。MaxNotifyAttempts 回の機会を使い切ると failed に落ち、手動対応を待つ。

// MaxNotifyAttempts は 1 件の配信の最大試行回数（初回を含む）。
// 配信エンジンではなくここで定義する。これは状態機械自身のポリシーであり、
// エンジンはそれを実行するだけだからである。
const MaxNotifyAttempts = 3

// MaxDigestBatchSize は 1 つのダイジェストバッチで一度にまとめる配信の最大数。
//
// 存在理由はリソースである。1 つのダイジェスト周期で数万件の脆弱性が出ることは
// 十分ありうる（全件スキャン 1 回で到達しうる）。上限を設けないと、取得時に全行を
// メモリへ読み込み、1 つの超長メッセージとしてレンダリングし、チャネルの長さ上限で
// 大半が切り捨てられる。メモリを浪費するうえ、切り捨てられた脆弱性を**静かに
// 失う**ことになる。上限を設ければ、超過分は DB に残って次のバッチとなり、
// 次の周期に自然に送信されるため失われない。
//
// 500 という値の根拠：メッセージにレンダリングしたとき、WeCom の 4096 バイト上限内で
// まだ「読める内容」が残る量である。これより大きくしても、切り捨て位置が後ろに
// ずれるだけである。
const MaxDigestBatchSize = 500

// NotificationDelivery は 1 件の配信タスクで、レンダリングに必要なチャネル設定とイベントスナップショットを含む。
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// 結合ロードされたレンダリングコンテキスト。JSON には含めない（server 層が DTO を組み立てる）。
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind はイベントから引き継ぎ、履歴一覧から脆弱性詳細へ直接遷移できるようにする。
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind は一覧表示用の冗長フィールドで、フロントの二次クエリを省く。
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery は配信行の統一的な読み取り形状である。配信 + イベント
// スナップショット + チャネル設定。メッセージ 1 件のレンダリングには 3 つすべてが
// 必要で、別々に問い合わせると 3 回の往復が発生する。
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery は 1 回の取得を表す。まず sel で候補を選んでロックし、次にそれらを
// sending にしてリースを延長する。sel 内の lease の位置は呼び出し側が $n で
// プレースホルダ化し、自分で引数を渡す。
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries はあるチャネルの期限到来したリアルタイム配信を最大 limit
// 件取得する。
//
// **チャネル単位**で取得するのは意図的で、「グローバルに一括取得してから選んで送る」
// 方式は取らない。レート制限は配信エンジンでチャネルごとに管理されており、この
// チャネルが今回あと何件送れるかを先に知ってから同じ件数を取得して初めて、制限が
// 再試行回数を消費しないで済む。逆に先に取得してから捨てると、制限で止められた行も
// すでに attempts を 1 回消費しており、3 回の予算を純粋な待ち時間で使い果たして
// failed に落ちる。
//
// 条件には「リースが失効した sending」を含める。これがクラッシュ時の自己修復の
// 落とし所である。lease は 1 回の配信の最悪所要時間（チャネルの HTTP クライアントは
// タイムアウト 15 秒）より十分大きくする必要がある。そうしないと同一行が 2 つの
// dispatcher から同時に配信される。同時に無効化済みチャネルも除外する。
// 無効化操作は既存の配信を skipped にしているが、ここでもう一段防いで、
// 無効化と取得が並行したときの取りこぼしを防ぐ。
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now()
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue は、そのチャネルに期限到来バッチが十分溜まったかを報告する。
// 送信待ちの配信が存在し、**最も古いもの**の経過時間がダイジェスト周期に達して
// いる場合に真となる。
//
// 判定根拠を壁時計ではなく最古配信の経過時間にしているのは、作りたてのチャネルが
// 時刻の区切りに合わせて即座に 1 件だけの「ダイジェスト」を吐かないようにするため
// であり、長く滞留したバッチがもう一巡無駄に待つことも防げる。
//
// ClaimDigestBatch と分けているのは意味論が異なるため。本関数は「送るべきか」だけを
// 答える。一方、取得はそのチャネルの**すべて**の送信待ち行（経過時間が未到達のものを
// 含む）を持ち去る。そうしないと 1 周期が複数メッセージに分割され、ダイジェストの
// 意味が失われる。
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= now() - make_interval(secs => $4)
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch はあるチャネルの現在期限到来している送信待ち配信を、
// 1 つのダイジェストバッチとして取得する。1 バッチは最大 MaxDigestBatchSize 件。
//
// 同じバッチの配信はすべて batch_id を共有し、集合内の最小 id をバッチ番号に
// する（安定・可読・追加シーケンス不要）。再試行時は COALESCE で元のバッチ番号を
// 保つため、「この N 件は一緒に送ったもの」という事実が複数回の再試行後も
// 成立し続ける。
//
// ランダムではなく id 昇順で先頭 N 件を取る。最も早く発生した配信が最初に送られ、
// 滞留時にも「新しい脆弱性が先に送られ、古いものが永遠に後回しになる」飢餓が
// 起きない。
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit は**メモリ上の上限**で、呼び出し側は MaxDigestBatchSize を渡す。ここでも
	// もう一段挟み、呼び出し側がより大きな値を渡すのを防ぐ。
	//
	// 「レート制限の枠」をバッチサイズとして受け取らないのは意図的である。レート制限の
	// 単位はメッセージ件数であり、1 バッチはメッセージ 1 件を送ってトークンを 1 つ
	// 消費する（server 層の takeTokens が差し引く）。これは「1 バッチに脆弱性を何件
	// 入れるか」とは別の次元である。かつて rate_per_min を digest に効かせようとして
	// 毎回のリクエスト予算をバッチサイズとして渡した結果、rate=20/min のチャネルでは
	// 1 バッチに脆弱性 1 件しか入らず、digest がダイジェスト文言付きのリアルタイム
	// 配信に退化した。レート制限を変えたいなら takeTokens の want を変更し、ここには
	// 手を触れないこと。
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now() AND c.enabled
ORDER BY dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries は「選定 + sending への更新とリース延長 + 完全な行の読み取り」を
// 1 つのトランザクション内で実行する。postClaim は任意の追加ステップ
// （ダイジェストバッチが batch_id を書き込むのに使う）。
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // コミット成功後は no-op

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// sending にして next_attempt_at を未来へ押し出す。この未来時刻がリース失効時刻と
	// なるため、「リース未失効」と「再試行時刻未到達」は同じ条件式で表現でき、
	// 列を追加する必要がない。
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=now()+make_interval(secs => $2)
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent は一括の配信を送信済みにする。
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries は一括の配信を pending に戻し、再試行時刻を後ろへずらす。
//
// 新しい中間状態を導入せず pending に戻すのは、「残り何回の機会があるか」を
// 1 か所（MaxNotifyAttempts）だけで表現するためで、再試行ポリシーに応じて
// 状態機械の分岐が膨らむのを避ける。
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=now()+make_interval(secs => $2), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries は一括の配信を pending に戻して即座に再取得可能にし、**取得時に
// 加算した 1 回の試行を取り消す**。
//
// 用途は 1 つだけである。ダイジェストメッセージをチャネルの長さ上限に合わせて分割
// 送信する際、今回のメッセージに入らなかった項目は次のバッチへ回す。それは失敗では
// ないので再試行予算を消費すべきでない。取得時に attempts は楽観的に +1 済みなので、
// ここで必ず減らす。さもないと 500 件の滞留が 1 段 20 件で 25 段に分割され、
// 末尾の項目は 3 段目で MaxNotifyAttempts により failed と判定されてしまう。
// しかもそれらは一度もエラーを起こしていない。
//
// GREATEST(...,0) は「手動再送で attempts を 0 に戻した後、ここに到達した」場合を
// 受け止め、カウントが負にならないようにする。
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=GREATEST(attempts-1, 0), next_attempt_at=now(), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries は一括の配信を最終失敗としてマークし、配信履歴から手動で再送されるのを待つ。
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// プレースホルダは $3 から始まる。$1 は state、$2 は last_error。
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery は 1 件の配信を手動で再送する。pending に戻し、
// 再試行回数を 0 にし、即時期限到来にする。カウントを消すのは意図的である。
// 人が「再送」を押すのは、直前までの失敗原因が解消済みという意味であり、
// 古いカウントで制限するのは不合理である。
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("配信 %d は存在しないか、現在の状態では再送できません", id)
	}
	return nil
}

// NotificationDeliveryFilter は配信履歴の検索条件である。
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries は配信履歴をページングして返す。新しいものが先頭。
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError はエラーメッセージを列が受け付けられる長さに切り詰める。
// チャネルが返すレスポンスボディは長いことがあり（汎用 Webhook が自前サービスを
// 叩く場合は特に）、切り詰めないと履歴一覧のペイロードが膨らむ。
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// 文字境界まで戻し、UTF-8 文字の途中で切れてフロントが文字化けするのを防ぐ。
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders は start から始まる $n プレースホルダ列と対応する引数を生成し、
// IN (...) で使う。例えば start=3, ids=[7,8] → "$3,$4", [7,8]。
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
