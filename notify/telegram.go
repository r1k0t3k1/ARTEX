package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// telegramTextLimit は Telegram sendMessage の text フィールドの上限（文字数）。
const telegramTextLimit = 4096

// telegramChannel は Telegram Bot API を実装する。
//
// プラットフォームの特性：
//   - 認証はすべて URL path 内（/bot<token>/sendMessage）で、署名は不要。
//   - MarkdownV2 ではなく HTML パースモードを使う：MarkdownV2 は `_*[]()~`>#+-=|{}.!`
//     の 18 文字をエスケープする必要があり、1 つ漏らすとメッセージ全体が拒否される。
//     HTML なら & < > の 3 文字だけで済む。
//   - 業務エラーも HTTP 200 の中に隠れており、ok フィールドで判断する。
type telegramChannel struct{}

func (telegramChannel) Kind() string { return KindTelegram }

// Telegram は個人チャットで約 1 通/秒、グループで 20 通/分。控えめな値を取る。
func (telegramChannel) DefaultRatePerMin() int { return 20 }

// Bot Token は完全な資格情報。chat_id は単なる宛先で、秘密ではない（Token が
// なければこれだけ持っていても送れない）。
func (telegramChannel) SecretKeys() []string { return []string{"bot_token"} }

// base_url は Token がどの API エンドポイント（自前のリバースプロキシなど）へ
// 送られるかを決めるので、これを変えるなら Token を再提示しなければならない。
func (telegramChannel) DestinationKeys() []string { return []string{"base_url"} }

func (telegramChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "bot_token") == "" {
		return errors.New("Bot Token がありません")
	}
	if cfgString(cfg, "chat_id") == "" {
		return errors.New("Chat ID がありません")
	}
	if base := cfgString(cfg, "base_url"); base != "" {
		if err := validateHTTPURL(base); err != nil {
			return fmt.Errorf("API アドレスが無効です: %w", err)
		}
	}
	return nil
}

func (c telegramChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := telegramEndpoint(cfg)
	if err != nil {
		return 0, Permanent(err)
	}
	text, kept := telegramHTML(m)
	payload := map[string]any{
		"chat_id":                  cfgString(cfg, "chat_id"),
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": false,
	}
	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Telegram のレスポンス解析に失敗しました: %w (%s)", err, snippet(raw))
	}
	if res.OK {
		return kept, nil
	}
	// 429 はレート制限で、バックオフ後の再試行が有効。それ以外（400 パラメータ誤り、
	// 401 token 誤り、403 ブロック、404 chat 不在）は設定の問題で、再試行しても
	// 自然には直らない。
	if res.ErrorCode == 429 {
		return 0, fmt.Errorf("Telegram のレート制限: %s", res.Description)
	}
	return 0, Permanent(fmt.Errorf("Telegram がエラーを返しました %d: %s", res.ErrorCode, res.Description))
}

// telegramEndpoint は sendMessage のアドレスを組み立てる。base_url が空のときは
// 公式 API を使い、非空のときは自前の Bot API リバースプロキシに使う
// （国内ネットワークではよくある要件）。
func telegramEndpoint(cfg map[string]any) (string, error) {
	base := cfgString(cfg, "base_url")
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimSuffix(base, "/")
	token := cfgString(cfg, "bot_token")
	raw := base + "/bot" + token + "/sendMessage"
	u, err := url.Parse(raw)
	if err != nil {
		// err をそのまま通さない：アドレスに Bot Token が含まれ、この時点では
		// addr さえも表示すべきでない。
		return "", fmt.Errorf("API アドレスの組み立てに失敗しました（API アドレス：%s）", redactRequestTarget(base))
	}
	return u.String(), nil
}

// telegramHTML は HTML 本文をレンダリングし、本文と実際に書き込まれた件数を
// 返す（Channel.Send を参照）。
func telegramHTML(m Message) (string, int) {
	var b strings.Builder
	b.WriteString("<b>" + telegramEscape(markdownTitle(m)) + "</b>\n")
	if m.Batch {
		// Telegram の上限は**文字数**なので、詰め込みも文字数で計量する（runeSize）。
		footer := ""
		if m.HomeURL != "" {
			footer = fmt.Sprintf("\n\n<a href=\"%s\">プラットフォームですべて表示</a>", telegramEscapeAttr(m.HomeURL))
		}
		kept := packItemCount(m.Items, telegramTextLimit, telegramReservedRunes, footer, runeSize, func(it Item, idx int) string {
			return telegramBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		b.Reset()
		b.WriteString("<b>" + telegramEscape(telegramBatchTitle(m, items, len(m.Items))) + "</b>")
		for i, it := range items {
			b.WriteString("\n" + telegramEscape(telegramBatchLine(it, i+1)))
		}
		b.WriteString(footer)
		return TruncateHTML(b.String(), telegramTextLimit), kept
	}
	if len(m.Items) == 0 {
		return b.String(), 0
	}
	it := m.Items[0]
	if it.IsStatusChange() {
		b.WriteString(fmt.Sprintf("\n<b>ステータス変更</b>：%s → %s",
			telegramEscape(StatusLabel(it.FromStatus)), telegramEscape(StatusLabel(it.ToStatus))))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		b.WriteString("\n<b>タイプ</b>：" + telegramEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		b.WriteString("\n<b>資産</b>：" + telegramEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		b.WriteString("\n<b>サマリー</b>：" + telegramEscape(s))
	}
	if it.DetailURL != "" {
		b.WriteString(fmt.Sprintf("\n\n<a href=\"%s\">詳細を見る</a>", telegramEscapeAttr(it.DetailURL)))
	}
	return TruncateHTML(b.String(), telegramTextLimit), 1
}

// telegramReservedRunes はメッセージタイトルと起こりうる切り詰め通知に確保する
// 領域（文字数で計量）。
const telegramReservedRunes = 160

// telegramBatchLine は集約内の 1 件をレンダリングする（未エスケープ。呼び出し側が
// 一括でエスケープする）。
func telegramBatchLine(it Item, idx int) string {
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		return fmt.Sprintf("%d. %s · %s — %s", idx, SeverityLabel(it.Severity), it.Title(), a)
	}
	return fmt.Sprintf("%d. %s · %s", idx, SeverityLabel(it.Severity), it.Title())
}

// telegramBatchTitle は集約メッセージのタイトル行をレンダリングする。件数には
// **このメッセージが実際に含む**件数を使い、バッチの総数は使わない——そうしないと
// 読者はメッセージ冒頭の数字が全部だと誤解する。
func telegramBatchTitle(m Message, items []Item, total int) string {
	title := fmt.Sprintf("脆弱性ダイジェスト · 全 %d 件", total)
	if extra := total - len(items); extra > 0 {
		title += fmt.Sprintf("（先頭 %d 件を表示、残り %d 件は次に続きます）", len(items), extra)
	}
	if m.WindowMinutes > 0 {
		title = fmt.Sprintf("直近 %d 分 · %s", m.WindowMinutes, title)
	}
	return title
}

// telegramEscape は HTML のテキスト内容をエスケープする。
// Telegram はこの 3 種のエンティティしか解釈せず、エスケープ後に &amp; のような
// 既存のエンティティは二重エスケープされる——これはまさに期待通りの挙動：表示
// したいのは元の文字であり、ユーザーに HTML を注入させたいわけではない。
func telegramEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// telegramEscapeAttr は HTML 属性値をエスケープする。テキストのエスケープに加えて
// 引用符も処理する必要がある——URL に引用符が含まれると href 属性を早く閉じ、
// 後続の内容が注入点になってしまう。
func telegramEscapeAttr(s string) string {
	s = telegramEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
