package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// 本ファイルは 2 つの関連する堅牢化をカバーする：
//   ① 配信先アドレスがサーバーを踏み台にして内部ネットワーク／クラウドメタデータを
//      叩いてはならない（SSRF）
//   ② アドレス検証のエラーメッセージがアドレス内の資格情報を持ち出してはならない
//
// テスト環境について：本パッケージの多くのケースは 127.0.0.1 上の httptest の
// 偽受信先を使うが、ガードは既定でそれらをブロックする。そこで TestMain で一括して
// AllowLocalTargetsEnv を有効化し、以下の各 SSRF ケースが明示的にそれをクリアして
// **既定の拒否**の挙動を断言する。

func TestMain(m *testing.M) {
	// 通常のケースがローカルの偽受信先へ接続できるようにする。SSRF ケースは自分で
	// 一時的にクリアする。
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault は SSRF 防御の中核となる断言：
// 既定の設定では、ループバックアドレスへの配信は**接続層**で拒否されなければならない。
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // 逃げ道をオフ = 既定の挙動
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("既定ではループバックアドレスへの配信を許可すべきではない")
	}
	if hit {
		t.Fatal("リクエストがすでにローカルサービスへ到達した——ガードが効いていない")
	}
	// エラーメッセージはユーザーに解除方法を示さなければならない（ローカルの SMTP
	// 中継は正当な設定）。
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("拒否メッセージは明示的な解除方法を示すべき: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn は逆方向のケース：明示的に有効化すれば
// 使えなければならない。さもないとローカルの postfix／内部中継といった正当な
// デプロイが一律に潰される。
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("明示的に解除すれば配信できるべき: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // クラウドメタデータのエンドポイント——本関数が存在する主な理由
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped 形式は復元してから判定すべき。さもないとバイパスの穴になる
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s は拒否すべき", s)
		}
	}
	// RFC1918 のプライベートネットワークは**意図的に許可**する：内部に自前で建てた
	// Mattermost／SMTP 中継はよくある正当な使い方。この断言はその取捨を固定する
	// ——将来誰かがついでにプライベートネットワークの判定を加えるとここが失敗し、
	// 意識的な決定を迫られる（静かにデプロイを殺すのではなく）。
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s は許可すべき（プライベートネットワークはよくある正当な配信先）", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets は設定段階での事前警告をカバーする：
// リテラルの IP は保存時に拒否すべきで、最初の配信が失敗するまで待つべきではない。
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s は設定段階で拒否すべき", raw)
		}
	}
	// パブリックアドレスとプライベートアドレスは通常どおり通過する（プライベートは
	// ダイヤル段階に委ね、そこではブロックしない）。
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s は検証を通過すべき: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials は監査で指摘された、前回の私が
// 見落とした分岐。
//
// url.Parse が**失敗**すると *url.Error を返し、その Error() は完全な元のアドレスを
// 含む。前回は http.Client.Do の戻り値だけをマスクし、ここを漏らしていた。しかも
// 当時追加した「永久失敗経路」のケース（file://、gopher://、ftp://）はどれも
// url.Parse が成功して scheme の分岐を通るため、すべてグリーンでもこの経路が安全だと
// は証明できない——偽の保証だった。
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // 不正なパーセントエスケープ
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // ポートが数字でない
		"http://[::1?access_token=" + leakProbeToken,                  // 括弧の対応が取れていない
	}
	for _, raw := range cases {
		// まず、この入力が**確かに** url.Parse を失敗させることを確認する。この
		// 手順を踏まないと、ケースが気づかないうちに別の分岐へ流れうる（前回の
		// 偽の保証はこうして生まれた）。
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q は本来解析に失敗するはず。さもないとこのケースは対象の分岐をカバーしていない", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q は検証に失敗すべき", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// チャネル層のラップもアドレスを持ち出していないことを確認する。
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("不正なアドレスは検証に失敗すべき")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault は SMTP チャネルのダイヤルガードを
// カバーする。
//
// メールチャネルはかつて素の net.Dialer を使っており、SSRF 防御全体で唯一の欠陥
// だった：host を 169.254.169.254 や 127.0.0.1 にすると直接接続でき、しかも
// smtp.NewClient のハンドシェイク失敗時には相手が返した 1 行がエラーに包まれ、
// last_error を経て配信履歴 API からエコーされる——まさに他のチャネルがすでに
// 塞いだ半盲読み取りプリミティブであり、「接続拒否かタイムアウトか」の所要時間差は
// ポートスキャンにも使えた。
//
// 本パッケージの TestMain は AllowLocalTargetsEnv をグローバルに有効化しているので
// （多くのケースが 127.0.0.1 の偽受信先を使う）、このケースは自分でそれをクリア
// しなければならない——さもないとガードの有無にかかわらず通過してしまい、まさに
// それが当初この欠陥がどのテストにも見つからなかった理由でもある。
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // 逃げ道をオフ = 既定の挙動
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("既定ではメールをループバックアドレスへ配信すべきではない")
	}
	// 接続自体を確立してはならない：ガードが Control フックで止めるので、EHLO は
	// 永遠に送られない。
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP セッションがすでに確立された——ガードが効いていない")
	}
	// エラーメッセージはユーザーに解除方法を示さなければならない（ローカルの
	// postfix 中継は正当な設定）。
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("拒否メッセージは明示的な解除方法を示すべき: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn は対になる逆方向のケース：明示的に
// 有効化すれば正常に配信できなければならない。内部に自前で建てた SMTP／ローカル
// 中継は非常によくあるデプロイで、ガードが一律に潰してはならない。
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("明示的に解除すればローカル SMTP へ配信できるべき: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("EHLO が見えない——セッションが実際には確立されていない")
	}
}
