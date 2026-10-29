package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit は WeCom グループボットの markdown content の厳格な上限
// （バイト数、文字数ではない）。全 6 チャネル中で最も厳しい制限であり、
// TruncateBytes が存在する主な理由でもある。
const weComMarkdownLimit = 4096

// weComChannel は WeCom グループボットを実装する。
//
// プラットフォームの特性：
//   - URL 上の key だけで認証し、署名をサポートしない——したがって webhook
//     アドレス自身がすべての資格情報。
//   - markdown content の上限は 4096 **バイト**で、超過するとメッセージ全体が
//     拒否される（切り詰めではない）。日本語は 3 バイト/文字なので、本文に書けるのは
//     1000 文字強しかなく、クライアント側での切り詰めが必須。
//   - レート制限は 20 通/分で、これもクライアント側のレート制限で吸収する。
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom の資格情報は Webhook の 1 か所（URL 上の key）だけで、署名もサポート
// しない——アドレス全体がすべての資格情報であり、他にマスクすべきフィールドはない。
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom のフィールドは Webhook の 1 か所だけで、それが宛先でも資格情報でもある
// ため、「アドレス変更後に残る資格情報」というものは存在しない。
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook アドレスがありません")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook アドレスが無効です: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// 集約バッチは長くなりがち（50 件 × 各 1 行 + プレフィックス）で、4096 バイトを
	// 容易に超える。切り詰めはプラットフォームのエラーに頼らずここで行う：拒否は
	// バッチ全体の消失を意味するが、切り詰めなら少なくとも先頭の数件は届く。
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("WeCom のレスポンス解析に失敗しました: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 は API 呼び出し回数超過——プラットフォームのレート制限窓は
		// ローリングするので、バックオフ後の再試行は有効。明示的に再試行可能として
		// 扱う。ここに来た時点でクライアント側の rate_per_min が攻めすぎている証拠で、
		// 再試行はあくまで保険、本当の対処はそのチャネルのレート制限値を下げること。
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom のレート制限 %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 は webhook key が無効——恒久的失敗で、再試行しても自然には直らない。
		return 0, Permanent(fmt.Errorf("WeCom がエラーを返しました %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
