package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 本ファイルは doJSON の HTTP 層のエラー分類をカバーする。
//
// なぜ別にテストするか：各チャネルアダプタはプラットフォーム固有のビジネス
// エラーコード（DingTalk の errcode、Lark の code、Telegram の ok フィールド）
// だけを扱い、**HTTP 層**の分類は doJSON が一元的に行う。両者は独立した二重の
// 防衛線。これがなければ、503 を返す中継ゲートウェイは永久失敗とみなされて
// 再試行が打ち切られ、403 は再試行可能とみなされて無駄に 3 回バックオフする。

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 成功はエラーではない", 200, false},
		{"429 レート制限は再試行可能", 429, false},
		{"408 リクエストタイムアウトは再試行可能", 408, false},
		{"500 サーバーエラーは再試行可能", 500, false},
		{"502 ゲートウェイエラーは再試行可能", 502, false},
		{"503 サービス利用不可は再試行可能", 503, false},
		{"400 パラメーターエラーは永久失敗", 400, true},
		{"401 認証失敗は永久失敗", 401, true},
		{"403 アクセス禁止は永久失敗", 403, true},
		{"404 アドレス不在は永久失敗", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx はエラーを返すべきではない: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("非 2xx はエラーを返すべき")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("HTTP %d の permanent 判定が誤り：期待 %v 得られた %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// ステータスコードは必ずエラーに含める。さもないとユーザーは自分が
			// 設定を間違えたのか相手側が落ちているのか判断できない。Go の英語の
			// StatusText ではなく数字を断言する：本パッケージの文言は日本語であり
			// （プロジェクトの他の部分と同様）、数字こそ言語非依存で安定して
			// 断言できる部分だから。
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("エラーメッセージに HTTP ステータスコード %d を含めるべき、得られたのは %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet は snippet をカバーする：相手側のエラー説明を
// 持ち帰らなければ、ユーザーは「失敗した」としか分からず、相手がなぜ拒否したかを
// 知ることができない。
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("エラーを返すべき")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("エラーメッセージに相手側の説明を持ち帰るべき、得られたのは %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded は snippet の形を制約する：相手側の
// 応答はそのまま last_error 列とフロントエンドの表に入るので、複数行や超長文は
// レイアウトとペイロードを壊す。
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// 改行、タブ、5000 文字の超長文を含む応答。
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("エラーを返すべき")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("エラーメッセージは 1 行に畳むべき、得られたのは %q", msg)
	}
	// snippet の上限は 200 文字 + 固定プレフィックスなので、全体は元の応答より
	// はるかに短くなければならない。
	if len(msg) > 400 {
		t.Errorf("エラーメッセージが長すぎる（%d バイト）。snippet で切り詰めるべき: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse は読み取りに上限があることを確認する：
// 相手側が異常に巨大な内容を返しても、応答全体をメモリに読み込んではならない
// （配信履歴の各エントリーが last_error を 1 つずつ保存する）。
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("エラーを返すべき")
	}
	if len(err.Error()) > 400 {
		t.Errorf("巨大な応答は長さを制限して読み取り、切り詰めるべき。エラーメッセージ長 %d", len(err.Error()))
	}
}
