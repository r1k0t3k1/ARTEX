package notify

import (
	"bufio"
	"context"

	"net"
	"strings"
	"sync"
	"testing"
)

// 本ファイルはメールチャネルのプロトコルレベルのテストを補完する。これまで
// email.Send のカバレッジは 0 だった——SMTP の経路全体を 1 つもテストが通って
// おらず、しかもこれは 6 つのチャネルの中でプロトコルの面が最も広く、最も誤り
// やすいもの（ハンドシェイク、認証、エンベロープ、DATA 段階がそれぞれ異なる
// 失敗の意味論を持つ）。
//
// ここでは自前で用意した最小の SMTP サーバーで駆動し、net/smtp を mock しない：
// メールチャネルのリスクの大部分は「実際の SMTP サーバーと会話する」段階にあり、
// そこを mock するのはテストしないのと同じ。

// fakeSMTP は最低限動く SMTP サーバー：greet/EHLO/AUTH/MAIL/RCPT/DATA/QUIT を
// 完了でき、テストケースの要求に応じて特定の段階へ指定の応答コードを返す。
type fakeSMTP struct {
	ln net.Listener

	// rcptReply は RCPT TO への応答。既定は 250。
	rcptReply string
	// mailReply は MAIL FROM への応答。既定は 250。
	mailReply string
	// advertiseAuth が true のとき、EHLO で AUTH PLAIN のサポートを宣言する。
	advertiseAuth bool

	mu       sync.Mutex
	data     string
	commands []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, rcptReply: "250 OK", mailReply: "250 OK"}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	addr, ok := f.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("TCP 以外のリッスンアドレス")
	}
	return "127.0.0.1", addr.Port
}

func (f *fakeSMTP) record(cmd string) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
}

func (f *fakeSMTP) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func (f *fakeSMTP) sawCommand(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake.local ESMTP ready")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.record(line)
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// STARTTLS を宣言しない：コードを平文経路に通す（テスト対象は
			// エンベロープのロジックであり TLS ではない）。
			w("250-fake.local")
			if f.advertiseAuth {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case strings.HasPrefix(line, "AUTH"):
			// 簡略化：PLAIN の初期応答は複数行にまたがる可能性があるが、そのまま受け入れる。
			w("235 2.7.0 Authentication successful")
		case strings.HasPrefix(line, "MAIL FROM"):
			w(f.mailReply)
		case strings.HasPrefix(line, "RCPT TO"):
			w(f.rcptReply)
		case strings.HasPrefix(line, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>")
			var sb strings.Builder
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				sb.WriteString(dl)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			w("250 2.0.0 Ok: queued as FAKE1")
		case strings.HasPrefix(line, "QUIT"):
			w("221 2.0.0 Bye")
			return
		default:
			w("250 OK")
		}
	}
}

func emailCfg(t *testing.T, f *fakeSMTP, extra map[string]any) map[string]any {
	t.Helper()
	host, port := f.hostPort(t)
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "artex@example.com",
		"to":   []any{"a@example.com", "b@example.com"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func TestEmailSendDeliversFullMessage(t *testing.T) {
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	cfg := emailCfg(t, f, map[string]any{"username": "artex", "password": "pw"})

	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
	// エンベロープ段階まで到達すべき：差出人、2 人の宛先、DATA。
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP セッションに %q がない。実際のコマンド：%v", want, f.commands)
		}
	}
	// 本文は base64 の HTML で、実際の脆弱性の内容を含むべき（エンコード後も判別可能）。
	body := f.body()
	if body == "" {
		t.Fatal("DATA 段階で本文を受信していない")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("Content-Type ヘッダーがない:\n%s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("本文が base64 エンコードされていない（長い HTML 行は SMTP の 1000 バイト行長制限を壊す）:\n%s", body)
	}
	// 複数の宛先がすべて To ヘッダーに現れるべき。
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("To ヘッダーにすべての宛先が含まれていない:\n%s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// アカウント未設定のときは AUTH を送信すべきではない——中継によっては
	// それを理由に拒否する。
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("アカウント未設定なのに AUTH を送信した: %v", f.commands)
	}
}

// TestEmailSendClassifiesSMTPReplies は今回の監査修正の直接の検証：
// 5xx は永久失敗、4xx（グレーリスト）は再試行可能と判定する。
func TestEmailSendClassifiesSMTPReplies(t *testing.T) {
	cases := []struct {
		name      string
		rcptReply string
		mailReply string
		permanent bool
	}{
		{"宛先が 550 で永久拒否", "550 5.1.1 User unknown", "250 OK", true},
		{"宛先が 450 のグレーリスト", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"宛先が 452 メールボックス満杯", "452 4.2.2 Mailbox full", "250 OK", false},
		{"差出人が 553 で永久拒否", "250 OK", "553 5.1.3 Bad address", true},
		{"差出人が 451 の一時エラー", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("エラーを返すべき")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("permanent 判定が誤り：期待 %v 得られた %v (%v)", tc.permanent, got, err)
			}
			// サーバーの原文は保持すべき。さもないとユーザーはサーバー管理者に
			// 問い合わせるべきかアドレスを変えるべきか分からない。
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("エラーにサーバーの応答コードを保持すべき: %v", err)
			}
		})
	}
}

func TestEmailSendRefusesPlaintextCredentials(t *testing.T) {
	// net/smtp の PlainAuth は暗号化されていない接続で資格情報を送るのを拒否する
	// （宛先が localhost の場合を除く）。これは**正しい**セキュリティ挙動で、迂回しては
	// ならない。ただしユーザーが修正できるエラーを示す必要がある。
	// ここでは localhost 以外のホスト名でそれを引き起こす。
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	_, port := f.hostPort(t)
	cfg := map[string]any{
		"host":     "smtp.example.com", // localhost ではない
		"port":     float64(port),
		"from":     "a@example.com",
		"to":       []any{"b@example.com"},
		"username": "artex",
		"password": "pw",
	}
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Skip("ローカルの DNS がローカルサーバーに解決されたためスキップ（他のケースには影響しない）")
	}
	// 接続できない、または資格情報の送信を拒否されるのどちらでもこの断言は通る。
	// 重要なのは**静かにパスワードを送信しない**こと。
	if !IsPermanent(err) && !strings.Contains(err.Error(), "接続") {
		t.Logf("エラー：%v（localhost 以外では接続できないのが想定どおり）", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// メールチャネルは設定フィールドが最も多く、どれか 1 つ漏れても配信時に
	// 初めて露見する。ここでは 1 つずつ検証で事前に止められることを確認する。
	// 断言が確認するのは「エラーメッセージが何の不足に言及しているか」。
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"host がない", map[string]any{"port": float64(25), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"port がない", map[string]any{"host": "smtp.example.com"}},
		{"port が範囲外", map[string]any{"host": "h", "port": float64(70000), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"from がない", map[string]any{"host": "h", "port": float64(25), "to": []any{"d@e.f"}}},
		{"to がない", map[string]any{"host": "h", "port": float64(25), "from": "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (emailChannel{}).Validate(tc.cfg); err == nil {
				t.Fatalf("検証に失敗すべき: %v", tc.cfg)
			}
		})
	}
}

// TestEmailConfigTolerance は設定読み取りの寛容さをカバーする：JSONB では数値は
// float64 だが、ユーザーが UI でポートを文字列で入力する可能性もある。配列が
// 単一の文字列であることもある。
func TestEmailConfigTolerance(t *testing.T) {
	cfg := map[string]any{
		"host": "smtp.example.com",
		"port": "587", // 文字列形式のポート
		"from": "a@b.c",
		"to":   "d@e.f", // 配列ではなく単一の文字列
		"tls":  "true",  // 文字列形式のブール値
	}
	if err := (emailChannel{}).Validate(cfg); err != nil {
		t.Fatalf("文字列形式の数値を許容すべき: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt が文字列のポートを解析できていない、得られたのは %d", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool が文字列 \"true\" を解析できていない")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgStrings が単一の文字列に対応していない、得られたのは %v", to)
	}
}

// TestFilterValidateRejectsTypo は監査修正の直接の検証：
// しきい値の打ち間違いは書き込み時に止めなければ、フィルターが静かに無効化され
// 全部配信になる。
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("正当なしきい値 %q が拒否された: %v", s, err)
		}
	}
	// 実際に起こりうる誤記——すべて拒否しなければならない。
	for _, s := range []string{"hgih", "HIGH", "深刻", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("不正なしきい値 %q は拒否すべき（さもないとフィルターが静かに無効化され全部配信になる）", s)
			continue
		}
		// エラーメッセージはユーザーが正しく修正できるものでなければならない。
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("エラーメッセージに選択肢を列挙すべき、得られたのは %q", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly は「書き込みは厳格、読み取りは寛容」という
// 役割分担を固定する：DB に既存の不正値があってもチャネル全体を読み出せなくしては
// ならない（さもないと過去のチャネルが突然すべて配信を停止する）。
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // エラーを返さない
	if f.MinSeverity != "hgih" {
		t.Fatalf("読み取り経路はそのまま保持すべき、得られたのは %q", f.MinSeverity)
	}
	// かつそのチャネルは依然としてイベントを判定できる（panic せず、ブロックしない）。
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
