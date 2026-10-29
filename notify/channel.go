package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel は通知チャネルのアダプタ。実装は**ステートレス**でなければならない：
// 同じインスタンスが複数のチャネル設定で並行に再利用され、資格情報は一律に
// cfg 引数から渡される。
type Channel interface {
	// Kind はチャネル種別の識別子を返す。レジストリのキーと一致させること。
	Kind() string
	// Validate は設定保存時に呼ばれ、必須フィールドと形式を検証する。返すエラーは
	// 設定者にそのまま表示されるため、メッセージは「どのフィールドが欠けているか」を
	// 説明すること。曖昧な「設定が無効です」ではいけない。
	Validate(cfg map[string]any) error
	// Send はメッセージを 1 回配信し、**実際に届いた件数**とエラーを返す。
	//
	// なぜ件数を返すのか：各プラットフォームにはメッセージ長の上限があり、集約
	// メッセージがバッチ全体を収めきれないときは切り詰められる。呼び出し側が無条件
	// にバッチ全体を配信済みと記録すると、切り詰められた項目は消えてしまう——メッセージ
	// にも見えず、配信履歴にも成功と表示され、どこからも「脆弱性が一度も送られて
	// いない」ことに気づけない。kept を返せば、呼び出し側は先頭 kept 件だけを配信
	// 済みとし、残りは次のバッチに回せる。
	//
	// エラーを返すのは配信失敗を表し、うち *PermanentError は再試行すべきでない
	// ことを表す。失敗時は kept は意味を持たないので、呼び出し側は無視すること。
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin はそのチャネルが公式に推奨する毎分配信上限を返す。
	// チャネルインスタンス新規作成時の既定レート制限値として使う。0 は既知の
	// 制限なしを表す。
	DefaultRatePerMin() int
	// SecretKeys はそのチャネル設定のうち資格情報に当たるキー名を返す。API 応答では
	// これらのキーの値がマスクされ、更新時にマスク値を受け取ったら DB の元の値を
	// 保持する。どのフィールドが資格情報かは実装自身しか知らない
	// （WeCom は Webhook アドレス全体が資格情報だが、DingTalk はそのうちの secret
	// だけ）ので、この知識はチャネルが提供する必要があり、上位層が推測してはならない。
	SecretKeys() []string
	// DestinationKeys はそのチャネル設定のうち「メッセージをどこへ送るか」を
	// 決めるキー名を返す。
	//
	// SecretKeys と同じくセキュリティに関わるもの：宛先アドレスと資格情報は
	// 独立した 2 系統のフィールドであり、もし「アドレスだけ変えて資格情報はそのまま」
	// を許すと、チャネル設定を変更できる者は誰でも DB の本物の資格情報を自分が
	// 支配するサーバーへ送りつけられる。チャネル設定のマスクが完全に無意味になる。
	// 詳しくは PrepareConfigUpdate を参照。
	DestinationKeys() []string
}

// registry はチャネルのレジストリ。あえて init() による自己登録ではなく明示的な
// リテラルにしている：こうすれば「どのチャネルがあるか」が 1 か所で見渡せ、
// チャネル追加時の漏れは実行時の副作用ではなくコンパイル時に露見する。
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get は種別からチャネル実装を取得する。
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind は kind がサポートされるチャネル種別かどうかを報告する。
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds はサポートされる全チャネル種別を辞書順で返す（UI のドロップダウンを
// 安定して表示するため）。
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError は再試行すべきでない配信失敗を表す：資格情報の誤り、宛先による
// 拒否、不正なリクエストボディなど。再試行が意味を持つのは一時的な障害
// （ネットワークの揺らぎ、レート制限、相手側の 5xx）だけであり、恒久的な失敗に
// 対してバックオフ再試行を繰り返しても成功しないうえ、本当のエラーが再試行ログに
// 埋もれて見えなくなる。
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent は err を恒久的失敗としてマークする。err が nil のときは nil を
// 返すので、`return Permanent(someCheck())` と書けて便利。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent は err の連鎖に恒久的失敗のマークが付いているかを報告する。
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- 設定読み取りヘルパー ----
//
// チャネル設定はデータベースの JSONB 列から来て、encoding/json で
// デシリアライズすると map[string]any になり、数値は一律 float64、配列は []any
// になる。以下のヘルパーはこの変換層を統一し、ユーザーが UI で空欄にしたことによる
// 型のずれ（ポートを文字列で入力するなど）を許容する。

// cfgString は文字列設定項目を取得し、前後の空白を一律に除去する——Web フォーム
// からのコピー＆ペーストでは付きやすい。
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt は整数設定項目を取得し、float64（JSON の既定）と文字列の両方の由来に
// 対応する。
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool は真偽値設定項目を取得し、文字列 "true"/"1" にも対応する。
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings は文字列配列設定項目を取得し、空白を自動で除去して空文字列を捨てる。
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// 値が 1 つだけのときのフォーム送信を考慮し、単一の文字列も受け付ける。
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap は文字列マップ設定項目（カスタム HTTP ヘッダーなど）を取得し、キーと値の
// 両方の空白を除去して空のキーを捨てる。
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
