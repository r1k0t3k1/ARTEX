package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes は s を max バイト以内に切り詰め、結果が正当な UTF-8 で文字を
// 分断しないことを保証する。
//
// なぜ文字境界で切らなければならないか：WeCom グループボットの markdown には
// 4096 **バイト**の厳格な上限があり（文字数ではない）、一方で日本語の 1 文字は
// 3 バイトある。バイト単位でそのままスライスすると漢字 1 文字を真っ二つにし、
// 不正な UTF-8 を生む——プラットフォーム側ではメッセージ全体を拒否するか、
// 文字化けした豆腐四角として表示される。ここではまず予算位置から最も近い
// rune の開始バイトへ後退する（utf8.RuneStart が継続バイト 0b10xxxxxx を判定）。
//
// max<=0 は制限なしを意味する。切り詰め後は省略記号を追加するが、max が
// 省略記号すら収まらないほど小さい場合は除く。
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max が省略記号より短い：省略記号を諦めて純粋な切り詰めにし、
		// かえって max を超える結果になるのを避ける。
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine は複数行のテキストを 1 行に圧縮する：空白をすべて畳んでから文字数で
// 切り詰める。IM メッセージのタイトル行に使う——要約には改行が多いので、そのまま
// 表組みやタイトルに押し込むとレイアウトが崩れる。max<=0 は長さ制限なし。
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes は s を max 文字（バイトではなく）以内に切り詰め、超過時は
// 省略記号を付ける。max<=0 は制限なし。
//
// TruncateBytes との違いはプラットフォームの計量基準にある：WeCom はバイト数、
// Telegram は文字数で長さを制限する。基準を間違えてもエラーにはならず、メッセージが
// 想定よりずっと短く切られるだけ（日本語 1 文字 = 3 バイトなので、バイトで 4096
// 切ると約 1365 文字しか残らない）。したがって両方の関数を残し、チャネルごとに
// 使い分けなければならない。
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML は HTML 断片を文字数で切り詰め、中途半端なタグを生まないことを
// 保証する。
//
// HTML をそのまま文字数で切ると `<a href="htt` のような欠けたタグが生まれ、
// プラットフォームのパーサーはエラーでメッセージ全体を拒否するか、後続の本文を
// 属性値として飲み込んでしまう。ここではまず文字数で切り詰め、末尾に閉じられて
// いない `<` があればその手前まで戻す。
//
// タグの対応補完（`</b>` を補うなど）はしない：Telegram の HTML パーサーは
// 閉じ忘れたタグを自動で閉じるし、自前で補完するには属性内の引用符・コメント・
// 自己閉じタグを扱う必要があり、複雑さと得られるものの釣り合いが取れない。
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// 末尾が `<` で始まる残片（最後の `<` より後ろに `>` がない）なら、`<` の手前へ戻す。
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// 末尾が切断された HTML エンティティ（`&amp;` が `&amp` に切られたなど）の場合も
	// 同様に戻す。エンティティの残片は、エンティティしか解釈しないパーサーでは
	// **メッセージ全体**の拒否につながりうる——長さ上限を超えた集約メッセージは
	// もともとよくあるので、それで通知全体を落とす価値はない。
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount は予算内に**丸ごと**何件収まるかを計算し、集約メッセージを
// 項目単位で詰め込む。
//
// なぜ全体をレンダリングしてから切り詰めるのでなく項目単位で詰めるのか：切り詰め
// ると後半の項目が跡形もなく消えるのに、その配信記録は配信済みとマークされたまま
// になる——メッセージにも配信履歴にも現れず、脆弱性がそうして失われる。項目単位で
// 詰めれば、収まらなかった項目は DB に残って次のバッチになり、呼び出し側が受け取る
// kept がこのメッセージで本当に届いた件数になる。
//
// 引数：maxSize<=0 は制限なし。reserve はメッセージの先頭／末尾に確保する量。
// size は計量を担う（各プラットフォームで基準が異なる：WeCom／DingTalk はバイト、
// Telegram は文字数——基準を間違えてもエラーにはならず、日本語メッセージが上限を
// はるかに下回る長さに圧縮されるだけ）。render は idx 番目を実際のテキストに
// レンダリングする——長さは内容によって変わるので、見積もりではいけない。
//
// 少なくとも 1 件を返す（項目が残っている限り）。単一項目が極端に長い場合もこの
// 1 件は送り、呼び出し側の最終的な切り詰めに任せる。そうしなければ 1 件の超長大
// 脆弱性がバッチ全体を永久にせき止めてしまう。
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize は packItemCount の 2 つの計量基準。名前を付けておくのは
// 呼び出し側に裸の func(s string) int クロージャが現れるのを避けるため——
// そうでないとどちらの基準を使っているのか一目で分からない。
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine は資産リストを 1 行の表示テキストにレンダリングし、limit 件を超える
// ときは残りを省略して総数を示す。1 つの脆弱性に何十もの資産が紐づきうるので、
// 全部並べるとメッセージが溢れる。
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, "、")
	}
	return strings.Join(assets[:limit], "、") + " ほか " + itoa(len(assets)) + " 件"
}

// itoa は strconv.Itoa の短い別名で、表示テキストの連結にだけ使い、
// あちこちで strconv を import しないで済むようにする。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
