package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// feishuChannel は Lark のカスタムボットを実装し、インタラクティブ
// カードを使う。
//
// プラットフォームの特性：
//   - 署名アルゴリズムが DingTalk と**異なり**、しかも間違えやすい。feishuSign の
//     コメントを参照。
//   - DingTalk と同様、業務エラーを HTTP 200 の body に詰めて返す（code != 0）。
//   - カードの header は色テンプレートに対応しており、レベルを配色に写像して、
//     メッセージ一覧で深刻度が一目で分かるようにする。
type feishuChannel struct{}

func (feishuChannel) Kind() string { return KindFeishu }

// Lark のカスタムボットは約 5 回/秒、換算して 100 回/分。
func (feishuChannel) DefaultRatePerMin() int { return 100 }

// Webhook アドレスの末尾がボットの一意な識別子であり、資格情報に当たる。
func (feishuChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 同様：Webhook アドレスを変えるなら、新しいアドレスに対して署名キーを再提示
// しなければならない。
func (feishuChannel) DestinationKeys() []string { return []string{"webhook"} }

func (feishuChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook アドレスがありません")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook アドレスが無効です: %w", err)
	}
	return nil
}

func (c feishuChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	card, kept := feishuCard(m)
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     card,
	}
	// 署名パラメータはメッセージと同じ階層に置き、secret が設定されているときだけ
	// 現れる。
	if secret := cfgString(cfg, "secret"); secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		payload["timestamp"] = ts
		payload["sign"] = feishuSign(ts, secret)
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		// 一部バージョンの Lark hook はこのフィールド名を使うので、併せて対応する。
		StatusCode    int    `json:"StatusCode"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Lark のレスポンス解析に失敗しました: %w (%s)", err, snippet(raw))
	}
	if res.Code != 0 {
		return 0, Permanent(fmt.Errorf("Lark がエラーを返しました %d: %s", res.Code, res.Msg))
	}
	if res.StatusCode != 0 {
		return 0, Permanent(fmt.Errorf("Lark がエラーを返しました %d: %s", res.StatusCode, res.StatusMessage))
	}
	return kept, nil
}

// feishuSign は Lark の公式規則に従って署名を計算する。
//
// ここは特に嵌まりやすい：公式のサンプルは
//
//	hmac.new(string_to_sign.encode(), digestmod=sha256)
//
// つまり **key = timestamp + "\n" + secret、message は空**であり、直感に反して
// 「key=secret, message=stringToSign」ではない——それは DingTalk のアルゴリズム。
// 両者のアルゴリズムはちょうど入れ替わっており、他社の実装を真似て書くと必ず
// 署名検証に失敗する（19021 が返る）。
func feishuSign(timestamp, secret string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// feishuSeverityTemplate は脆弱性レベルをカード header の配色テンプレートに
// 写像する。未知のレベルは grey——blue は使わない。low と紛らわしいため。
func feishuSeverityTemplate(severity string) string {
	switch severity {
	case "critical":
		return "red"
	case "high":
		return "orange"
	case "medium":
		return "yellow"
	case "low":
		return "blue"
	default:
		return "grey"
	}
}

// feishuMaxCardBytes はカード内容の控えめな上限。Lark はカードにサイズ制限があり、
// 超過するとメッセージ全体が拒否される。公式上限より明らかに低い値を取り、JSON
// ラップのオーバーヘッドも算入する。
const feishuMaxCardBytes = 24000

// feishuCard はインタラクティブカードを構築し、カードと**実際に書き込まれた
// 件数**を返す。kept の用途は markdownBody と同じ：本当にカードに入った項目だけが
// 配信済みとマークされるべき。
func feishuCard(m Message) (map[string]any, int) {
	elements := []any{}
	kept := 0
	if m.Batch {
		// 先に項目単位で詰めてからヘッダを組み立てる：ヘッダには「残り N 件は次の
		// メッセージで続きます」と書く必要があり、N は実際に収まった件数から
		// 出さなければならない。
		kept = packItemCount(m.Items, feishuMaxCardBytes, markdownReservedBytes, "", byteSize, func(it Item, idx int) string {
			return feishuBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		elements = append(elements, feishuMarkdownDiv(markdownBatchIntro(m, items, len(m.Items))))
		for i, it := range items {
			elements = append(elements, feishuMarkdownDiv(feishuBatchLine(it, i+1)))
		}
		if m.HomeURL != "" {
			elements = append(elements, feishuButton("プラットフォームですべて表示", m.HomeURL))
		}
	} else if len(m.Items) > 0 {
		kept = 1
		it := m.Items[0]
		elements = append(elements, feishuMarkdownDiv(feishuItemLines(it)))
		if it.DetailURL != "" {
			elements = append(elements, feishuButton("詳細を見る", it.DetailURL))
		}
	}

	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   map[string]any{"title": map[string]any{"tag": "plain_text", "content": markdownTitle(m)}},
		"elements": elements,
	}
	if len(m.Items) > 0 {
		card["header"].(map[string]any)["template"] = feishuSeverityTemplate(m.Items[0].Severity)
	}
	return card, kept
}

func feishuMarkdownDiv(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

func feishuButton(label, url string) map[string]any {
	return map[string]any{
		"tag": "action",
		"actions": []any{map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "lark_md", "content": label},
			"url":  url,
			"type": "primary",
		}},
	}
}

// feishuItemLines は脆弱性 1 件の lark_md 本文をレンダリングする。
//
// lark_md は markdown と同族のテキスト形式で、同じくリンクと強調を解釈するので、
// 外部由来のフィールドは一律に markdownText（1 行化 + エスケープ）を通す——
// そうしないと、脆弱性タイトル 1 つが Lark の中でクリック可能な外部リンクになる。
func feishuItemLines(it Item) string {
	out := fmt.Sprintf("**%s · %s**", SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if it.IsStatusChange() {
		out += fmt.Sprintf("\n**ステータス変更**：%s → %s",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		out += fmt.Sprintf("\n**タイプ**：%s", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		out += fmt.Sprintf("\n**資産**：%s", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			out += fmt.Sprintf("\n**サマリー**：%s", s)
		}
	}
	return out
}

// feishuBatchLine は集約カード内の 1 件をレンダリングする。
func feishuBatchLine(it Item, index int) string {
	line := fmt.Sprintf("**%d. %s · %s**", index, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		line += " — " + markdownText(a, 0)
	}
	return line
}
