package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 本ファイルは通知配信機能のエンドツーエンド挙動をカバーする：脆弱性の保存 → イベント → 分派 → 実際の HTTP 送信。
//
// セキュリティ上の注意点が 1 つある：これらのケースは**グローバルの Notifier.step() を呼ばない**。
// 自分で作成したチャネルに対してのみ stepRealtime/stepDigest を呼ぶ。step() は DB 内の有効な
// 全チャネルを走査するため——実際の DingTalk/WeCom ボットが設定済みの開発 DB でテストを走らせると、
// グローバルの step はテスト中に生成された脆弱性を本当にそれらのグループへ配信してしまう。
// チャネル単位で呼ぶことで、影響範囲をテスト自作の偽受信端だけに厳密に限定できる。
//
// クリーンアップ：ケース終了時に、このケースが生成したイベント（配信はカスケード削除）と
// チャネルを削除し、実チャネルにバックログを残さない。
//
// アサーションの方針：stepRealtime/stepDigest は戻り値を返さず内部でログを記録するため、
// ここでアサートするのは**観測可能な外部挙動**（偽受信端が何を受け取ったか、配信行が
// どの状態になったか）であり、関数の戻り値ではない——戻り値をスタブ化するより実呼び出し
// 経路に近い。

// notifyFixture は本ファイルのケース共通の装置。
type notifyFixture struct {
	s       *Server
	pg      *db.DB
	request func(method, path, body string) *httptest.ResponseRecorder
	n       *Notifier
	// 自前で作る task/exploration：ケースは脆弱性をここに記録し、他のケースのデータと隔離する。
	taskID int64
	expID  int64
	// cleanupMark より後に生成されたイベントはクリーンアップ時にまとめて削除する。
	cleanupMark int64
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	// 本ファイルの偽受信端はすべて 127.0.0.1 上で動くが、配信は既定でループバック
	// アドレスを拒否する（同一ホスト上のサービスやクラウドメタデータへの SSRF を防ぐため）。
	// テストではこのスイッチを明示的に有効化する。「既定で拒否」のガード自体の挙動は
	// notify パッケージの ssrf_test.go がカバーする。
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg

	// 自前で task を作る：共有装置 trafficEvidenceServer が作る task では exploration id が
	// 取得できず、脆弱性の記録にはそれが必須だから。
	task, err := s.m.CreateTask("通知配信テスト", "配信挙動の検証", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Exec(`DELETE FROM tasks WHERE id=$1`, taskID) })

	var mark int64
	if err := pg.QueryRow(`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	// 装置を自己完結させる：fixture 作成前に存在していたイベントを一括で分派済みにする。
	//
	// どうして必須か：FanOutPendingEvents は**グローバル**で、DB 内のすべての未分派イベントを
	// すべての一致チャネルへ展開する。共有装置 trafficEvidenceServer 自身も 1 件の脆弱性を
	// 記録し（それこそが返す初期 finding）、他のケースにも残留があるかもしれない。隔離しないと
	// これらの雑多なイベントが本ケースのチャネルへ分派され、「N 件の配信があるはず」といった
	// アサーションが実行順序によって成否が変わってしまう——しかもその壊れ方は直接失敗より
	// 追跡が難しい。
	if _, err := pg.Exec(`UPDATE notification_events SET fanned_out = true WHERE id <= $1 AND NOT fanned_out`, mark); err != nil {
		t.Fatal(err)
	}

	f := &notifyFixture{s: s, pg: pg, request: request, n: newNotifier(s), taskID: taskID, expID: task.ExpID, cleanupMark: mark}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1`, f.cleanupMark); err != nil {
			t.Logf("通知イベントのクリーンアップに失敗: %v", err)
		}
	})
	// マスタースイッチは有効でなければならない（他のケースが無効化している可能性がある）。
	if err := pg.SetBool(settingNotifyEnabled, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// record は実際の証拠書き込み経路を通して脆弱性を 1 件保存し、finding id を返す。
// この経路は**同一トランザクション**内で配信イベントを登録する——まさに本機能のフック点。
func (f *notifyFixture) record(t *testing.T, vulnclass, severity string) int64 {
	t.Helper()
	out, err := f.s.evidenceStore().Record(context.Background(), db.RecordFindingInput{
		TaskID:        f.taskID,
		ExplorationID: f.expID,
		Worker:        "test",
		VulnClass:     vulnclass,
		Name:          vulnclass,
		Severity:      severity,
		Summary:       vulnclass + " のサマリー",
		Evidence:      "poc",
	}, nil)
	if err != nil {
		t.Fatalf("脆弱性の記録に失敗: %v", err)
	}
	return out.FindingID
}

// channel はチャネル設定を読み戻す（チャネル単位の stepX 呼び出しに使う）。
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("チャネルの読み取りに失敗: %v", err)
	}
	return ch
}

// deliver はイベントを分派し、指定チャネルに対してのみ 1 ラウンド配信する。
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("分派に失敗: %v", err)
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel は HTTP インターフェース経由でチャネルを作り、インターフェース自身の
// 検証経路も併せてカバーする。
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("チャネル作成に失敗 %d: %s", r.Code, r.Body)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("チャネル作成の戻り値が異常: %s (%v)", r.Body, err)
	}
	t.Cleanup(func() { f.pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, res.ID) })
	return res.ID
}

// fakeWebhook は受け取ったリクエストボディを記録する偽受信端。
type fakeWebhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	t.Helper()
	f := &fakeWebhook{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeWebhook) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("偽受信端は %d 件のリクエストしか受け取っておらず、%d 件目を取得できない", len(f.bodies), i)
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("偽受信端はリクエストを 1 件も受け取っていない")
	}
	return f.body(t, f.count()-1)
}

// markdownText はリクエストボディから本文を取り出す。各社のフィールド名の違いに対応する：
// DingTalk の markdown は `text`、ActionCard は `text`、WeCom の markdown は `content`。
func markdownText(t *testing.T, body map[string]any) string {
	t.Helper()
	for _, key := range []string{"markdown", "actionCard"} {
		section, ok := body[key].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"text", "content"} {
			if s, ok := section[field].(string); ok && s != "" {
				return s
			}
		}
	}
	t.Fatalf("リクエストボディに認識可能な本文がない: %v", body)
	return ""
}

// agePendingBatch はそのチャネルの未送信配信を古くし、まとめバッチの期限切れをテストする。
func (f *notifyFixture) agePendingBatch(t *testing.T, chID int64) {
	t.Helper()
	if _, err := f.pg.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '2 hours'
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEndToEndRealtimeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "リアルタイム配信",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQLインジェクション", "high")
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("メッセージは 1 件送られるはずが、実際は %d 件", hook.count())
	}
	text := markdownText(t, hook.last(t))
	for _, want := range []string{"SQLインジェクション", "🟠 高", "サマリー"} {
		if !strings.Contains(text, want) {
			t.Fatalf("メッセージ本文に %q がない:\n%s", want, text)
		}
	}
	// 配信は sent に遷移するはず。
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("配信後も sent にマークされていない配信が %d 件ある", pending)
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "マスクの用例",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("チャネル一覧の取得に失敗 %d: %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("インターフェースの応答が資格情報を漏らしている: %s", r.Body)
	}
	var listed struct {
		Channels []struct {
			ID         int64          `json:"id"`
			Config     map[string]any `json:"config"`
			SecretKeys []string       `json:"secret_keys"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var mine *struct {
		ID         int64          `json:"id"`
		Config     map[string]any `json:"config"`
		SecretKeys []string       `json:"secret_keys"`
	}
	for i := range listed.Channels {
		if listed.Channels[i].ID == chID {
			mine = &listed.Channels[i]
		}
	}
	if mine == nil {
		t.Fatal("作成したチャネルが一覧に現れていない")
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("資格情報フィールドはマスク値であるべき: %v", mine.Config)
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("インターフェースはどのフィールドが資格情報かをフロントエンドに伝えるべき")
	}

	// PATCH で名前だけ変更し、マスク済み資格情報を送り返す：実際の資格情報はそのまま保持されなければならない。
	body, _ := json.Marshal(map[string]any{
		"name":   "名称変更後",
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("更新に失敗 %d: %s", r.Code, r.Body)
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("マスク値の送り返しが実際の資格情報を上書きした: %v", cfg["webhook"])
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("マスク値の送り返しが secret を上書きした: %v", cfg["secret"])
	}
	if f.channel(t, chID).Name != "名称変更後" {
		t.Fatal("名前が更新されていない")
	}

	// secret を明示的に空にした場合は反映されるべき（「マスク値を送り返す=変更しない」とは異なる）。
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("secret のクリアに失敗 %d: %s", r.Code, r.Body)
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("空文字列で secret がクリアされるべき")
	}
}

func (f *notifyFixture) channelConfig(t *testing.T, id int64) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(f.channel(t, id).Config, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNotifyChannelAPICreateValidation(t *testing.T) {
	f := newNotifyFixture(t)
	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		{"種別が不正", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "チャネル種別が不正です"},
		{"名前がない", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "チャネル名がありません"},
		{"webhook がない", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "Webhook"},
		{"webhook のプロトコルが不正", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "Webhook"},
		{"モードが不正", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "通知モードが不正です"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("400 を返すべきだが %d だった: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("エラーメッセージに %q が含まれるべきだが、実際は %s", tc.wantSub, r.Body)
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("存在しないチャネルの削除は 404 を返すべきだが %d だった", r.Code)
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "深刻のみ",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "低リスクの問題", "low")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("しきい値未満の脆弱性は配信を生成すべきでないが、%d 件あった", n)
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("フィルタされた脆弱性はメッセージを送るべきでない")
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "まとめ配信",
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("集約脆弱性%d", i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// 未到来：送らない。
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("まとめバッチが期限前なのに送信された")
	}

	// バッチを古くした後：3 件を 1 件のメッセージにまとめる。
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("3 件は 1 件のメッセージにまとまるべきだが、実際は %d 件送信された", got)
	}
	text := markdownText(t, hook.last(t))
	if !strings.Contains(text, "直近") || !strings.Contains(text, "3 件の脆弱性") {
		t.Fatalf("まとめメッセージに件数/時間窓の文言がない:\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("集約脆弱性%d", i)) {
			t.Fatalf("まとめメッセージに %d 件目がない:\n%s", i, text)
		}
	}
	// 同一バッチは batch_id を共有するはず。
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("3 件の配信は 1 つの batch_id を共有すべきだが、distinct=%d total=%d", distinct, total)
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "停止中のチャネル",
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "停止期間中の脆弱性", "critical")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("停止中のチャネルは配信を生成すべきでないが、%d 件あった", n)
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "ステータス変更購読",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "ステータス変更の用例", "high")
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("ステータス変更に失敗 %d: %s", r.Code, r.Body)
	}
	f.deliver(t, chID, "")

	// 2 件あるはず：fixed の方がステータス変更であり、finding_created の方は同じラウンドで
	// 送られる可能性がある。ステータス変更は実際にはより後に作成されるが、順序に依存せず全件を探す。
	found := false
	for i := 0; i < hook.count(); i++ {
		text := markdownText(t, hook.body(t, i))
		if strings.Contains(text, "ステータス変更") && strings.Contains(text, "修正済み") {
			found = true
		}
	}
	if !found {
		t.Fatalf("「ステータス変更 → 修正済み」を含むメッセージを受け取っていない（全 %d 件）", hook.count())
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "ステータス変更を購読しない",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "変更を購読しない", "high")
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("ステータス変更に失敗 %d: %s", r.Code, r.Body)
	}
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
WHERE d.channel_id=$1 AND e.kind=$2`, chID, notify.EventFindingStatusChanged).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("ステータス変更を購読していないチャネルは変更通知を受け取るべきでないが、%d 件あった", n)
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "テスト送信",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("テスト送信に失敗 %d: %s", r.Code, r.Body)
	}
	if hook.count() != 1 {
		t.Fatalf("偽受信端はテストメッセージを 1 件受け取るべきだが、%d 件だった", hook.count())
	}
	// テストメッセージは一目でテストだと分かるものでなければならず、本物の脆弱性と誤認されてはならない。
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "テスト") {
		t.Fatalf("テストメッセージはテストであることを明示すべき: %s", text)
	}
	// 設定が壊れている場合は、チャネルの元のエラーをそのままユーザーに返すべき。
	badID := f.createChannel(t, map[string]any{
		"name":   "壊れたアドレス",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("配信失敗は 502 を返すべきだが %d だった: %s", r.Code, r.Body)
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// 必ず失敗するアドレスを指し、failed 配信を生成する。
	chID := f.createChannel(t, map[string]any{
		"name":   "失敗リトライ",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "失敗する配信", "high")
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)
	// リトライ予算を使い切るまで配信し続ける。
	for i := 0; i < db.MaxNotifyAttempts; i++ {
		f.n.stepRealtime(ctx, ch, 50, "")
		if _, err := f.pg.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, chID); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.pg.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStateFailed {
		t.Fatalf("リトライを使い切った後は failed であるべきだが %s だった", state)
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("履歴の取得に失敗 %d: %s", r.Code, r.Body)
	}
	var hist struct {
		Deliveries []struct {
			ID        int64  `json:"id"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
			Attempts  int    `json:"attempts"`
			Title     string `json:"title"`
		} `json:"deliveries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Total != 1 || len(hist.Deliveries) != 1 {
		t.Fatalf("失敗配信が 1 件見つかるべきだが、total=%d len=%d", hist.Total, len(hist.Deliveries))
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("履歴に失敗理由が含まれるべき。さもないとユーザーが調査できない")
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("試行回数が記録されるべきだが %d だった", hist.Deliveries[0].Attempts)
	}
	if hist.Deliveries[0].Title != "失敗する配信" {
		t.Fatalf("履歴に脆弱性タイトルが含まれるべきだが %q だった", hist.Deliveries[0].Title)
	}

	// 手動再送：pending に戻り、カウントがゼロになるべき。
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("再送に失敗 %d: %s", r.Code, r.Body)
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("再送後は pending かつ attempts=0 であるべきだが %s/%d だった", state, attempts)
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta の取得に失敗: %s", r.Body)
	}
	var meta struct {
		Kinds []struct {
			Kind       string   `json:"kind"`
			SecretKeys []string `json:"secret_keys"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Kinds) != len(notify.Kinds()) {
		t.Fatalf("meta は全 %d チャネルを列挙すべきだが %d 件だった", len(notify.Kinds()), len(meta.Kinds))
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("チャネル %s が資格情報フィールドを報告していない", k.Kind)
		}
	}

	// 3 つのグローバル設定のラウンドトリップ。末尾のスラッシュは正規化されて消えるべき。
	// そうしないとバックリンクが "//function/..." になってしまう。
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("設定の書き込みに失敗 %d: %s", r.Code, r.Body)
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("バックリンクアドレスが正規化されていない: %v", payload["notify_public_base_url"])
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("まとめ周期が反映されていない: %v", payload["notify_digest_interval_min"])
	}

	// 不正な値は拒否されるべき。
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s は 400 を返すべきだが %d だった", body, r.Code)
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL はバックリンクの組み立てをカバーする：public_base_url が
// 設定されている場合、単発メッセージはボタン付きの ActionCard を使い、リンクは脆弱性詳細ページを指す。
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "バックリンク",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "バックリンク付きの脆弱性", "high")
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("バックリンクがある場合は ActionCard を使うべきだが msgtype=%v だった", body["msgtype"])
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("バックリンクが違う\n期待値 %s\n実際の値 %v", want, card["singleURL"])
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL は逆方向をカバーする：外部アドレスが未設定のときは
// 壊れたリンク（localhost や相対パスを指すなど）を生成せず、純粋な markdown に戻るべき。
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "バックリンクなし",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "バックリンクなしの脆弱性", "high")
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("外部アドレス未設定のときは markdown を送るべきだが %v だった", body["msgtype"])
	}
	if text := markdownText(t, body); strings.Contains(text, "詳細を見る") {
		t.Fatalf("外部アドレス未設定のときは詳細リンクが現れるべきでない:\n%s", text)
	}
}

// TestNotifyDigestSegmentsAndDefersRemainder は「静かな消失」修正のエンドツーエンド証拠。
//
// まとめメッセージはチャネルの長さ上限（WeCom は 4096 バイト）に縛られ、1 バッチが収まらない
// ときは**メッセージ単位**で分割しなければならない：このメッセージに収まった項目は送信済みと
// マークし、残りはキューに戻して次のメッセージを待つ。かつての実装はバッチ全体を成功として
// マークしていた——切り捨てられた項目はメッセージにも失敗リストにもなく、配信履歴は成功と
// 表示され、脆弱性はそのまま消えていた。
//
// 4 つのことをアサートする：① 実際に収まった件数だけが送信済みになる ② 残りは未送信のまま
// ③ 延期された項目はリトライ回数を**消費しない** ④ もう 1 ラウンド走らせれば残りを送り切れる
// （デッドロックしない）。
func TestNotifyDigestSegmentsAndDefersRemainder(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	// WeCom を使う：markdown の上限は 4096 バイトで、6 チャネルの中で最も厳しい。
	chID := f.createChannel(t, map[string]any{
		"name":   "分割まとめ",
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// タイトルを長めにして、60 件が 4096 バイトを大きく超え、必ず分割されるようにする。
	longName := strings.Repeat("非常に長い脆弱性名称", 6)
	for i := 0; i < total; i++ {
		f.record(t, longName+strconv.Itoa(i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	f.agePendingBatch(t, chID)
	ch := f.channel(t, chID)

	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 1 {
		t.Fatalf("メッセージは 1 件だけ送られるべきだが %d 件だった", hook.count())
	}

	var sent, pending int
	if err := f.pg.QueryRow(`SELECT
    count(*) FILTER (WHERE state=$2),
    count(*) FILTER (WHERE state=$3)
  FROM notification_deliveries WHERE channel_id=$1`, chID, db.NotifyStateSent, db.NotifyStatePending).
		Scan(&sent, &pending); err != nil {
		t.Fatal(err)
	}
	if sent == 0 {
		t.Fatal("送信済みとマークされる項目があるはず")
	}
	if pending == 0 {
		t.Fatalf("%d 件のバッチが 4096 バイトに全収まるはずがない。残りが未送信であるべき；sent=%d", total, sent)
	}
	if sent+pending != total {
		t.Fatalf("件数が合わない：sent=%d pending=%d total=%d（送信も未送信もされていない=消失）", sent, pending, total)
	}
	// メッセージ本文は、まだ何件がこのメッセージに含まれていないかを正直に伝えなければならない。
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "残り") {
		t.Fatalf("メッセージは未包含の項目があることを説明すべき:\n%.400s", text)
	}

	// 延期された項目はリトライ予算を消費してはならない：取得時に attempts は楽観的に +1 され、
	// 延期時に減らし戻す必要がある。
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("延期された項目はリトライ回数を消費すべきでない（さもないと数件で失敗扱いになる）が attempts=%d だった", maxAttempts)
	}

	// 収束するまで繰り返し実行する。アサートするのは**最終的に全部送信済み**になり、かつ
	// 途中で実際に複数ラウンドに分かれたこと——「2 ラウンド目で送り切った」より強い：分割が
	// デッドロックせず、残り項目も失わないことを証明する。
	rounds := 0
	for {
		var undelivered int
		if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries
WHERE channel_id=$1 AND state <> $2 AND state <> $3`, chID, db.NotifyStateSent, db.NotifyStateFailed).
			Scan(&undelivered); err != nil {
			t.Fatal(err)
		}
		if undelivered == 0 {
			break
		}
		rounds++
		if rounds > total+5 {
			t.Fatalf("分割配信が収束しない：%d ラウンド走っても %d 件が未決着", rounds, undelivered)
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("%d ラウンド目に進展がなく、残り %d 件が永久に詰まる", rounds, undelivered)
		}
	}
	if rounds < 2 {
		t.Fatalf("4096 バイトのメッセージに長タイトルの脆弱性 %d 件は収まらないはずで、複数ラウンドに分かれるべきだが、実際は %d ラウンドだけだった", total, rounds)
	}
	// 初回以降の各ラウンドは**純粋な続き送信**であり、チャネルに拒否された項目は存在しないはず。
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("偽受信端は常に成功を返すため失敗項目はないはずだが %d 件あった", failed)
	}
}

// TestNotifyBackoffTableMatchesAttemptBudget はドリフト防止のアサーション。
//
// リトライ予算（db.MaxNotifyAttempts）とバックオフ序列表（notifyBackoff）は別々のパッケージに
// 分かれている：前者は状態機械のポリシー、後者はエンジンの実行テンポだ。片方だけを変えると
// ——たとえば予算を 5 回に上げてバックオフ段を追加し忘れると——コードはエラーにならず、
// 4・5 回目のリトライが最後の段の間隔を流用するだけになり、「リトライのテンポが妙に遅くなった」
// という形で現れる。調査時にここを疑うのは難しい。両者の長さが一致することをアサートし、
// この種のドリフトを CI で露呈させる。
func TestNotifyBackoffTableMatchesAttemptBudget(t *testing.T) {
	if len(notifyBackoff) != db.MaxNotifyAttempts {
		t.Fatalf("バックオフ段数(%d)と最大試行回数(%d)が一致しない——片方を変えたらもう片方も同時に変えなければならない",
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// バックオフ間隔は単調非減少でなければならない。そうでないとリトライがだんだん慌ただしくなり、
	// かえってレート制限を悪化させる。
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("バックオフ間隔は単調非減少でなければならない：第 %d 段 %v < 第 %d 段 %v",
				i, notifyBackoff[i], i-1, notifyBackoff[i-1])
		}
	}
}

// TestNotifyRateLimitDoesNotConsumeRetryBudget は「先にトークンを取り、その後で取得する」順序を固定する。
// 逆（先に取得してから破棄）だと、レート制限で弾かれた配信はすでに attempts を 1 回消費しており、
// 予算が純粋な待機で使い果たされ、最後には failed に落ちる。
func TestNotifyRateLimitDoesNotConsumeRetryBudget(t *testing.T) {
	// トークンバケット自体だけをテストする。Server は不要（作るべきでもない）。
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 毎分 1 件：満タンなら最大 1 件。
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now); got != 1 {
		t.Fatalf("満タンで毎分 1 件ならトークンは 1 個取れるはずだが %d だった", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("トークン枯渇後は直ちに 0 を返すべきだが %d だった", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("半周期ではトークン 1 個分も補充されないはずだが %d だった", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("1 周期経過でトークンが 1 個補充されるべきだが %d だった", got)
	}
	// レート制限なしのチャネルも有限の上限で動き、1 ラウンドが無限のバックログに引きずられないようにする。
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("レート制限なしでは毎ラウンド上限 %d を返すべきだが %d だった", notifyUnlimitedBurstPerTick, got)
	}
	// チャネル間のトークンバケットは互いに独立。
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("チャネル 1 のバケットはまだ空のはずだが %d だった", got)
	}
}

// TestNotifyTakeTokensKeepsUnusedTokens は「want 個だけ取る」セマンティクスを固定する。
//
// かつての実装はバケットを丸ごと空にしてから呼び出し元が切り詰めていたため、rate=100/min の
// チャネルが満タンでも 1 ラウンドで 5 件しか使わず、残り 95 個のトークンをそのまま捨てていた。
// そのラウンドに未送信配信がない場合も同様に減っていた。結果として、コメントが主張する
// 「バックログ時に rate_per_min 件を一気に送れる」がどの状況でも実現できなかった。
func TestNotifyTakeTokensKeepsUnusedTokens(t *testing.T) {
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// バケットは初期状態で満タン（100）で、今回は 5 個だけ必要。
	if got := n.takeTokens(1, 100, 5, now); got != 5 {
		t.Fatalf("want=5 のときはちょうど 5 個取るべきだが %d だった", got)
	}
	// 重要なアサーション：残り 95 個はバケットに残っていなければならず、空にされて捨てられてはならない。
	// 時間を進めず、取得分が補充ではなく残量から来ることを保証する。
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("残りトークンはまだ取れるはず（期待 95）だが %d——バケットがラウンドごと空にされた", got)
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("バケットは取り尽くしているので 0 を返すべきだが %d だった", got)
	}
	// want<=0 はトークンを減らさない（空のラウンドは無料）。
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0 は 0 を返すべきだが %d だった", got)
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("want=0 の回はトークンを消費すべきでなく、満タンの 20 個が取れるべきだが %d だった", got)
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget はまとめモードの 2 つの量の次元を固定する。
//
// まとめバッチのサイズが毎ラウンドのリクエスト予算に結び付くと、rate_per_min=20 のチャネルは
// 3 秒 tick ごとにトークン 1 個しか補充されず、まとめメッセージ 1 件に脆弱性 1 件しか入らない——
// 機能的にはまとめがないのと同じで、しかもメッセージ冒頭には「直近 30 分間に 1 件の脆弱性を検出」と
// 書かれる。この劣化はエラーにならず、既存のエンドツーエンドケースでも見えない（手動で stepDigest に
// 十分大きい limit を渡し、step 内の枠計算を迂回しているため）ので、ここで決定そのものをアサートする。
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// 1 バッチ = 1 メッセージ = 1 リクエスト = 1 トークン。トークンの単位はメッセージであり、脆弱性ではない。
	if tokens != 1 {
		t.Fatalf("まとめバッチはメッセージ 1 件だけを送るので、トークンはちょうど 1 個消費すべきだが %d だった", tokens)
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("まとめバッチサイズはメモリ上の上限 db.MaxDigestBatchSize=%d であるべきだが %d だった",
			db.MaxDigestBatchSize, claimLimit)
	}
	// 重要な関係：バッチサイズは毎ラウンドのリクエスト予算よりはるかに大きくなければならない。
	// 両者が同程度になったら、「何件のメッセージを送るか」と「1 バッチに何件の脆弱性を入れるか」を
	// また 1 つの数に混ぜてしまったということ。
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("まとめバッチサイズ %d は毎ラウンドのリクエスト予算 %d に縛られるべきでない——"+
			"リクエスト予算はリースから逆算した「何回リクエストを送るか」であり、「1 バッチに何件の脆弱性を入れるか」とは別の量の次元",
			claimLimit, notifyMaxSendsPerChannelPerTick)
	}
}

// TestNotifyTickBudgetFitsWithinLease ももう 1 つのドリフト防止アサーション。
//
// 単一チャネルの毎ラウンド配信件数上限（notifyMaxSendsPerChannelPerTick）はリース時間から
// 逆算されている：1 ラウンド内で直列に配信する最悪所要時間はリース未満でなければならず、
// そうでないと後ろの数件が送り終わる前にリースが切れ、マルチインスタンス構成では相手側が
// それらを再取得して重複送信する。この 3 つの定数は別々の場所にあり、どれを変えても関係が
// 壊れうるのにエラーは出ない——だからここで固定する。
func TestNotifyTickBudgetFitsWithinLease(t *testing.T) {
	worst := time.Duration(notifyMaxSendsPerChannelPerTick) * notifySendTimeout
	if worst >= notifyLease {
		t.Fatalf("単一チャネルの 1 ラウンドの最悪所要時間 %v はリース %v に達してはならない"+
			"（notifyMaxSendsPerChannelPerTick=%d × notifySendTimeout=%v）——"+
			"この 3 つの定数のどれか 1 つを変えたら、残り 2 つも必ず確認すること",
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}
