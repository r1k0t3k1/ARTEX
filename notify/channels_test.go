package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// singleMsg は引用符と改行を含む単発メッセージを構築する。タイトル／サマリーに
// `"` と `\n` を意図的に入れてある：テンプレート補間が不正な JSON を生みやすい
// 典型的な入力だから。
func singleMsg() Message {
	return Message{
		Items: []Item{{
			FindingID: 42,
			Name:      `ログイン画面 "SQLインジェクション" リスク`,
			VulnClass: "SQLインジェクション",
			Severity:  "high",
			Summary:   "パラメータ id\n未検証 による注入",
			Assets:    []string{"a.example.com", "b.example.com"},
			DetailURL: "https://artex.local/function/findings/detail?id=42",
		}},
	}
}

// batchMsg はダイジェストメッセージを構築する。
func batchMsg(n int) Message {
	m := Message{Batch: true, WindowMinutes: 30, HomeURL: "https://artex.local/function/findings"}
	for i := 0; i < n; i++ {
		m.Items = append(m.Items, Item{
			FindingID: int64(i + 1),
			Name:      "脆弱性" + itoa(i+1),
			VulnClass: "XSS",
			Severity:  "medium",
			Summary:   "反射型クロスサイトスクリプティング",
			Assets:    []string{"target.example.com"},
		})
	}
	return m
}

// capturePost は偽の受信先を立ち上げ、受け取ったリクエストボディとヘッダーを
// 検証関数へ渡す。
func capturePost(t *testing.T, respBody string, assert func(t *testing.T, body map[string]any, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("リクエストボディが正当な JSON ではありません: %v\n原文: %s", err, raw)
			}
		}
		if assert != nil {
			assert(t, body, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDingTalkSendsActionCardWhenLinkPresent(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "actionCard" {
			t.Fatalf("リンクがある場合は actionCard を送信すべき、得られたのは %v", body["msgtype"])
		}
		card, _ := body["actionCard"].(map[string]any)
		if card["singleURL"] != "https://artex.local/function/findings/detail?id=42" {
			t.Errorf("リンクが失われた: %v", card["singleURL"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestDingTalkFallsBackToMarkdownForBatch(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "markdown" {
			t.Fatalf("ダイジェストは markdown で送信すべき、得られたのは %v", body["msgtype"])
		}
		md, _ := body["markdown"].(map[string]any)
		if !strings.Contains(md["text"].(string), "直近 30 分") {
			t.Errorf("ダイジェスト本文に時間枠がない: %v", md["text"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, batchMsg(3)); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

// TestDingTalkBusinessErrorIsPermanent は「HTTP 200 だが errcode が非 0」という
// 判定を固定する。errcode を確認しないと配信失敗を成功として記録してしまう——
// 中国の IM プラットフォームに共通する罠。
func TestDingTalkBusinessErrorIsPermanent(t *testing.T) {
	srv := capturePost(t, `{"errcode":310000,"errmsg":"keywords not in content"}`, nil)
	_, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg())
	if err == nil {
		t.Fatal("errcode が非 0 ならエラーを返すべき")
	}
	if !IsPermanent(err) {
		t.Fatalf("キーワード不一致は設定エラーなので永久失敗にすべき、得られたのは %v", err)
	}
	if !strings.Contains(err.Error(), "310000") {
		t.Errorf("エラーメッセージにプラットフォームのエラーコードを含めるべき、得られたのは %v", err)
	}
}

func TestWeComTruncatesCJKWithinByteLimit(t *testing.T) {
	var contentLen int
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		md, _ := body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		contentLen = len(content)
		if !utf8.ValidString(content) {
			t.Fatal("切り詰め後に正当な UTF-8 ではない——WeCom はメッセージ全体を拒否する")
		}
	})
	// 4096 バイトを確実に超える長さの日本語ダイジェストを生成する。
	m := batchMsg(200)
	if _, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, m); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
	if contentLen > weComMarkdownLimit {
		t.Fatalf("本文 %d バイトが WeCom の上限 %d を超過", contentLen, weComMarkdownLimit)
	}
	if contentLen == 0 {
		t.Fatal("本文が空")
	}
}

func TestWeComRateLimitIsRetryableButKeyErrorIsPermanent(t *testing.T) {
	limited := capturePost(t, `{"errcode":45009,"errmsg":"api freq out of limit"}`, nil)
	_, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": limited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("45009 はローリングウィンドウのレート制限で、再試行すべき、得られたのは %v", err)
	}

	badKey := capturePost(t, `{"errcode":93000,"errmsg":"invalid webhook url"}`, nil)
	_, err = (weComChannel{}).Send(context.Background(), map[string]any{"webhook": badKey.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("93000 は key 無効で、再試行しても回復しないため永久失敗にすべき、得られたのは %v", err)
	}
}

func TestFeishuCardStructureAndSign(t *testing.T) {
	const secret = "SECtest123"
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msg_type"] != "interactive" {
			t.Fatalf("インタラクティブカードを送信すべき、得られたのは %v", body["msg_type"])
		}
		card, _ := body["card"].(map[string]any)
		header, _ := card["header"].(map[string]any)
		if header["template"] != "orange" {
			t.Errorf("high レベルは orange 配色にすべき、得られたのは %v", header["template"])
		}
		// secret が設定されていれば署名パラメーターを必ず付ける。さもないと Lark は
		// 19021 で拒否する。
		if body["sign"] == nil || body["timestamp"] == nil {
			t.Fatalf("署名パラメーターがない: %v", body)
		}
		// カード要素にはボタンが 1 つあり、その url が脆弱性の詳細を指すべき。
		elements, _ := card["elements"].([]any)
		foundButton := false
		for _, e := range elements {
			em, _ := e.(map[string]any)
			if em["tag"] != "action" {
				continue
			}
			actions, _ := em["actions"].([]any)
			for _, a := range actions {
				am, _ := a.(map[string]any)
				if am["url"] == "https://artex.local/function/findings/detail?id=42" {
					foundButton = true
				}
			}
		}
		if !foundButton {
			t.Fatal("カードに詳細ページへのボタンがない")
		}
	})
	cfg := map[string]any{"webhook": srv.URL, "secret": secret}
	if _, err := (feishuChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestFeishuWithoutSecretOmitsSign(t *testing.T) {
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["sign"] != nil || body["timestamp"] != nil {
			t.Fatalf("secret 未設定時は署名パラメーターを付けるべきではない: %v", body)
		}
	})
	if _, err := (feishuChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestTelegramEscapesHTMLInUntrustedContent(t *testing.T) {
	var text string
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		text, _ = body["text"].(string)
		if body["parse_mode"] != "HTML" {
			t.Fatalf("HTML パースモードを使用すべき、得られたのは %v", body["parse_mode"])
		}
	})
	m := Message{Items: []Item{{
		Severity: "high",
		// タイトルとサマリーは検査対象／モデルの出力、つまり信頼できない内容。
		Name:    `<script>alert(1)</script>`,
		Summary: "a & b < c",
	}}}
	if _, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": srv.URL}, m); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
	if strings.Contains(text, "<script>") {
		t.Fatalf("HTML がエスケープされておらず、インジェクションが可能: %q", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Fatalf("エスケープ後の実体を期待、得られたのは %q", text)
	}
	if !strings.Contains(text, "a &amp; b") {
		t.Fatalf("& がエスケープされていない、得られたのは %q", text)
	}
}

func TestTelegramErrorClassification(t *testing.T) {
	rateLimited := capturePost(t, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`, nil)
	_, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": rateLimited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("429 は再試行可能にすべき、得られたのは %v", err)
	}

	forbidden := capturePost(t, `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`, nil)
	_, err = (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": forbidden.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("403 は設定問題なので永久失敗にすべき、得られたのは %v", err)
	}
}

func TestWebhookDefaultTemplateProducesValidJSON(t *testing.T) {
	// これが既定テンプレートの存在意義：タイトルに引用符や改行が入ると、素朴な
	// `"title": "{{.Title}}"` では不正な JSON になる。{{json .}} ならならない。
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["title"] != `[🟠 高] ログイン画面 "SQLインジェクション" リスク` {
			t.Errorf("タイトルが正しく復元されていない: %v", body["title"])
		}
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items の件数は 1 であるべき、得られたのは %d", len(items))
		}
		it, _ := items[0].(map[string]any)
		if it["summary"] != "パラメータ id\n未検証 による注入" {
			t.Errorf("サマリーが正しく復元されていない: %v", it["summary"])
		}
		// 値は文字列ではなく JSON の数値でなければならない（json:"...,string" の
		// ような書き方がこの罠を踏む）。
		if _, ok := it["finding_id"].(float64); !ok {
			t.Errorf("finding_id は数値であるべき、得られたのは %T", it["finding_id"])
		}
	})
	if _, err := (webhookChannel{}).Send(context.Background(), map[string]any{"url": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestWebhookCustomTemplateAndHeaders(t *testing.T) {
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, r *http.Request) {
		if r.Header.Get("X-Token") != "s3cret" {
			t.Errorf("カスタムヘッダーが失われた: %v", r.Header)
		}
		if body["msg"] != "3 件" {
			t.Errorf("カスタムテンプレートのレンダリングが誤っている: %v", body["msg"])
		}
		if body["first"] != "脆弱性1" {
			t.Errorf("range の取り出しが誤っている: %v", body["first"])
		}
	})
	cfg := map[string]any{
		"url":           srv.URL,
		"headers":       map[string]any{"X-Token": "s3cret"},
		"body_template": `{"msg": {{json (printf "%d 件" .Count)}}, "first": {{json (index .Items 0).Name}}}`,
	}
	if _, err := (webhookChannel{}).Send(context.Background(), cfg, batchMsg(3)); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestWebhookRejectsNonJSONRenderResult(t *testing.T) {
	cfg := map[string]any{"url": "https://example.com/hook", "body_template": `not json at all`}
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("レンダリング結果が JSON でない場合は永久失敗にすべき（テンプレートの誤りで再試行は無意味）、得られたのは %v", err)
	}
}

func TestWebhookValidateCatchesBadConfigEarly(t *testing.T) {
	bad := []map[string]any{
		{},
		{"url": "file:///etc/passwd"},
		{"url": "https://example.com", "method": "DELETE"},
		{"url": "https://example.com", "body_template": `{{.Items.`},
	}
	for i, cfg := range bad {
		if err := (webhookChannel{}).Validate(cfg); err == nil {
			t.Errorf("設定 %d は拒否されるべき: %v", i, cfg)
		}
	}
}

func TestEmailMessageIsWellFormed(t *testing.T) {
	msg, err := buildEmailMessage("artex@example.com", []string{"a@example.com", "b@example.com"}, singleMsg())
	if err != nil {
		t.Fatalf("メールの組み立てに失敗しました: %v", err)
	}
	if !strings.HasPrefix(msg, "From: artex@example.com\r\n") {
		t.Fatalf("From ヘッダーが誤っている:\n%s", msg)
	}
	if !strings.Contains(msg, "To: a@example.com, b@example.com\r\n") {
		t.Fatalf("To ヘッダーが誤っている:\n%s", msg)
	}
	// 日本語の件名は RFC 2047 でエンコードしなければ、クライアントで文字化けする。
	if !strings.Contains(msg, "Subject: =?utf-8?") {
		t.Fatalf("件名が RFC 2047 エンコードされていない:\n%s", msg)
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(mustExtractHeader(t, msg, "Subject")); err != nil {
		t.Fatalf("件名をデコードできない: %v", err)
	} else if !strings.Contains(dec, "SQLインジェクション") {
		t.Fatalf("件名のデコード結果が誤っている: %q", dec)
	}

	// 本文は base64 で、デコードすると正当な HTML になるはず。
	parts := strings.SplitN(msg, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("メールにヘッダー／本文の区切りがない")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(parts[1]), "\r\n", ""))
	if err != nil {
		t.Fatalf("本文の base64 デコードに失敗しました: %v", err)
	}
	html := string(decoded)
	if !strings.HasPrefix(html, "<div") {
		t.Fatalf("本文が HTML ではない: %.80s", html)
	}
	// タイトルはテキスト位置にそのまま現れる：HTML テキスト内容の二重引用符は
	// 正当な文字で、エスケープ不要。ここで「そのまま保持」を断言するのは、将来
	// 誰かが誤って引用符エスケープを追加し、引用符が &quot; と表示されるのを
	// 防ぐため。
	if !strings.Contains(html, `"SQLインジェクション"`) {
		t.Fatalf("タイトル内の引用符はテキスト位置ではそのまま保持すべき: %.200s", html)
	}
}

// TestEmailEscapesStructuralInjection はメール本文で本当に防ぐべきインジェクションを
// カバーする：脆弱性タイトルとサマリーは検査対象とモデルの出力であり、信頼できない
// 内容。テキスト位置では & < > をエスケープし（さもないとタグを注入できる）、
// 属性位置ではさらに引用符をエスケープする（さもないと href を閉じられる）。
func TestEmailEscapesStructuralInjection(t *testing.T) {
	m := Message{
		Items: []Item{{
			Severity:  "high",
			Name:      `<script>alert(1)</script>`,
			Summary:   "a & b > c",
			DetailURL: `https://artex.local/x?a="onmouseover=alert(1)`,
		}},
	}
	html := htmlBody(m, 0)
	if strings.Contains(html, "<script>") {
		t.Fatalf("タイトルがエスケープされておらず、タグを注入できる: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("エスケープ後の実体を期待: %s", html)
	}
	if !strings.Contains(html, "a &amp; b &gt; c") {
		t.Fatalf("& と > がエスケープされていない: %s", html)
	}
	// リンクは管理者設定の public_base_url で信頼度は高いが、属性位置では依然と
	// して引用符をエスケープする必要がある——引用符付きのアドレスは href を閉じ、
	// イベントハンドラーを注入できる。
	if strings.Contains(html, `onmouseover=alert(1)">`) {
		t.Fatalf("href 属性が正しくエスケープされていない: %s", html)
	}
	if !strings.Contains(html, "&quot;") {
		t.Fatalf("属性位置の引用符はエスケープすべき: %s", html)
	}
}

func mustExtractHeader(t *testing.T, msg, name string) string {
	t.Helper()
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	t.Fatalf("ヘッダー %s が見つからない", name)
	return ""
}

func TestChannelValidateReportsMissingFields(t *testing.T) {
	// 検証エラーは設定者に直接表示されるので、「設定が無効」ではなく何が足りない
	// かを明確に伝えなければならない。
	cases := []struct {
		kind   string
		cfg    map[string]any
		substr string
	}{
		{KindDingTalk, map[string]any{}, "Webhook"},
		{KindFeishu, map[string]any{}, "Webhook"},
		{KindWeCom, map[string]any{}, "Webhook"},
		{KindTelegram, map[string]any{}, "Bot Token"},
		{KindTelegram, map[string]any{"bot_token": "t"}, "Chat ID"},
		{KindEmail, map[string]any{}, "SMTP"},
		{KindEmail, map[string]any{"host": "h"}, "ポート"},
		{KindEmail, map[string]any{"host": "h", "port": 587, "from": "f"}, "宛先"},
	}
	for _, tc := range cases {
		ch, ok := Get(tc.kind)
		if !ok {
			t.Fatalf("チャネル %s が登録されていない", tc.kind)
		}
		err := ch.Validate(tc.cfg)
		if err == nil {
			t.Errorf("%s の設定 %v は検証に失敗すべき", tc.kind, tc.cfg)
			continue
		}
		if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("%s のエラーメッセージに %q が含まれるべき、得られたのは %q", tc.kind, tc.substr, err.Error())
		}
	}
}

// TestEmailSMTPErrorClassification は SMTP の 4xx/5xx の意味の区別を固定する。
// 4xx まで永久失敗と判定すると、グレーリストを有効にしたメールサーバーでは
// 毎回の配信が最初の試行後に failed になる——グレーリストこそ自動再試行が
// 最も効く場面なのに。
func TestEmailSMTPErrorClassification(t *testing.T) {
	cases := []struct {
		reply     string
		permanent bool
	}{
		{"450 4.7.1 Greylisting in action, please come back later", false},
		{"451 4.3.0 Temporary system failure", false},
		{"452 4.2.2 Mailbox full", false},
		{"550 5.1.1 User unknown", true},
		{"553 5.1.3 Bad address syntax", true},
		{"554 5.7.1 Relay access denied", true},
		// 応答コードが取得できないときは「再試行可能」として扱う：1 回余計に
		// 試す方が、起こりうる一時障害を恒久失敗と断定するよりまし。
		{"unexpected EOF", false},
		{"", false},
	}
	for _, tc := range cases {
		err := smtpStageError("宛先が拒否されました", errors.New(tc.reply))
		if got := IsPermanent(err); got != tc.permanent {
			t.Errorf("応答 %q: permanent=%v を期待、得られたのは %v", tc.reply, tc.permanent, got)
		}
		// 分類がどうであれ、原文は利用者の調査のために保持する。
		if tc.reply != "" && !strings.Contains(err.Error(), tc.reply) {
			t.Errorf("応答 %q の原文が失われた: %v", tc.reply, err)
		}
	}
}

func TestRegistryCoversAllKinds(t *testing.T) {
	// 6 つのチャネルは 1 つも欠かせない——1 つでも欠けると UI のドロップダウン
	// から静かに消える。
	want := []string{KindDingTalk, KindEmail, KindFeishu, KindTelegram, KindWebhook, KindWeCom}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("チャネル数は %d であるべき、得られたのは %d: %v", len(want), len(got), got)
	}
	for _, k := range want {
		if !ValidKind(k) {
			t.Errorf("チャネル %s が登録されていない", k)
		}
		if ch, ok := Get(k); !ok || ch.Kind() != k {
			t.Errorf("チャネル %s の Kind() が登録キーと一致しない", k)
		}
	}
	if ValidKind("nope") {
		t.Error("未登録の種別は検証を通過すべきではない")
	}
}

func TestPermanentErrorUnwrap(t *testing.T) {
	base := &permanentSentinel{}
	err := Permanent(base)
	if !IsPermanent(err) {
		t.Fatal("永久失敗として識別すべき")
	}
	if !strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("エラーメッセージは下位のエラーを透過すべき: %v", err)
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) は nil を返さなければならない")
	}
	if IsPermanent(nil) {
		t.Fatal("nil は永久失敗ではない")
	}
}

type permanentSentinel struct{}

func (*permanentSentinel) Error() string { return "sentinel" }
