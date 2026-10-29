package notify

import (
	"net/url"
	"testing"
	"time"
)

// 署名の基準値は OpenSSL で独立に計算したもので、本パッケージ自身の実装で生成した
// ものではない——さもないと「コードが変わっていない」ことしか示せず、「アルゴリズムが
// 正しい」ことを示せない。
//
//	TS=1700000000000, SECRET=SECtest123
//	DingTalk: printf '%s\n%s' "$TS" "$SECRET" | openssl dgst -sha256 -hmac "$SECRET" -binary | openssl base64 -A
//	      -> w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE=
//	Lark: printf '' | openssl dgst -sha256 -hmac "$(printf '%s\n%s' "$TS" "$SECRET")" -binary | openssl base64 -A
//	      -> Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo=
const (
	signTestTSMillis = int64(1700000000000)
	signTestSecret   = "SECtest123"
	dingTalkExpected = "w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE="
	feishuExpected   = "Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo="
)

func TestDingTalkSignMatchesReference(t *testing.T) {
	got, err := dingTalkSignedURL("https://oapi.dingtalk.com/robot/send?access_token=tok", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatalf("署名に失敗しました: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("生成されたアドレスを解析できない: %v", err)
	}
	q := u.Query()
	if q.Get("sign") != dingTalkExpected {
		t.Errorf("署名が一致しない\n期待 %s\n得られた %s", dingTalkExpected, q.Get("sign"))
	}
	if q.Get("timestamp") != "1700000000000" {
		t.Errorf("タイムスタンプはミリ秒でそのまま付与すべき、得られたのは %q", q.Get("timestamp"))
	}
	// 元の query パラメーター（access_token）が署名で上書きされてはならない。
	if q.Get("access_token") != "tok" {
		t.Errorf("元の query パラメーターが失われた、得られたのは %q", q.Get("access_token"))
	}
}

func TestFeishuSignMatchesReference(t *testing.T) {
	got := feishuSign("1700000000000", signTestSecret)
	if got != feishuExpected {
		t.Errorf("署名が一致しない\n期待 %s\n得られた %s", feishuExpected, got)
	}
}

// TestSignAlgorithmsDiffer は 2 社のアルゴリズムの違いを固定する。両者はちょうど
// 互いの引数の順序が逆（DingTalk は key=secret、Lark は key=署名対象文字列）なので、
// もう一方を丸写しすると必ず検証に失敗する。このケースは将来のリファクタリングで
// 両者が 1 つの関数に統合されないことを保証する。
func TestSignAlgorithmsDiffer(t *testing.T) {
	ts := "1700000000000"
	dingURL, err := dingTalkSignedURL("https://example.com/hook", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	dq, _ := url.Parse(dingURL)
	if dq.Query().Get("sign") == feishuSign(ts, signTestSecret) {
		t.Fatal("DingTalk と Lark の署名が同一。どちらかのアルゴリズム実装が誤っている")
	}
}

func TestDingTalkNoSecretLeavesURLUntouched(t *testing.T) {
	// 署名を有効にしていないボット：timestamp/sign パラメーターを勝手に追加してはならない。
	const hook = "https://oapi.dingtalk.com/robot/send?access_token=tok"
	got, err := dingTalkSignedURL(hook, "", time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	if got != hook {
		t.Fatalf("secret 未設定時はアドレスを変更すべきではない、得られたのは %q", got)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	ok := []string{"https://example.com/hook", "http://10.0.0.1:8080/x?y=1"}
	for _, s := range ok {
		if err := validateHTTPURL(s); err != nil {
			t.Errorf("%q は受理すべき: %v", s, err)
		}
	}
	// file:// などは通してはならない——http.Client の扱いが想定の範囲を超える。
	bad := []string{"", "file:///etc/passwd", "ftp://example.com", "https://", "gopher://x"}
	for _, s := range bad {
		if err := validateHTTPURL(s); err == nil {
			t.Errorf("%q は拒否すべき", s)
		}
	}
}
