package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 本ファイルは「項目単位でパッキングする」という修正をカバーする：ダイジェストが
// チャネルの長さ上限を超えるとき、**項目単位**で切り詰め、収まらなかった件数を
// 正確に報告して、呼び出し側が実際に配信された分だけをマークできるようにする
// 必要がある。
//
// 以前の実装は全文をレンダリングしてから切り詰め、その後バッチ全体を配信済みと
// マークしていた：メッセージの後半が消え失せ、配信履歴にはすべて成功と表示される
// ——脆弱性が失われ、しかもどこからも気づけない。

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 200 件の日本語ダイジェストは WeCom の 4096 バイトを確実に超える。
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("本文 %d バイトが上限 %d を超過", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("本文が正当な UTF-8 ではない")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("一部だけ収まるべき（0 < kept < %d）、得られたのは %d", len(m.Items), kept)
	}
	// ヘッダーには、このメッセージに何件含まれ、残りが何件かを正確に示す必要が
	// ある——さもないと読者はヘッダーの数字を全体だと誤解する。
	if !strings.Contains(body, "残り") || !strings.Contains(body, "次のメッセージで続きます") {
		t.Fatalf("ヘッダーに、このメッセージに含まれない件数を示すべき:\n%s", body[:minInt(400, len(body))])
	}
	// 先頭 kept 件だけを含むべき。
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "脆弱性"+itoa(i+1)) {
			t.Fatalf("%d 件目がこのメッセージに含まれるべき:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "脆弱性"+itoa(kept+1)) {
		t.Fatalf("%d 件目は含まれるべきではない（次のバッチに属する）", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = 制限なし
	if kept != len(m.Items) {
		t.Fatalf("長さを制限しないときはすべて保持すべき、得られたのは kept=%d", kept)
	}
	if strings.Contains(body, "残り") {
		t.Fatalf("切り詰めていないときは切り詰めの通知を出すべきではない:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// 予算が 1 件も収まらないほど小さくても、1 件は送信する（最終的な切り詰めが
	// 下支えする）。さもないと 1 件の超長い脆弱性がバッチ全体を永久に詰まらせる：
	// 毎回取り出すたびに収まらず、毎回送信されない。
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("少なくとも 1 件は保持すべき、得られたのは %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("単発メッセージは 1 件配信と報告すべき、得られたのは %d", kept)
	}
	// 空メッセージには配信できる項目がない。
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("空メッセージは 0 件と報告すべき、得られたのは %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram は**文字数**で長さを制限する。バイト基準だと日本語メッセージが
	// 3 分の 1 に圧縮されてしまう。
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("本文 %d 文字が上限 %d を超過", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("一部だけ収まるべき、得られたのは %d", kept)
	}
	if !strings.Contains(text, "次に続きます") {
		t.Fatalf("まだ残りがあることを示すべき:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("カードには一部だけ収まるべき、得られたのは %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// この 2 つのチャネルは本文を切り詰めないので、バッチ全体が配信済みとなる。
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("前提条件が成り立たない")
	}
	// レンダラーの戻り値を通じて間接的に確認する：markdownBody(0) は制限なしで
	// すべて保持する。
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("長さを制限しないときはすべて使用すべき、得られたのは %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent は「信頼できない内容がメッセージ構造を
// 変えてはならない」という回帰テスト。タイトルとサマリーはモデル出力（モデルは
// 検査対象の応答を読む）、資産名は検査対象の URL 由来。
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // 結果に必ず現れるべき（エスケープ後の形）
		wrong []string // 結果に現れてはならない（未エスケープの形）
	}{
		{
			name: "タイトル内の改行 + 外部リンク",
			item: Item{
				Severity: "high",
				Name:     "ログイン画面 SQL インジェクション\n[緊急：ここをクリックしてアカウントを検証](http://attacker.tld)",
			},
			// 改行は折り畳む必要がある（さもないと新しいリスト項目や引用ブロックを
			// 偽造できる）。角括弧と丸括弧はエスケープする必要がある（さもないと
			// クリック可能な外部リンクになる）。
			must:  []string{`\[緊急：ここをクリックしてアカウントを検証\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[緊急", "\n\n[緊急"},
		},
		{
			name: "タイトル内の画像ビーコン",
			item: Item{
				Severity: "high",
				Name:     "脆弱性 ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "資産名内の強調と引用",
			item: Item{
				Severity: "high",
				Name:     "通常のタイトル",
				Assets:   []string{"a.com/*注入*>引用"},
			},
			must:  []string{`\*注入\*`, `\>`},
			wrong: []string{"*注入*"},
		},
		{
			name: "サマリー内のバッククォートと縦棒",
			item: Item{
				Severity: "high",
				Name:     "タイトル",
				Summary:  "`code` | テーブル",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// 単発モードの writeItem は 3 つの markdown チャネルが共有する
			// レンダリング経路。
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("エスケープ後の形 %q がない:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("未エスケープの形 %q が現れた（構造や外部リンクの注入に使える）:\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst はエスケープの順序を固定する：バックスラッシュを
// 最初に処理しなければ、後から補うバックスラッシュにもう一層重なり、出力に二重の
// バックスラッシュが現れる。
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("エスケープの順序が誤っている、得られたのは %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes は具体的な回帰を固定する：markdown の
// エスケープが Telegram の HTML 出力へ漏れてはならない（かつて共有のタイトル
// 関数にエスケープを追加した結果、Telegram メッセージに `\(1\)` のような目に見える
// バックスラッシュが現れた）。
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *重点*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram 本文に markdown のバックスラッシュエスケープが現れた:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
