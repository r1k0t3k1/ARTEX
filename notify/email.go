package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout はそれぞれ接続確立と SMTP セッション
// 全体を制限する。net/smtp 自体にはタイムアウト機構が一切なく、この 2 つを
// 設けないと、固まった相手が配信 goroutine を永久にぶら下げてしまう——しかも
// dispatcher は単一 goroutine で直列に処理するため、通知システム全体が停止する。
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel は SMTP メール配信を実装する。
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// メールにはプラットフォームのレート制限はないが、これで画面を埋め尽くすべきでも
// ない。緩めの既定値を与える。
func (emailChannel) DefaultRatePerMin() int { return 60 }

// パスワードだけをマスクする。SMTP ホスト、アカウント、宛先は秘密ではないので、
// これらをマスクすると編集が面倒になるだけ。
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port はパスワードをどのサーバーに渡すかを決め、tls は通信を暗号化するかを
// 決める。3 つのいずれかが変わればパスワードの再提示を要求する——ついでに
// 「TLS を切る」という操作にも資格情報を明示的に持たせ、ついでの変更で済ませられない
// ようにする。
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("SMTP サーバーアドレスがありません")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("SMTP ポートが無効です（1-65535 の範囲で指定してください）")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("差出人アドレスがありません")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("宛先アドレスが少なくとも 1 つ必要です")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS：相手が対応していればアップグレードする。平文セッションでは資格情報を
	// 送れない（下の auth の説明を参照）。
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS に失敗しました: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth は暗号化されていない接続で資格情報を送るのを拒否する
			// （宛先が localhost の場合を除く）。これは**正しい**セキュリティ挙動で
			// 回避してはならないが、理由を分かりやすく伝える必要がある——そうしないと
			// 利用者には「unencrypted connection」とだけ見えて、どうすればよいか
			// 分からない。
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("資格情報の送信を拒否しました：接続が暗号化されていません。TLS を有効にするか、465 ポート（暗黙的 TLS）に変更するか、「TLS を有効化」にチェックを入れてください (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP 認証に失敗しました: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("差出人 %s が拒否されました", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("宛先 %s が拒否されました", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA に失敗しました: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("メール本文の書き込みに失敗しました: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("メールの送信に失敗しました: %w", err)
	}
	// Quit の失敗は「メールがサーバーに受理された」という事実を左右しないので、
	// そのエラーは無視する。
	_ = client.Quit()
	// メールには長さの切り詰めがない（HTML 本文を全部送る）ので、バッチ全体が
	// 配信済みとみなされる。
	return len(m.Items), nil
}

// emailDial は SMTP 接続を確立する。
//
// implicitTLS=true は 465 のような「接続即 TLS」方式。false は 25/587 で平文接続
// してから STARTTLS する。両者は混用できない：465 ポートに平文の greeting を
// 送ると即座に切断される。
//
// セッション期限は**接続確立時**に設定する（後から追加設定するのではなく）。
// net/smtp の Client は基盤の接続を未公開フィールドに隠し、外部から取得できない
// ため、接続を渡してしまった後は事前に設定した deadline に頼るしかない。これにより
// ハンドシェイク段階のブロッキングも併せてカバーされる。
// Control に blockInternalDial を掛け、HTTP 系チャネルと同じガードを共有する。
// 掛けないと SMTP が SSRF 防御全体の穴になる：host に 169.254.169.254 や
// 127.0.0.1 を書けば直接接続でき、smtp.NewClient のハンドシェイク失敗時には相手が
// 返した 1 行がエラーに包まれ、last_error 経由で配信履歴 API から返るため、
// 半盲読み取りプリミティブが成立する。「接続拒否 vs タイムアウト」の所要時間差も
// ポートプローブに使える。ダイヤル段階が最終的な適用点で、DNS リバインディングも
// カバーする。
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP サーバーへの接続に失敗しました: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP ハンドシェイクに失敗しました: %w", err)
	}
	return client, nil
}

// smtpStageError は SMTP 応答コードに応じて、ある段階の失敗を「再試行可能」と
// 「恒久的失敗」に振り分ける。
//
// なぜ区別が必要か：SMTP の 4xx と 5xx は意味がまったく異なる——
//   - 4xx（450 グレーリスト、451 ローカルエラー、452 保存容量不足）は**一時的**な
//     拒否で、正攻法はしばらくしてから再試行すること。とくにグレーリストは、
//     ほぼ毎回の初回配信で遭遇する。
//   - 5xx（550 ユーザー不在、553 不正なアドレス）は恒久的な拒否で、再試行に意味はない。
//
// 一律に恒久的失敗と判定すると、グレーリストを有効にしたメールサーバーは**すべての**
// 配信を 1 回目の試行後に failed にしてしまう——しかもそうした失敗こそ自動再試行が
// 最も力を発揮すべき場面である。応答コードはエラーテキストの先頭 3 桁から取り、
// 取れないときは再試行可能として扱う（1 回余計に試す方が、解析できないという
// 理由で起こりうる一時障害を恒久失敗と断定するよりまし）。
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode は SMTP のエラーテキストから先頭の 3 桁の応答コードを取り、
// 取れなければ 0 を返す。net/smtp はエラーコードのフィールドを公開しないので、
// テキストから取るしかない。形式は「450 4.7.1 ...」。
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage は完全な RFC 5322 メールを組み立てる。
//
// 本文を base64 でエンコードする理由は 2 つ：第一に、SMTP は 1 行 1000 バイトを
// 超えてはならないが、HTML 本文（とくに集約メール）は長い行が容易に発生する。
// 第二に、base64 は "." で始まる行を自然に生まないため、SMTP のドットエスケープの
// 手間が省ける。
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// 日本語の件名は RFC 2047 エンコードが必須で、そうしないとクライアントで
	// 文字化けして表示される。
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// メールには長さの厳格な上限がないので、本文は切り詰めない。
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64 は 76 文字で折り返し、RFC 2045 に準拠する。
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
