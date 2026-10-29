package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// グローバル設定キー（settings のキー・バリュー表に保存する。テーブル追加は不要）。
const (
	// settingNotifyEnabled は配信の全体スイッチ。既定はオン。これは保守時に一括で止血する
	// ためのものであって、機能の有効条件ではない——本当の有効条件は「チャネルが設定されているか」。
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL は脆弱性詳細へのバックリンクを生成するための外部公開 URL
	// （例: https://artex.example.com）。空ならメッセージにバックリンクのボタンを含めない。
	// プロジェクトに再利用できる外部アドレス設定がないため、ここで新たに追加した。
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes はまとめモードの周期（分）。
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick は配信エンジンのポーリング間隔。3 秒はこのエンジンのリアルタイム性の上限であり、
	// 「脆弱性が DB に保存される」から「メッセージが IM に届く」までの主要な遅延要因でもある。
	notifyTick = 3 * time.Second
	// notifyLease は配信を取得するときのリース期間。1 回の配信の最悪所要時間
	// （notify パッケージの HTTP クライアントのタイムアウト 15 秒）より明確に長い必要がある。
	// そうでないと同じ行が 2 つの dispatcher に同時に配信されてしまう。
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick は 1 ラウンドで展開するイベント数を制限し、チャネルを初めて
	// 有効化したときに過去の滞留分を一気に配信タスクへ展開してしまうのを防ぐ。
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes はまとめ周期の既定値。
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick はチャネルにレート制限がない場合の 1 ラウンドあたり配信上限。
	// 存在意義は「レート制限なしのチャネル + 1 回のスキャンで数千件の脆弱性」という状況が
	// 1 ラウンドのループを長時間ブロックするのを防ぐこと。
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick は 1 チャネルあたり 1 ラウンドで最大何件配信するか。
	//
	// この上限は**リース期間**から逆算したもの。取得時にその行へ付けるのはリース
	// （notifyLease = 3 分）であり、1 ラウンドで直列配信する件数が多すぎて最悪所要時間が
	// リースを超えると、後ろの数件は送り終わる前にリースが切れる。単一プロセスなら問題ない
	// （Run は単一 goroutine で直列に走り、tick は再入しない）が、**2 つのプロセスが同じ
	// DB に繋ぐ**場合、対向プロセスがリース切れの行を再取得して二重送信し、さらに
	// attempts を二重にインクリメントして、元のプロセスがまだ配信中なのに失敗と判定してしまう。
	//
	// 値：3 分リース / 30 秒単発タイムアウト = 6 は**リースをちょうど使い切る**ゼロ余裕なので
	// 採用できない。5 を取って最悪所要時間 150 秒に 30 秒の余裕を残す。この関係は
	// TestNotifyTickBudgetFitsWithinLease で固定されている——notifyLease、
	// notifySendTimeout、この値のどれを変えてもあのアサーションが失敗する。
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout は 1 回の配信のタイムアウト。前の定数の値も同時に決めており、
	// 両者の積は notifyLease を超えてはならない。TestNotifyTickBudgetFitsWithinLease を参照。
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff は失敗時のリトライのバックオフ系列で、添字は試行済み回数。
// 3 回のチャンス（初回を含む）は db.MaxNotifyAttempts に対応し、両者は必ず一緒に変更する。
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier は脆弱性通知の配信エンジン。
//
// Scheduler と並び、独立した goroutine として動作する（server.New を参照）。Scheduler の
// tick を意図的に再利用していないのは、通知のリアルタイム性の要求（3 秒）がトリガーの
// 業務リズムと異なるうえ、両者の失敗が互いに巻き込まれないようにするため——通知が
// 詰まっても agent のトリガーに影響を与えてはならない。
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu は buckets を保護する。チャネル数が少なく競合も低いので、1 本のミューテックスで
	// 十分であり、これのために細粒度の構造を持ち込む価値はない。
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket はチャネル単位のトークンバケット。
//
// 「毎分カウントしてからクリアする」スライディングウィンドウではなくトークンバケットを
// 使うのは、前者の境界効果がひどいため。ウィンドウ末尾で 20 件送り、その直後にさらに
// 20 件送ると、プラットフォームから見れば 1 秒間に 40 件でレート制限に掛かる。
// トークンバケットは一定速率で補充されるので、この種のバーストを自然に避けられる。
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run は ctx が終わるまでループする。server.New が 1 回起動する。
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step は 1 ラウンドを実行する：まず新規イベントを展開し、次に期限の来たタスクを配信する。
//
// どの段階が失敗してもログを残してループを止めない——通知システムの障害がプロセス全体の
// 問題に格上げされてはならない。各 tick は独立しており、次のラウンドが自然に再試行する。
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] イベントの展開に失敗: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] チャネルの読み込みに失敗: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// トークンバケットの計量単位は**メッセージ件数**（= HTTP リクエスト数）であり、
		// 脆弱性の件数ではない。リアルタイムモードでは両者は同じ（脆弱性 1 件 = メッセージ 1 件）だが、
		// まとめモードではまとまった脆弱性 1 バッチがメッセージ 1 件になるので、トークンは 1 つだけ消費する。
		//
		// どちらのモードでも、まずトークンバケットに問い合わせてから枠の分だけ取得する——順序を
		// 逆にしてはいけない。そうしないとレート制限で弾かれた配信がすでにリトライ回数を消費してしまう。
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan はまとめチャネルの今ラウンドのトークン消費量とバッチサイズの上限を返す。
//
// 2 つの戻り値は**次元の異なる量**であり、それこそが独立した関数にしている理由:
//
//   - tokens はメッセージ件数。脆弱性のバッチがメッセージ 1 件、HTTP リクエスト 1 回に
//     まとまるので常に 1。したがって rate_per_min は digest にも効く（毎分これだけの
//     まとめメッセージまで）。
//   - claimLimit はこのバッチに最大何件の脆弱性を詰めるか。これはメモリ上の上限にのみ
//     縛られ、リクエスト予算とは無関係。
//
// かつて rate_per_min を digest にも効かせようとして、ラウンドごとのリクエスト予算
// （notifyMaxSendsPerChannelPerTick、リースから逆算したもの）をそのままバッチサイズとして
// 渡していた。結果、rate_per_min=20 のチャネルは 3 秒の tick でトークンが 1 つしか補充されず、
// まとめメッセージ 1 件に脆弱性が 1 件しか入らない——digest が「まとめ文付きのリアルタイム
// 通知」に退化し、読者が受け取るのは「直近 30 分で 1 件追加」という連続になり、
// db.MaxDigestBatchSize には永遠に到達しなかった。
//
// この症状はエンドツーエンドテストでは見つけにくい（既存のケースはどれも十分大きい limit を
// 手動で stepDigest に渡し、step 内の枠計算を迂回していた）。そこで判断をここに集約し、
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget で直接固定している。
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime はあるチャネルのリアルタイムタスクを取得して配信する。脆弱性 1 件につきメッセージ 1 件。
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] リアルタイム配信の取得に失敗 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("チャネル種別 %q は未登録", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// レンダリング失敗はローカルデータの問題であり、リトライしても直らない。
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest はバッチの期限が来たとき、あるチャネルの未送信の配信をまとめメッセージ 1 件にして送る。
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] まとめバッチの期限判定に失敗 channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] まとめバッチの取得に失敗 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("チャネル種別 %q は未登録", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// スナップショットが壊れてメッセージに入れられなかった配信は、明示的に失敗と判定する。
	// そうしないと included の外に取り残され、メッセージにも失敗リストにも入らない——送信
	// 成功時には後続の一括マークから漏れ、ずっと sending のままリースが切れるたびに
	// 繰り返し取得され続ける。
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "イベントのスナップショットを解析できず、この脆弱性をメッセージにレンダリングできない"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] 壊れたスナップショットの配信を失敗として記録できず channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] スナップショットを解析できず %d 件の配信をスキップ channel=%d", len(skipped), ch.ID)
	}
	// メッセージに入ったものだけを send に渡す。included[i] と msg.Items[i] は厳密に対応しており、
	// send はこの対応関係に依存して「チャネルが前 K 件まで収めた」という報告を正しい配信行に反映する。
	n.send(ctx, channel, cfg, msg, included)
}

// send は配信し、結果に応じて状態を遷移させる。
//
// 同じバッチ（まとめモードでは数十件になることもある）は 1 つの送信結果を共有する。届くか、
// バッチ全体が再試行か。1 件ずつ再試行はしない——まとめメッセージは 1 通であり、一部だけ
// 再送するとバッチの意味が崩れる。
//
// 唯一の例外は**チャネルの長さ上限による分割**：チャネルが実際には前 K 件しか収めなかったと
// 報告したなら、K+1 件目以降は一緒に成功扱いにするのではなく次のバッチへ回さなければならない。
// そうしないと切り捨てられた脆弱性はメッセージにも失敗リストにもなく、完全に消えてしまう。
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// 1 回の配信に上限を設け、あるチャネルが詰まってこのラウンドの残り全チャネルを
	// 引きずり込むのを防ぐ。
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// チャネルが報告する件数が配信数を超えるはずはない。本当に起きたならレンダリング層の
			// 計算違いなので、全件配信として処理しつつ問題を記録する。レコードを壊すよりましである。
			log.Printf("[notify] チャネルの配信報告件数 %d が配信数 %d を超過 channel=%s、全件配信として処理",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] 配信済みの記録に失敗 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// このメッセージがチャネルの長さ上限に達した：残りは即座に列へ戻し、次の tick で続けて送る。
			// RescheduleDeliveries ではなく DeferDeliveries を使う——これは失敗ではなく、
			// リトライ予算を消費すべきでない（取得時に楽観的に +1 しているので、そこで減らされる）。
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("このメッセージがチャネルの長さ上限に達し、先頭 %d 件のみ配信、残りは次のバッチへ", delivered)); err != nil {
				log.Printf("[notify] 分割分の再キューに失敗 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// チャネルはエラーも報告せず、何件届いたとも言わなかった。失敗として扱い（バックオフへ）、
		// この配信が取得され続けたまま永遠にマークされない事態を避ける。
		err = fmt.Errorf("チャネルが配信件数を報告しなかった（delivered=%d）", delivered)
	}

	// 失敗の扱いは**1 件ずつ**決める。バッチ全体の最大試行回数で判断してはならない。
	//
	// かつては `if maxAttempts(deliveries) >= MaxNotifyAttempts` でバッチ全体を失敗確定していたが、
	// バッチ内の各件の試行回数は同じではない：すでに 2 回リトライした古い配信（attempts=2）が、
	// 同じバッチ内の真新しい配信（attempts=1）を一緒に failed へ引きずり込んでしまう——
	// 新しい脆弱性は 1 回もリトライされずに永久に失われ、「古い行が新しい行を道連れにしない」
	// という当初の意図とは正反対だった。
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] 失敗状態の記録でエラー channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("%d 回リトライしても失敗: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] 失敗状態の記録でエラー channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// 遅延ごとにグループ化して再スケジュールする。バックオフは 3 段階しかないので、
	// グループ数は自然に少なく、1 件ずつ UPDATE を発行する必要はない（500 件のバッチで
	// 500 回の往復が発生してしまう）。
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] 配信の再スケジュールに失敗 channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] 配信失敗 channel=%d kind=%s 永久失敗=%d リトライ枯渇=%d 再試行待ち=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries は all のうち keep にないものを返す（ポインタ同一性で比較）。
// 「メッセージに入れられなかった」配信を見つけるために使い、それらは必ず明示的に
// 処理しなければならない——グレーゾーンに残してはならない。
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt はチャネル実装を取得し、その設定を解析する。
// ok=false は種別が未登録であることを示し、配信は無限リトライではなく即座に失敗と判定すべき。
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// 設定の解析に失敗したら空の map を渡す。チャネル自身の Validate が「どのフィールドが
		// 欠けているか」を報告してくれ、そのエラーは JSON 解析エラーよりユーザーの修復に役立つ。
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle は脆弱性 1 件のメッセージをレンダリングする。
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch はまとめメッセージをレンダリングする。スナップショットは 1 件ずつ解析し、
// 1 件壊れていたらその 1 件だけをスキップして、まとめ全体を失わせない。
//
// 戻り値の included は msg.Items と**厳密に 1 対 1 で対応する**（i 番目の配信 ↔ i 番目の項目）。
// この対応関係は必須条件である：呼び出し元は「チャネルが前 K 件まで収めた」という報告に基づいて
// 先頭 K 件の配信を配信済みにする。ここで壊れたスナップショットをスキップしながら、スキップした
// 配信を included から取り除かないと、添字がずれて——本来失敗すべき壊れた項目が配信済みに、
// 正常な項目が未配信と誤判定される。壊れたものは呼び出し元が明示的に失敗と記録する。stepDigest を参照。
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// 壊れたスナップショットはメッセージにも included にも入れない——その処理は呼び出し元が
			// 担う（「配信済み」に紛れ込ませず、明示的に失敗と記録する）。
			log.Printf("[notify] まとめバッチで解析できないスナップショットをスキップ delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("まとめバッチの配信 %d 件がすべて解析不能", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor はイベントのスナップショットを通知項目にレンダリングし、併せて資産名と詳細への
// バックリンクを解決する。
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// 資産名の解決失敗で通知を止めるべきではない。名前が読めないことは通知が届かないより
		// はるかに軽く、メッセージから資産の行が 1 行減るだけである。
		log.Printf("[notify] 資産名の解決に失敗 finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// 詳細ページのルートは web/src/app/(main)/function/findings/detail/page.tsx を参照。
		// そちらはクエリパラメータ id から脆弱性の id を読む。
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens はチャネルのトークンバケットから**最大 want 個**のトークンを取り、実際に
// 取れた数を返す。
//
// トークン 1 個 = メッセージ 1 件（HTTP リクエスト 1 回）。リアルタイムモードでは呼び出し元が
// 必要な件数をそのまま渡し、まとめモードではまとまった脆弱性 1 バッチがメッセージ 1 件なので 1 を渡す。
//
// バケット容量はそのチャネルの 1 分あたり上限で、一定速率で補充される。ratePerMin<=0 は
// レート制限なしを意味し、有限だが十分大きい値を返して、1 ラウンドのループが無限の滞留に
// 引きずられないようにする。
//
// want という上限は必須である：これがないとバケットを丸ごと空にするしかなく、しかも呼び出し元
// 自身にもラウンド上限があるため、余分に取ったトークンは使われないまま次の補充までに消える——
// 貯めておいたバースト容量には永遠に到達できず、「このラウンドに配信待ちが 1 件もない」場合でも
// 律儀に差し引かれてしまう。
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// 経過した実時間で補充する。速率は ratePerMin/60 毎秒。
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// ごく小さなイプシロンを足してから整数化する：トークン数は浮動小数点の累積なので、
	// 2 回に分けて満杯になる場合、0.5 + 0.5 が 0.9999999999 になり得る。そのまま int() すると
	// 0 に切り捨てられ、数学的には満杯のバケットからトークンが取れなくなる。1e-9 は 1 トークンより
	// はるかに小さく、本当の不足を誤って通すことはない。
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled は全体スイッチを読み取る。
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL はバックリンク用の外部アドレスを、末尾のスラッシュを除いて返す。
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval はまとめ周期を返し、不正または未設定なら既定値にフォールバックする。
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot は配信に対応するイベントのスナップショットを解析する。
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("配信 %d のイベントスナップショットが空", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("配信 %d のイベントスナップショットの解析に失敗: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// イベント種別はイベント行のものを正とする。スナップショット内のものは旧バージョンが
		// 書いた可能性がある。
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
