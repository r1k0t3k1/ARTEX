package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel は汎用 Webhook アダプタ：ユーザーが URL、メソッド、リクエスト
// ヘッダー、JSON テンプレートを自由に定義できる。これがあるおかげで、本機能は
// Slack / Mattermost / Discord / 自前システムのために個別の実装を書かずに済む——
// それらはすべて設定可能なテンプレート 1 つでカバーできる。
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// 汎用 Webhook には公式の制限がないので、0 を返して既定ではレート制限なしとし、
// 利用者が相手側の能力に応じて自分で設定する。
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// url と headers をマスクする：宛先アドレス自体に token が付いていることが多く、
// カスタムヘッダーには認証資格情報が入っているのが普通で、どちらも API 応答に
// 現れるので両方とも塞ぐ必要がある。
// 代償として、編集時にどちらかのヘッダーを変更したい場合はヘッダー群を丸ごと
// 入力し直す必要がある（マスク値は「元の値を維持」と解釈される）——この取捨は
// 意図的で、1 回多く入力する方が資格情報をブラウザに返すよりまし。
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// 宛先は url。url を変えるときは headers を再提示しなければならない——
// そうしないと元の Authorization ヘッダーがそのまま新しいアドレスへ送られ、
// これがマスク回避の主要経路になる。
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate はテンプレート未設定時のフォールバックリクエストボディ：
// 素直な JSON 構造で、「JSON を 1 件受け取って保存する」大半の自前受信先をカバーする。
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData はユーザーテンプレートに公開するコンテキスト。
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt は今回の配信時刻（RFC3339）で、受信側の記録用。
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel はステータス変更の可読な説明で、「未対応 → 修正済み」など。
	// ステータス変更でないときは空。
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("宛先 URL がありません")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("宛先 URL が無効です: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("サポートされていないメソッド %s です（GET/POST/PUT/PATCH が使用可能）", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("リクエストボディテンプレートの構文エラー: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET はリクエストボディを伴わない：内容を query に詰めるのはテンプレートの
	// 能力範囲を超え、GET の意味論にも合わない。したがって GET は「ヒットしたら
	// フックを発火させる」タイプの受信先にだけ適する。
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// テンプレートがレンダリングするのは文字列形式の JSON なので、ここで
		// json.RawMessage に変換してそのまま送る。二重エスケープでユーザーが丹精
		// 込めて組んだ構造を 1 つの JSON 文字列に包み込んでしまわないため。
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("リクエストボディテンプレートのレンダリング結果が正当な JSON ではありません"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// 上書きは許可するが、headers の後に適用し、明示的な設定を優先させる。
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// 汎用 Webhook は本文を切り詰めない（受信先はユーザー自身のサービスで、
	// サイズは body_template が決める）ので、バッチ全体が配信済みとみなされる。
	return len(m.Items), nil
}

// renderWebhookBody はユーザーテンプレート（または既定テンプレート）でリクエスト
// ボディをレンダリングする。
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("リクエストボディテンプレートの構文エラー: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("リクエストボディテンプレートのレンダリングに失敗しました: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate はテンプレートを解析する。
//
// missingkey=zero により、存在しない map キーはエラーでなくゼロ値として
// レンダリングされる——ただし本ファイルのコンテキストは構造体なので、主な効果は
// .Items が空のときに range がエラーにならないこと。本当に防ぐべきは .Items が
// nil であること。
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs はテンプレートに公開する補助関数。
var webhookTemplateFuncs = template.FuncMap{
	// json は任意の値を JSON にシリアライズする。
	//
	// この関数はおまけではなく必須：これがなければユーザーは {{.Title}} と直接
	// 補間するしかなく、脆弱性タイトルに引用符や改行が 1 つでもあればリクエスト
	// ボディ全体が正当な JSON でなくなる——受信先は拒否し、しかもエラーは
	// 「JSON 解析失敗」を指すので、タイトルに引用符があることなど全く連想できない。
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons は JSON 断片を別の JSON 文字列値の内部に埋め込む（文字列エスケープを
	// 1 層かける）。
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// 外側の引用符を外す：引用符を付けるかどうかは呼び出し側が決める。
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
