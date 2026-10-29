package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 本ファイルは不変条件テスト：チャネル実装から出てくる**あらゆる**エラーテキストが
// 資格情報を持ち出してはならない。
//
// なぜ別ファイルに切り出すか：当初のチャネルのケースは成功経路とプラットフォームの
// ビジネスエラーしかカバーしておらず、トランスポート層の失敗をまったく見ていなかった。
// しかも危険なのはまさにトランスポート層のエラー（接続拒否／DNS 失敗／タイムアウト）
// ——http.Client.Do が返す *url.Error は**完全な URL** をエラーテキストに書き込み、
// 本機能のこの数社は資格情報が URL の中にある。資格情報はこの経路で 4 つの出口へ
// 流れ出していた：
//
//	notification_deliveries.last_error  → 平文で DB に保存
//	GET /api/notify/deliveries の応答   → チャネル設定のマスクを迂回し、ブラウザへエコー
//	サーバーログ                        → 外部へ持ち出され保存されることが多い
//	テスト送信 API の 502 応答          → フロントエンドに直接表示
//
// なのでここでは 1 つの関数だけをテストするのではなく、チャネルごとに必ず失敗する
// リクエストを実際に送り、エラーテキストにその資格情報が見つからないことを断言する。

// credentialCases は「資格情報が URL にある」すべてのチャネル形態をカバーする：
// DingTalk／WeCom は query、Lark はパスの末尾、Telegram はパスの中段。
var credentialCases = []struct {
	name   string
	ch     Channel
	cfg    map[string]any
	secret string
}{
	{
		name:   "DingTalk の access_token が query にある",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "WeCom の key が query にある",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Lark の hook id がパスの末尾にある",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Telegram の bot token がパスの中段にある",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "DingTalk の署名キー",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send", "secret": leakProbeToken},
		secret: leakProbeToken,
	},
}

// leakProbeToken は絶対に本物の資格情報でありえない番兵値で、エラーテキスト内を
// 検索するために使う。
const leakProbeToken = "LEAKPROBE0123456789abcdef"

// TestChannelErrorsNeverLeakCredentials は中核となる不変条件。
func TestChannelErrorsNeverLeakCredentials(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			// 必ず失敗する相手：127.0.0.1:1 は誰も待ち受けていないので、接続拒否の
			// 経路を通る。
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{
				Items: []Item{{FindingID: 1, Severity: "high", Name: "漏洩プローブ"}},
			})
			if err == nil {
				t.Fatal("到達不能なアドレスならエラーを返すべき")
			}
			assertNoSecret(t, err.Error(), tc.secret)
		})
	}
}

// TestChannelErrorsNeverLeakCredentialsInPermanentPath は永久失敗の分岐をカバーする：
// URL 検証の失敗やプラットフォームのビジネスエラーもエラーテキストを外へ出すので、
// 同様に資格情報を含んではならない。
func TestChannelErrorsNeverLeakCredentialsInPermanentPath(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		cfg  map[string]any
	}{
		// アドレスに資格情報を含むが形式が不正 → validateHTTPURL / url.Parse の分岐を踏む。
		{"DingTalk のアドレスが不正", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"WeCom のアドレスが不正", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"Lark のアドレスが不正", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"Telegram の API アドレスが不正", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"汎用 Webhook のアドレスが不正", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("不正な設定ならエラーを返すべき")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("エラーテキストが資格情報 %q を漏らした:\n    %s", secret, text)
	}
}

func TestRedactRequestTargetKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=S1":    "https://oapi.dingtalk.com/…",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=S2": "https://qyapi.weixin.qq.com/…",
		"https://open.feishu.cn/open-apis/bot/v2/hook/S3":         "https://open.feishu.cn/…",
		"https://api.telegram.org/botS4/sendMessage":              "https://api.telegram.org/…",
		"http://10.0.0.5:8080/hook":                               "http://10.0.0.5:8080/…",
	}
	for in, want := range cases {
		got := redactRequestTarget(in)
		if got != want {
			t.Errorf("redactRequestTarget(%q) = %q、期待 %q", in, got, want)
		}
		// マスク結果自体が元アドレスのパス／クエリ断片を再び含んではならない。
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("マスク後もパス／クエリ断片 %q を含む: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// 解析できない入力は絶対に元の文字列をエコーしない。
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("解析できない入力 %q が %q としてエコーされた", bad, got)
		}
	}
}

// TestRedactTransportErrorStripsURL は *url.Error という具体的な型を直接狙う：
// これは http.Client.Do の戻り値の型であり、漏洩の第一現場でもある。
func TestRedactTransportErrorStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + leakProbeToken + "/sendMessage",
		Err: inner,
	}
	got := redactTransportError(uerr)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "api.telegram.org") {
		t.Errorf("調査のために host は保持すべき、得られたのは %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("調査のために根本原因は保持すべき、得られたのは %q", got)
	}
	// Op も保持すべき（POST か GET かは調査に意味がある）。
	if !strings.Contains(got, "Post") {
		t.Errorf("操作名は保持すべき、得られたのは %q", got)
	}
}

// TestRedactURLsInTextHandlesFallback はフォールバック経路：*url.Error ではない
// 独自のエラー（リダイレクトポリシーが返すエラーなど）内のアドレスも同様に
// 取り除かなければならない。
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("クロスホストのリダイレクトを拒否しました（a.example → http://b.example/bot%s/send）", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("アドレスをマスク後の形へ置換すべき、得られたのは %q", got)
	}
	// アドレスを含まないテキストはそのまま保持する。
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("アドレスを含まないテキストは変更すべきではない")
	}
}

// TestCrossHostRedirectRefused は「資格情報が URL にあり + クロスホストのリダイレクト
// に追従 = 資格情報を渡す」をカバーする。httptest の 2 つのサーバーは 127.0.0.1 の
// 異なるポートで待ち受け、ポートが違えば Host も違うので、ちょうどクロスホストの
// リダイレクトを構成できる。
func TestCrossHostRedirectRefused(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/robot/send?access_token="+leakProbeToken, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": redirector.URL + "/robot/send?access_token=" + leakProbeToken},
		Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("クロスホストのリダイレクトは拒否すべき")
	}
	if hit {
		t.Fatal("リダイレクト先へアクセスされた——資格情報がリダイレクトに伴って漏洩した")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestSameHostRedirectAllowed は逆方向のケース：同一ホストへのリダイレクト
// （末尾のスラッシュ追加など）は引き続き利用できなければならない。さもないと
// 正常なワークフローまで一緒に塞いでしまう。
func TestSameHostRedirectAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robot/send" {
			// 同一ホスト、同一ポートへのリダイレクト。
			http.Redirect(w, r, "/robot/send/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"},
		Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("同一ホストのリダイレクトは拒否すべきではない: %v", err)
	}
}
