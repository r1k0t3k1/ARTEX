package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// これは本パッケージで最も重要な不変条件。WeCom は**バイト**数で長さを制限し、
	// 日本語は 3 バイト/文字なので、バイト単位でぶった切る実装は必ず文字を半分に
	// して不正な UTF-8 を生み、プラットフォームに拒否される。長さが互いに素な
	// 多種の日中混在入力を用意し、あらゆる切れ目を突く。
	inputs := []string{
		"日本語テスト内容",
		"混合 mixed 内容 content",
		"a日b本c語dテe",
		"🔴🟠🟡🔵", // 4 バイトの emoji で、切れ目の誤りがより明白
		strings.Repeat("脆弱性", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("入力 %q max=%d: 不正な UTF-8 を生成 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("入力 %q max=%d: 結果 %d バイトが上限を超過", in, max, len(got))
			}
			// 切り詰められていないときは内容を変えてはならない。
			if len(in) <= max && got != in {
				t.Fatalf("入力 %q max=%d: 上限内なのに内容を変更 -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 は制限なしを意味すべき")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 は制限なしを意味すべき")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// max が省略記号そのものより小さいとき、省略記号の追加で逆に上限を超えてはならない。
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("max=1 のとき結果 %q の長さ %d が上限超過", got, len(got))
	}
	// 通常は省略記号が付くべき。
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("省略記号が付くはずが、得られたのは %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// TruncateBytes との基準の違いは保持しなければならない：Telegram は文字数で
	// 長さを制限し、バイト基準だと日本語メッセージが 3 分の 1 に切られてしまう。
	s := "一二三四五六七八九十"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("5 文字を期待、得られたのは %d 文字 (%q)", n, got)
	}
	// 同じ文字列をバイト基準で切ると、明らかに短くなるはず。
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("バイト基準で文字基準と同じ文字数を生んではならない")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("1 行目\n\n2 行目\tタブ付き   複数空白", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("空白をすべて畳むべき、得られたのは %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("連続空白を残してはならない、得られたのは %q", got)
	}
	// 切り詰め後も読め、かつ正当でなければならない。
	got = OneLine("一二三四五六七八九十", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("4 文字を期待、得られたのは %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// HTML をそのまま切り詰めると `<a href="htt` のような残片が生まれ、プラット
	// フォームはメッセージ全体を拒否する。
	s := `<b>タイトル</b>本文本文本文<a href="https://example.com/very/long/path">詳細を見る</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: 結果 %d 文字が上限超過", max, n)
		}
		// 末尾に閉じられていない `<` があってはならない（最後の区間に `<` はあるが `>` がない状態）。
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: 末尾のタグが切断された -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("資産なしは空文字列を返すべき、得られたのは %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a、b" {
		t.Fatalf("上限内なら全件列挙すべき、得られたのは %q", got)
	}
	// 上限超過時は総数を明記しなければならない。そうでないと読者は資産が何件
	// 残っているか分からない。
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "ほか 5 件") {
		t.Fatalf("総数 5 を明記すべき、得られたのは %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("空レベルの序数は 0 なので、あらゆるしきい値に阻まれるべき")
	}
	if !AtLeast("critical", "") {
		t.Fatal("空のしきい値は通過させるべき")
	}
	if got := StatusLabel("fixed"); got != "修正済み" {
		t.Fatalf("ステータスの対応付けが誤り、得られたのは %q", got)
	}
	// 未知のステータスはそのまま返し、勝手にラベルを作らない。
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("未知のステータスはそのまま返すべき、得られたのは %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity は監査で指摘された 1 つの漏れをカバーする：
// 切り詰めでは中途半端なタグだけでなく、切断された HTML エンティティも避ける
// 必要がある。
//
// `&amp;` が `&amp` に切られると、エンティティしか解釈しないパーサーが**メッセージ
// 全体**を拒否しうる——長すぎる集約メッセージはよくあるので、代償が大きすぎる。
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// 末尾に「& はあるが対応する ; がない」エンティティ残片が現れてはならない。
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: 末尾にエンティティ残片 %q が残った", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: 不正なエンティティが発生", max)
		}
	}
}
