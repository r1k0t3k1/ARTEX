package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel は DingTalk のカスタムボットを実装する。
//
// プラットフォームの特性（ここでの実装上の取捨を決める）：
//   - 1 ボットあたり 20 通/分のレート制限があり、超過分は黙って破棄される
//     （HTTP は 200 を返しうる）ので、レート制限はクライアント側で行う必要がある。
//     DefaultRatePerMin を参照。
//   - セキュリティ設定は 3 択：署名 / カスタムキーワード / IP ホワイトリスト。
//     署名はメッセージ内容に依存しない唯一の方式なので、署名のみをサポートする
//     （3 つとも無効な生の webhook もサポートする）。
//   - 成功も失敗も HTTP 200 を返し、body の errcode で区別する——errcode を
//     確認しないと配信失敗を成功として記録してしまう。
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk の Webhook アドレスには access_token が含まれ、それ自体が資格情報
// なので、全体をマスクする。
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 宛先は DingTalk の Webhook アドレスそのもの。アドレスを変えるなら、新しい
// アドレスに対して署名キーを再提示しなければならない。
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook アドレスがありません")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook アドレスが無効です: %w", err)
	}
	return nil
}

// Send はメッセージを 1 回配信する。バックリンクがあり単発のときは ActionCard
// （ボタン付き）、それ以外は markdown を使う。
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk の markdown 本文には明確なバイト数上限はないが、証拠フィールドが
	// 異常に膨らむのを避けるため、念のため上限を設ける。
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "詳細を見る",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk は業務エラーを 200 レスポンスの中に隠す。
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("DingTalk のレスポンス解析に失敗しました: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 は署名検証失敗、310000 はキーワード不一致——いずれも設定ミスで、
		// 再試行しても自然には直らない。
		return 0, Permanent(fmt.Errorf("DingTalk がエラーを返しました %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL は公式の署名規則に従い、webhook に timestamp と sign の
// パラメータを付加する。
//
// 規則：署名対象文字列 = timestamp + "\n" + secret、HMAC-SHA256 の**鍵も secret**、
// 結果を base64 にして URL エンコードする。timestamp はミリ秒。secret が空のときは
// そのまま返し、署名を有効にしていないボットに対応する。
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// err をそのまま通さない：url.Parse のエラーテキストには完全なアドレス
		// （access_token を含む）が入っている。
		return "", fmt.Errorf("Webhook アドレスの解析に失敗しました: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL はアドレスが利用可能でプロトコルがサポートされていることを
// 検証し、リテラル IP の宛先には内部ネットワーク判定を行う。
//
// 2 つのこだわり：
//
//  1. **エラーメッセージは必ずマスクする**。url.Parse 自身が返すのは *url.Error で、
//     その Error() には**完全な元のアドレス**が含まれる。しかも本機能の各社の
//     アドレスには資格情報が埋まっている（DingTalk の access_token、WeCom の key、
//     Telegram の bot token、Lark の hook id）。かつてここで `return err` をそのまま
//     返していたため、「アドレス形式が不正」というエラーが資格情報を外へ持ち出し、
//     テスト送信 API の 400 レスポンス、配信ごとに DB へ記録される last_error、
//     サーバーログ、配信履歴 API へと流れていった。
//
//  2. **リテラル IP はその場で内部判定する**。ドメイン名はダイヤル段階での判定に
//     任せる（blockInternalDial が最終的な適用点であり、DNS リバインディングも
//     カバーできる）。ここで一度判定するのは、設定保存時にヒントを得られるように
//     するためで、初回配信の失敗まで待たせないため。
//
// プロトコルを制限するのは防御的措置：file:// や gopher:// などは http.Client に
// 予期しない挙動を生む（scheme の検査で既に弾かれているが、この面を開けておく
// 理由はない）。
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("アドレスを解析できません（%s）", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http/https のみサポートしています。受け取ったのは %q です", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("ホスト名がありません")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("ローカル／リンクローカルアドレス %s への配信を拒否しました（本当にローカルサービスへ配信する場合は %s=1 を設定してください）", ip, AllowLocalTargetsEnv)
	}
	return nil
}
