package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets はメッセージをループバック／リンクローカルアドレスへ配信する
// ことを許可するかを決める。
//
// 既定では拒否する。これらのアドレス帯は IM ボットや公衆のメールサーバーが現れる
// 場所ではなく、しかもそこから触れるものはきわめて機微：同一マシン上の別サービスの
// 管理ポート、そしてクラウド環境のメタデータエンドポイント（169.254.169.254 からは
// インスタンスの資格情報が読める）。配信アドレスは管理者が設定するが、XSS/CSRF に
// 乗っ取られた管理セッションや、同じ JWT を共有する二人目の人物は、設定を変える
// だけで応答内容を読み戻せる——doJSON は 4xx/5xx の応答体の先頭 200 バイトを
// last_error に書き込み、配信履歴 API がそれを表示するので、これが半盲読み取り
// プリミティブになる。
//
// ただし「ローカル SMTP リレー」（127.0.0.1:25 の postfix）は自前メールのよくある
// 構成で、一律に塞ぐと利用者が詰まる。そこでハードコードで許可するのではなく、
// 明示的な非常口を用意する：ARTEX_NOTIFY_ALLOW_LOCAL=1 を設定すれば許可される。
//
// AllowLocalTargetsEnv としてエクスポートしているのは、テストが明確に有効化できる
// ようにするため——本パッケージと server パッケージのケースは 127.0.0.1 上の
// httptest の偽受信先を多用しており、有効化しないとすべてガードに止められる。
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP は宛先 IP が「既定では配信を許可しない」アドレス帯に属するかを
// 報告する。
//
// 拒否するのはループバック、リンクローカル（クラウドメタデータ 169.254.169.254 を
// 含む）、未指定、マルチキャストのみ。RFC1918 のプライベートネットワークは
// **拒否しない**：内部に自前で立てた Mattermost や SMTP リレーはよくある正当な
// 使い方で、これらまで塞ぐと実環境で機能がまったく使えなくなる。この取捨は意図的
// ——防御は本当に機微な宛先を止めつつ、正常なデプロイをまとめて無効にしてはならない。
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6（::ffff:127.0.0.1）は IPv4 に戻してから判定する。
	// そうしないと検査を回避できる。
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial は http.Transport のダイヤラの Control フックで、**接続確立
// 時**に宛先アドレスを検査する。
//
// 設定保存時の検証だけでなくダイヤル段階に置く理由：ここが最終的な適用点だから。
// これにより設定検証を回避する 2 つのケースを同時にカバーする——DNS リバインディング
// （検証時は公衆 IP に解決され、実際の接続時は内部ネットワークに解決される）と
// リダイレクト（クロスホストのジャンプはすでに拒否しているが、同一ホスト内のジャンプ
// でもパスを別の場所へ向けられる）。
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("宛先アドレス %q を解析できません", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("ローカル／リンクローカルアドレス %s への配信を拒否しました（本当にローカルサービスへ配信する場合は %s=1 を設定してください）", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport は既定の Transport にダイヤルのガードを 1 つだけ加えたもの。
// Clone を使って既定の全チューニング（コネクションプール、HTTP/2、タイムアウト、
// proxy など）を保持し、検査を 1 つ加えるために他の挙動を変えないようにする。
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient は全チャネルの配信が共有するクライアント。
//
// プロジェクトのグローバル出口プロキシ（server 側の GlobalProxy）を**あえて**
// 再利用しない：あのプロキシはペネトレーションテスト対象のトラフィック用で、
// 不安定なトンネルであることが多く、通知の可用性を対象ネットワークの揺らぎに
// 人質に取られるべきではない。IM 配信は直接接続で十分。タイムアウトは 15 秒
// ——これより遅い相手は実質的に障害状態である。
//
// クロスホストのリダイレクトを拒否する：本機能の配信アドレスはすべて「固定の
// エンドポイント 1 つ」の形で、通常は別ホストへリダイレクトされない。しかも各社の
// 資格情報（DingTalk の access_token、WeCom の key、Telegram の bot token）は
// **URL の中に**あるので、クロスホストのジャンプを追うことは資格情報をリダイレクト
// 先へ渡すことに等しい。同一ホスト内のジャンプ（末尾スラッシュの補完など）は
// 引き続き許可する。
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("リダイレクト回数が多すぎます")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("クロスホストのリダイレクトを拒否しました（%s → %s）", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit はレスポンスボディを読み取るサイズを制限する。相手が異常なときは
// 巨大な内容を返しうるが、必要なのはエラーコードと配信履歴に表示するための
// 短いエラー説明だけ。
const respBodyLimit = 8 << 10

// doJSON はリクエストを 1 回送信し、レスポンスボディ（長さ制限済み）を返す。
//
// payload が nil のときは空の body を送る（GET、またはプラットフォームが body を
// 要求しない場面向け）。headers のキーと値はそのまま付加し、汎用 Webhook の
// カスタムヘッダーに使う。
//
// エラーの分類がこの関数の中核的な責務：ネットワーク層の失敗と 5xx/408/429 は
// 「再試行可能」、その他の 4xx は「恒久的失敗」——403 を再試行しても同じエラーを
// 3 回ログに吐くだけ。
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// シリアライズ失敗はローカルのバグ（設定フィールドの型違い）で、再試行しても直らない。
			return nil, Permanent(fmt.Errorf("リクエストボディの構築に失敗しました: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// URL が不正——多くはユーザーがアドレスを書き間違えたケースで、恒久的失敗に当たる。
		// ここでも err をそのまま通してはならない：url.Parse のエラーテキストには
		// 完全なアドレスが入っている。
		return nil, Permanent(fmt.Errorf("リクエストアドレスが不正です: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// 接続拒否、DNS 失敗、タイムアウト——多くは一時障害で、バックオフ再試行に委ねる。
		//
		// エラーテキストはマスクしてから外へ渡さなければならない。理由：http.Client.Do
		// が返すのは *url.Error で、その Error() は `Op "完全なURL": 下位エラー` の形を
		// しており、本機能の各社の資格情報は**URL の中に**ある
		// （DingTalk の access_token、WeCom の key、Lark の hook id、Telegram の
		// /bot<token>/）。マスクしないと、資格情報はこのエラー文字列を伝って 4 か所へ
		// 流れる：notification_deliveries の last_error（平文で DB に保存）、配信履歴
		// API のレスポンス（**チャネル設定のマスクを迂回する**）、サーバーログ、
		// そしてテスト送信 API がフロントへ返す 502 のテキスト。
		return nil, fmt.Errorf("リクエストに失敗しました: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("レスポンスの読み取りに失敗しました: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429（レート制限）と 408（タイムアウト）は再試行に値する。その他の 4xx は
	// 設定または権限の問題で、再試行しても無意味。
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("相手側のレート制限またはタイムアウト (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("相手側サービスの異常 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("相手側がリクエストを拒否しました (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet はレスポンスボディをエラーメッセージ用の短い 1 行テキストに圧縮する。
// レスポンスには改行や大量の空白が含まれうるので、そのまま last_error に詰めると
// 配信履歴ページのレイアウトが崩れる。
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget は配信アドレスを「scheme://host/…」に圧縮し、エラー
// メッセージに使う。
//
// これは本パッケージ唯一のアドレスマスク基準で、あえて**十分に乱暴**に作って
// いる：scheme と host 以外は一律に捨てる。URL のどの部分が資格情報かを判断する
// 「汎用的かつ安全な」方法が存在しないため：
//
//	DingTalk   資格情報は query      /robot/send?access_token=xxx
//	WeCom      資格情報は query      /cgi-bin/webhook/send?key=xxx
//	Lark      資格情報は**パス末尾** /open-apis/bot/v2/hook/<hook_id>
//	Telegram   資格情報は**パス中間** /bot<token>/sendMessage
//
// 「有用な部分だけ残す」にはチャネルごとにパッチを当てることになり、どれか 1 社を
// 漏らせば資格情報の漏洩になる。host を残せば調査には十分で（DNS が引けない、
// 接続できない、証明書が違う、いずれも特定できる）、具体的にどのボットかはチャネル
// 設定のマスク末尾のヒントで識別すればよい。
//
// 解析に失敗したときは固定のプレースホルダーを返す——元の文字列は決して表示しない。
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(アドレスを解析できません)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError はトランスポート層のエラーからアドレスを剥がし、下位の
// 原因だけを残す。
//
// *url.Error の構造は {Op, URL, Err} で、Error() は URL も一緒に出力してしまう。
// ここでは Err フィールドを明示的に取り、その Error() を迂回する——事後に文字列
// 置換するより信頼できる。置換は URL エンコード／エスケープ後の多様な変種に正しく
// 対応する必要があり、漏れやすい。
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: 不明なエラー", uerr.Op, host)
	}
	// *url.Error 以外（リダイレクトポリシーが返すエラーなど）もアドレスを含みうるので、
	// 一律にマスク処理を通す。
	return redactURLsInText(err.Error())
}

// redactURLsInText はテキスト中に現れる http(s) アドレスをマスク形態に置換する。
//
// 構造化フィールドを取れないエラー（リダイレクトポリシーのエラー、サードパーティ
// ライブラリの独自エラー）のフォールバック用。http/https 接頭辞のみを認識し、
// 空白と引用符で区切る——アドレスにこれらの文字は含まれない。
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
