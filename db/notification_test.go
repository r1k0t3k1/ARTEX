package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// 本ファイルのテストはすべて実際に PostgreSQL へ接続する（DB がない場合はスキップ）。これらの SQL は
// FOR UPDATE SKIP LOCKED、make_interval、JSONB、複数行 IN(...) のプレースホルダ連結を使っており、
// いずれも「コンパイルは通るが実行時に落ち得る」書き方なので、実走させて初めて検証したことになる。

func notifyTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestChannel はチャネルを 1 つ作成し、テスト終了時に自動削除する。
func newTestChannel(t *testing.T, d *DB, kind, mode string, filter string) *NotificationChannel {
	t.Helper()
	if filter == "" {
		filter = `{}`
	}
	ch := &NotificationChannel{
		Name:       "テストチャネル-" + t.Name(),
		Kind:       kind,
		Mode:       mode,
		Config:     json.RawMessage(`{"webhook":"https://example.com/hook"}`),
		Filter:     json.RawMessage(filter),
		RatePerMin: 100,
	}
	id, err := d.SaveNotificationChannel(context.Background(), ch)
	if err != nil {
		t.Fatalf("チャネル作成に失敗: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	ch.ID = id
	return ch
}

// addTestEvent はイベントを 1 件直接書き込む（finding を経由しない）。分派と配信のテスト用。
func addTestEvent(t *testing.T, d *DB, kind string, findingID int64, snap notify.Snapshot) int64 {
	t.Helper()
	snap.Kind = kind
	snap.FindingID = findingID
	id, err := d.AddNotificationEvent(context.Background(), kind, findingID, snap)
	if err != nil {
		t.Fatalf("イベント書き込みに失敗: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE id=$1`, id) })
	return id
}

func TestNotificationAssetNamesResolvesAndPreservesOrder(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 3 種の資産はそれぞれ表示の基準が異なる：ドメイン、IP、URL。
	insertAsset := func(query, value string) int64 {
		t.Helper()
		var id int64
		if err := d.QueryRow(query, value).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	domID := insertAsset(`INSERT INTO assets(type, domain) VALUES('subdomain',$1) RETURNING id`, "a.example.com")
	ipID := insertAsset(`INSERT INTO assets(type, ip) VALUES('ip',$1) RETURNING id`, "10.1.2.3")
	svcID := insertAsset(`INSERT INTO assets(type, url) VALUES('service',$1) RETURNING id`, "https://a.example.com/admin")
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id IN ($1,$2,$3)`, domID, ipID, svcID)
	})

	// 渡す順序はわざとばらばらにし、存在しない id も 1 つ混ぜる。
	got, err := d.NotificationAssetNames(ctx, []int64{svcID, 999999999, domID, ipID, svcID})
	if err != nil {
		t.Fatalf("資産名の解決に失敗: %v", err)
	}
	want := []string{"https://a.example.com/admin", "a.example.com", "10.1.2.3"}
	if len(got) != len(want) {
		t.Fatalf("資産名の件数が不一致。期待 %v、実際 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("順序/値が不一致。期待 %v、実際 %v", want, got)
		}
	}
}

// TestRecordNotificationEventTxUnwindsOnFailure はセーブポイント機構の中核テスト：
// トランザクション内で notification_events への書き込みを必ず失敗させ（一時的に常に false の制約を追加）、
// ① この関数が false を返す ② トランザクションが aborted 状態にならず、後続の文を実行できる、を検証する。
//
// セーブポイントがなければ PostgreSQL はトランザクション全体を無効化し、後続のあらゆる文が
// "current transaction is aborted" で失敗する——まさに「通知テーブルの問題で
// 脆弱性が保存できなくなる」障害経路である。
//
// ここでは意図的に **COMMIT ではなく ROLLBACK で締めくくる**：PG では ALTER TABLE がトランザクション対象なので、
// コミットしてしまうと一時制約が schema に永久に残り、後続のテストをまとめて壊す。
// ロールバックなら DDL を自動で取り消せるため、手作業の後始末は不要。「トランザクションが生きている」ことだけを
// 検証すればよく、実際にコミットする必要はない。
func TestRecordNotificationEventTxUnwindsOnFailure(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 防御的な後始末：過去の実行がこの制約を残していたら、まず外しておく。
	if _, err := d.Exec(`ALTER TABLE notification_events DROP CONSTRAINT IF EXISTS notify_test_never`); err != nil {
		t.Fatal(err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // 一時制約を取り消す。関数のコメントを参照

	// NOT VALID：以降に書き込まれる行だけを制約し、既存の履歴イベントは検証しない
	// （さもないと存量行の違反で制約を追加できなくなる）。
	if _, err := tx.ExecContext(ctx, `ALTER TABLE notification_events ADD CONSTRAINT notify_test_never CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("一時制約の追加に失敗: %v", err)
	}
	if RecordNotificationEventTx(ctx, tx, notify.EventFindingCreated, 1, notify.Snapshot{Severity: "high"}) {
		t.Fatal("必ず失敗する制約下なのに書き込み成功を報告した")
	}
	// 重要な検証：トランザクションがまだ使えること。
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("トランザクションが汚染された（セーブポイントが効いていない）: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("ロールバックに失敗: %v", err)
	}
	// DDL がロールバックで取り消されたことを確認し、後続のテストに爆弾を残さない。
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='notify_test_never')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("一時制約がロールバックで取り消されておらず、後続のテストを汚染する")
	}
}

func TestFanOutRoutesEventsByFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	all := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	onlyCritical := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"min_severity":"critical"}`)
	sqlOnly := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["SQL"]}`)

	highSQL := addTestEvent(t, d, notify.EventFindingCreated, 1001, notify.Snapshot{Severity: "high", VulnClass: "SQLインジェクション"})
	lowXSS := addTestEvent(t, d, notify.EventFindingCreated, 1002, notify.Snapshot{Severity: "low", VulnClass: "XSS"})
	criticalXSS := addTestEvent(t, d, notify.EventFindingCreated, 1003, notify.Snapshot{Severity: "critical", VulnClass: "XSS"})

	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatalf("分派に失敗: %v", err)
	}

	cases := []struct {
		name    string
		eventID int64
		channel int64
		want    bool
	}{
		{"全件受信チャネルが high を受信", highSQL, all.ID, true},
		{"全件受信チャネルが low を受信", lowXSS, all.ID, true},
		{"重大のみのチャネルが high をスキップ", highSQL, onlyCritical.ID, false},
		{"重大のみのチャネルが critical を受信", criticalXSS, onlyCritical.ID, true},
		{"SQL のみのチャネルが SQL を受信", highSQL, sqlOnly.ID, true},
		{"SQL のみのチャネルが XSS をスキップ", lowXSS, sqlOnly.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exists bool
			if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE event_id=$1 AND channel_id=$2)`,
				tc.eventID, tc.channel).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tc.want {
				t.Fatalf("配信の有無: 期待 %v、実際 %v", tc.want, exists)
			}
		})
	}

	// もう一度分派しても重複配信は生じない（fanned_out の冪等性）。
	events, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("分派済みイベントは再処理されないはず。実際 events=%d deliveries=%d", events, deliveries)
	}
}

// TestFanOutMarksEventsWithNoMatchingChannel は「イベントがどのチャネルにも一致しなかった」ケースを扱う。
// こうしたイベントも分派済みとしてマークしなければ、未分派集合に永遠に残り、tick ごとに再走査されてしまう。
func TestFanOutMarksEventsWithNoMatchingChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	pick := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["絶対に一致しない種別"]}`)
	_ = pick

	ev := addTestEvent(t, d, notify.EventFindingCreated, 2001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	_, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("配信が生じてはいけないのに %d 件生じた", deliveries)
	}
	var fanned bool
	if err := d.QueryRowContext(ctx, `SELECT fanned_out FROM notification_events WHERE id=$1`, ev).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if !fanned {
		t.Fatal("どのチャネルにも一致しないイベントも分派済みにしなければ、無限に再走査される")
	}
}

func TestClaimRealtimeDeliveriesHonorsLeaseAndMode(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	realtime := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	digest := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)

	addTestEvent(t, d, notify.EventFindingCreated, 3001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// リアルタイム取得は realtime チャネルの行だけを取るべきで、digest チャネルの行には触れない。
	got, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatalf("取得に失敗: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("1 件取得するはずが %d 件だった", len(got))
	}
	if got[0].State != NotifyStateSending || got[0].Attempts != 1 {
		t.Fatalf("取得後は sending かつ attempts=1 のはず。実際 state=%s attempts=%d", got[0].State, got[0].Attempts)
	}
	// 関連ロードされるレンダリングコンテキストが揃っていること（チャネル設定 + イベントスナップショット + finding id）。
	if got[0].Channel == nil || len(got[0].Channel.Config) == 0 {
		t.Fatal("取得結果にチャネル設定がなく、レンダリングが失敗する")
	}
	if got[0].FindingID != 3001 {
		t.Fatalf("イベントから finding id が引き継がれていない。実際 %d", got[0].FindingID)
	}

	// リースが未失効なので 2 回目の取得は空になる——これが「同じ行を 2 つの dispatcher が
	// 同時に配信しない」ことの保証である。
	again, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("リース期間内に重複取得してはいけないのに %d 件取得した", len(again))
	}

	// digest チャネルの配信はリアルタイム取得に触れられてはいけない。
	left, err := d.ClaimRealtimeDeliveries(ctx, digest.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("リアルタイム取得が digest チャネルの配信を取ってはいけないのに %d 件取った", len(left))
	}
}

// TestClaimExpiredLeaseRecovers はクラッシュからの自己復旧を扱う：配信途中でプロセスが落ちると sending 行が
// 残るが、リース失効後は再取得できなければ、この配信は永遠に詰まってしまう。
func TestClaimExpiredLeaseRecovers(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 4001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	first, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("初回取得に失敗: %v（%d 件）", err, len(first))
	}
	// リースを手動で過去へ押し出し、「リース失効済み」を再現する。
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE id=$1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("リース失効した sending 行は再取得できるはずが %d 件だった", len(second))
	}
	if second[0].Attempts != 2 {
		t.Fatalf("再取得で試行回数が加算されるはずが %d だった", second[0].Attempts)
	}
}

func TestClaimSkipsDisabledChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 5001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// 無効化すると既存の送信待ち配信もまとめて skipped になる。
	if err := d.SetNotificationChannelEnabled(ctx, ch.ID, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateSkipped {
		t.Fatalf("無効化したチャネルの既存送信待ち配信は skipped になるはずが %s だった", state)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("無効化済みチャネルから取得できてはいけないのに %d 件取れた", len(got))
	}
}

func TestDigestBatchDueAndStableBatchID(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 3; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(6000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// バッチを作成した直後で経過時間は 0 なので、30 分周期ではまだ期限が来ないはず。
	due, err := d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("バッチ期限の判定に失敗: %v", err)
	}
	if due {
		t.Fatal("作成直後のバッチはすぐには期限が来ないはず")
	}

	// 3 件の配信の作成時刻をまとめて古くし、周期分を蓄積したバッチを再現する。
	if _, err := d.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '40 minutes' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	due, err = d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("周期を超えたバッチは期限到来と判定されるべき")
	}

	batch, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatalf("ダイジェストバッチの取得に失敗: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("ダイジェストは 3 件すべてを一度に取得するはずが %d 件だった", len(batch))
	}
	if batch[0].BatchID == nil {
		t.Fatal("ダイジェストバッチは batch_id を書かねばならず、さもないと履歴で一緒に送った事実が分からない")
	}
	firstBatchID := *batch[0].BatchID
	for _, dl := range batch {
		if dl.BatchID == nil || *dl.BatchID != firstBatchID {
			t.Fatalf("同一バッチは batch_id を共有するはず。実際 %v と %d", dl.BatchID, firstBatchID)
		}
	}

	// このバッチを**丸ごと**失敗させて再スケジュールしてから再取得しても、batch_id は元の値を保たねばならない（COALESCE の役割）：
	// さもないと 1 回の再試行で「このバッチは一緒に送った」という事実が消えてしまう。
	//
	// 1 件だけではなくバッチ全体を再スケジュールする必要がある——配信エンジンがダイジェストを送るときもそう扱う
	// （1 通のメッセージがバッチ全体を表し、成否を共にする）。1 件だけ再スケジュールすると、残りはまだリース期間内なので、
	// 再取得では当然その 1 件しか取れない。
	allIDs := make([]int64, 0, len(batch))
	for _, dl := range batch {
		allIDs = append(allIDs, dl.ID)
	}
	if err := d.RescheduleDeliveries(ctx, allIDs, time.Second, "疑似失敗"); err != nil {
		t.Fatal(err)
	}
	// リースを過去へ押し出し、バックオフ時間が経過した状態を再現する。
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("再取得で 3 件すべて取れるはずが %d 件だった", len(reclaimed))
	}
	if reclaimed[0].BatchID == nil || *reclaimed[0].BatchID != firstBatchID {
		t.Fatalf("再試行後も batch_id は元の値 %d を保つはずが %v だった", firstBatchID, reclaimed[0].BatchID)
	}
}

func TestDeliveryStateTransitions(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 7001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("取得に失敗: %v（%d）", err, len(got))
	}
	id := got[0].ID

	if err := d.RescheduleDeliveries(ctx, []int64{id}, time.Second, "ネットワーク揺らぎ"); err != nil {
		t.Fatal(err)
	}
	var state, lastErr string
	if err := d.QueryRow(`SELECT state, last_error FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || lastErr != "ネットワーク揺らぎ" {
		t.Fatalf("再スケジュール後は pending で理由が記録されるはず。実際 state=%s err=%q", state, lastErr)
	}

	if err := d.FailDeliveries(ctx, []int64{id}, "再試行を使い切った"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateFailed {
		t.Fatalf("failed になるはずが %s だった", state)
	}

	// 手動再送では再試行回数をリセットし、即座に取得可能にすべき。さもないと古い失敗予算を引き継いでしまう。
	if err := d.RetryNotificationDelivery(ctx, id); err != nil {
		t.Fatalf("再送に失敗: %v", err)
	}
	var attempts int
	var next time.Time
	if err := d.QueryRow(`SELECT state, attempts, next_attempt_at FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || attempts != 0 {
		t.Fatalf("再送後は pending かつ attempts=0 のはず。実際 state=%s attempts=%d", state, attempts)
	}
	if next.After(time.Now().Add(time.Second)) {
		t.Fatal("再送後は即座に取得できるべき")
	}

	// 送信済みの配信は再送できてはいけない。
	if err := d.MarkDeliveriesSent(ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryNotificationDelivery(ctx, id); err == nil {
		t.Fatal("送信済みの配信は再送を許可すべきではない")
	}
}

func TestListNotificationDeliveriesPagingAndFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	for i := 0; i < 5; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(8000+i), notify.Snapshot{Severity: "high", Name: "ページングテスト"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute); err != nil {
		t.Fatal(err)
	}

	page1, total, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 1, 2)
	if err != nil {
		t.Fatalf("クエリに失敗: %v", err)
	}
	if total != 5 {
		t.Fatalf("総数は 5 のはずが %d だった", total)
	}
	if len(page1) != 2 {
		t.Fatalf("1 ページ 2 件のはずが %d 件だった", len(page1))
	}
	// 新しいものが先頭：1 ページ目の先頭 id は 2 ページ目の先頭より大きいはず。
	page2, _, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID >= page1[0].ID {
		t.Fatalf("ページング順は新しいものが先頭のはず。実際 page1[0]=%d page2[0]=%d", page1[0].ID, page2[0].ID)
	}
	// レンダリングコンテキストは履歴とともに返さねばならない。さもないと一覧で「何を配信したか」を表示できない。
	if page1[0].ChannelName == "" || page1[0].FindingID == 0 {
		t.Fatalf("履歴項目に表示用フィールドがない: %+v", page1[0])
	}

	// 状態でフィルタ：pending は存在しない。
	pending, totalPending, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStatePending}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if totalPending != 0 || len(pending) != 0 {
		t.Fatalf("pending の配信は存在しないはずが %d 件あった（total=%d）", len(pending), totalPending)
	}
}

func TestSetFindingStatusWithNotifyOnlyEmitsOnRealChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("通知状態変更テスト", "目標", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQLインジェクション", Name: "状態変更のテストケース",
		Severity: "high", Summary: "要約",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 永続化時にすでに finding_created イベントが 1 件登録されているので、まずそれを数えて基準値にする。
	var base int
	if err := d.QueryRow(`SELECT count(*) FROM notification_events WHERE finding_id=$1`, f.FindingID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base < 1 {
		t.Fatal("脆弱性の永続化は同一トランザクションで配信イベントを 1 件登録するはず")
	}

	// 同じ状態に設定：イベントを生じさせない（再送信による通知ノイズを避ける）。
	from, found, notified, err := d.SetFindingStatusWithNotify(ctx, f.FindingID, "pending")
	if err != nil || !found {
		t.Fatalf("状態設定に失敗: found=%v err=%v", found, err)
	}
	if notified {
		t.Fatal("状態が変わっていないときは配信イベントを登録すべきでない")
	}
	if from != "pending" {
		t.Fatalf("変更前の状態 pending を返すはずが %q だった", from)
	}

	// 実際に変更：イベントを登録し、from/to を記録するはず。
	from, found, notified, err = d.SetFindingStatusWithNotify(ctx, f.FindingID, "fixed")
	if err != nil || !found {
		t.Fatalf("状態設定に失敗: found=%v err=%v", found, err)
	}
	if !notified {
		t.Fatal("状態が実際に変わったときは配信イベントを登録するはず")
	}
	if from != "pending" {
		t.Fatalf("from は pending のはずが %q だった", from)
	}
	var snapshot []byte
	if err := d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot); err != nil {
		t.Fatalf("状態変更イベントが見つからない: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != "fixed" {
		t.Fatalf("スナップショットの状態遷移が不正: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// スナップショットにはレンダリングに必要なフィールドを載せること。さもないと状態変更メッセージが空になる。
	if snap.VulnClass != "SQLインジェクション" || snap.Severity != "high" || snap.Name != "状態変更のテストケース" {
		t.Fatalf("スナップショットにレンダリング用フィールドがない: %+v", snap)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fixed" {
		t.Fatalf("状態は fixed に更新されているはずが %s だった", status)
	}

	// 存在しない脆弱性：found=false で、エラーは返さない。
	if _, found, _, err := d.SetFindingStatusWithNotify(ctx, 999999999, "fixed"); err != nil || found {
		t.Fatalf("存在しない脆弱性は found=false かつエラーなしのはず。実際 found=%v err=%v", found, err)
	}
}

func TestNotificationStatsSnapshot(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 9001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stats, err := d.NotificationStatsSnapshot(ctx)
	if err != nil {
		t.Fatalf("統計の取得に失敗: %v", err)
	}
	if stats.Channels < 1 || stats.ChannelsOn < 1 {
		t.Fatalf("チャネル数が不正: %+v", stats)
	}
	if stats.Pending < 1 {
		t.Fatalf("送信待ち配信が集計されるはず: %+v", stats)
	}
	// 作成直後の配信のバックログ経過時間は 0 に近いはずで、負値や巨大値であってはならない。
	if stats.BacklogAgeMS < 0 || stats.BacklogAgeMS > int64(time.Hour/time.Millisecond) {
		t.Fatalf("バックログ経過時間が不正: %d ms", stats.BacklogAgeMS)
	}
	_ = ch
}

func TestNotificationChannelCRUDRoundTrip(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	ch := &NotificationChannel{
		Name:       "CRUD ラウンドトリップ",
		Kind:       notify.KindEmail,
		Mode:       NotifyModeDigest,
		Config:     json.RawMessage(`{"host":"smtp.example.com","port":587,"from":"a@b.c","to":["x@y.z"]}`),
		Filter:     json.RawMessage(`{"min_severity":"medium","on_status_change":true}`),
		RatePerMin: 42,
	}
	id, err := d.SaveNotificationChannel(ctx, ch)
	if err != nil {
		t.Fatalf("新規作成に失敗: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })

	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatalf("読み取りに失敗: %v", err)
	}
	if got.Mode != NotifyModeDigest || got.RatePerMin != 42 || got.Name != "CRUD ラウンドトリップ" {
		t.Fatalf("ラウンドトリップでフィールドが不一致: %+v", got)
	}
	if !got.IsEnabled() {
		t.Fatal("既定では有効のはず")
	}
	var cfg map[string]any
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["host"] != "smtp.example.com" {
		t.Fatalf("設定が正しく永続化されていない: %v", cfg)
	}
	var filter notify.Filter
	if err := json.Unmarshal(got.Filter, &filter); err != nil {
		t.Fatal(err)
	}
	if filter.MinSeverity != "medium" || !filter.OnStatusChange {
		t.Fatalf("フィルタ条件が正しく永続化されていない: %+v", filter)
	}

	// 更新してから読み直す。
	got.Name = "名前を変えた"
	off := false
	got.Enabled = &off
	if _, err := d.SaveNotificationChannel(ctx, got); err != nil {
		t.Fatal(err)
	}
	after, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "名前を変えた" || after.IsEnabled() {
		t.Fatalf("更新が反映されていない: %+v", after)
	}

	// 削除後は黙って成功するのではなく「存在しない」と報告すべき。
	if err := d.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NotificationChannelByID(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("ErrNotificationChannelNotFound を期待したが %v だった", err)
	}
	if err := d.DeleteNotificationChannel(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("重複削除は存在しないと報告すべきが %v だった", err)
	}
}

// TestSaveNotificationChannelKeepsExplicitZeroRate はかつて書き間違えた箇所を固定する：
// **0 は正当な設定で「レート制限なし」を意味し、db 層が「未指定」と取り違えて既定値で上書きしてはならない**。
//
// 過去の不具合：SaveNotificationChannel に `if RatePerMin <= 0 { 既定値を使う }` と書かれていたため、
// ドキュメント、UI のヒント、takeTokens はすべて「0=レート制限なし」と解釈するのに、書き込み層だけが黙って
// 20（DingTalk/WeCom/Telegram）や 100（Feishu）に変えていた——操作者は制限を解除したつもりが実際は絞られており、
// しかも何の表示もなかった。「未指定」と「明示的な 0」の区別はリクエストボディでしか表現できないため、
// 既定値は server 層で埋め（notifyCreateChannel 参照）、db 層は保存するだけにした。
func TestSaveNotificationChannelKeepsExplicitZeroRate(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 明示的な 0（レート制限なし）：そのまま保存されねばならない。
	unlimited := &NotificationChannel{
		Name: "レート制限なし", Kind: notify.KindDingTalk, RatePerMin: 0,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	id, err := d.SaveNotificationChannel(ctx, unlimited)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RatePerMin != 0 {
		t.Fatalf("明示的な 0 はレート制限なしを意味し、そのまま保存されねばならないが %d だった", got.RatePerMin)
	}
	if got.Mode != NotifyModeRealtime {
		t.Fatalf("既定モードは realtime のはずが %s だった", got.Mode)
	}

	// 負値は不正な入力なので、黙って別の値に変えるのではなく拒否すべき。
	bad := &NotificationChannel{
		Name: "負のレート制限", Kind: notify.KindDingTalk, RatePerMin: -1,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	if _, err := d.SaveNotificationChannel(ctx, bad); err == nil {
		t.Fatal("負のレート制限は拒否されるべき")
	}
}

// TestDeleteChannelCascadesDeliveries は外部キーの挙動を固定する：チャネル削除後はその配信履歴も消える
// （設定がなくなれば履歴を解釈できない）が、イベント自体は残さねばならない——他のチャネルから参照されている可能性がある。
func TestDeleteChannelCascadesDeliveries(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	ev := addTestEvent(t, d, notify.EventFindingCreated, 9101, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("前提条件が成立しない：配信が生じていない")
	}
	if err := d.DeleteNotificationChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("チャネル削除後は配信がカスケード削除されるはずが、まだ %d 件残っている", after)
	}
	var evExists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_events WHERE id=$1)`, ev).Scan(&evExists); err != nil {
		t.Fatal(err)
	}
	if !evExists {
		t.Fatal("チャネル削除でイベント自体まで削除してはいけない")
	}
}

// TestClaimDigestBatchHonorsCallerLimit は監査で指摘された抜け穴を扱う：
// ダイジェストチャネルは以前トークンバケットを完全に迂回しており、allow が takeTokens で消費されても誰も使わず、
// rate_per_min が digest モードではまったく効いていなかった。現在は limit も制約に加わる。
func TestClaimDigestBatchHonorsCallerLimit(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 10; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(7000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// limit=3 を指定：3 件しか取得できず、残りは DB に残る。
	got, err := d.ClaimDigestBatch(ctx, ch.ID, 3, time.Minute)
	if err != nil {
		t.Fatalf("取得に失敗: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("呼び出し側のレート制限枠どおり 3 件だけ取得するはずが %d 件だった", len(got))
	}
	// limit=0 は今回の枠を使い切ったことを意味する：1 件も取得せず、エラーも返さないはず。
	if got, err := d.ClaimDigestBatch(ctx, ch.ID, 0, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("枠が 0 のときは 0 件取得してエラーなしのはず。実際 %d 件 err=%v", len(got), err)
	}
}

// TestFinishFindingRetestEmitsStatusChange は監査で指摘された完全性の欠落を扱う：
// 再検証の結論が「修正済み」のとき状態は確かに変わるが、その UPDATE は直接 DB へ書いており、
// 通知付きの版を迂回していた——そのため on_status_change を設定したチャネルはこの状態遷移を
// まったく受信できず、UI 上では状態がひっそり変わり、運用担当がプラットフォームを開いて初めて気づく状態だった。
//
// このテストは「状態を変更するすべての経路が状態変更イベントを登録すること」を固定する。
func TestFinishFindingRetestEmitsStatusChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("再検証配信テスト", "目標", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQLインジェクション", Name: "再検証の対象",
		Severity: "high", Summary: "要約",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 再検証レコードを 1 件作成し、そのまま完了状態へ進める。
	rt, _, _, err := d.CreateFindingRetest(ctx, f.FindingID, "再確認")
	if err != nil {
		t.Fatal(err)
	}
	if rt.ConversationID == nil {
		t.Fatal("再検証には会話が関連付けられるはず")
	}
	// 再検証は結論を記録する前に必ず running へ入る必要がある（実際のフローと同じ）。
	if ok, err := d.StartFindingRetest(ctx, rt.ID); err != nil || !ok {
		t.Fatalf("再検証の開始に失敗: ok=%v err=%v", ok, err)
	}
	if err := d.RecordFindingRetestResult(ctx, *rt.ConversationID, "fixed", "修正済み", "証拠"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(rt.ID, "completed", ""); err != nil {
		t.Fatalf("再検証の終了に失敗: %v", err)
	}

	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != FindingFixed {
		t.Fatalf("再検証で修正済みと判定した後の状態は fixed のはずが %s だった", status)
	}

	// 重要な検証：状態変更イベントが 1 件必要で、from/to が正しいこと。
	var snapshot []byte
	err = d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot)
	if err != nil {
		t.Fatalf("修正済み判定時は状態変更の配信イベントを登録するはず（さもないと on_status_change 設定済みのチャネルが受信できない）: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != FindingFixed {
		t.Fatalf("スナップショットの状態遷移が不正: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// スナップショットにはレンダリングに必要なフィールドを載せること。さもないと配信が空になる。
	if snap.Name != "再検証の対象" || snap.Severity != "high" {
		t.Fatalf("スナップショットにレンダリング用フィールドがない: %+v", snap)
	}
}
