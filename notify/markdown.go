package notify

import (
	"fmt"
	"strings"
)

// 本ファイルは「Markdown 系」チャネル（DingTalk、WeCom）共通のメッセージ
// レンダリング。Lark はカード JSON、Telegram は HTML、メールは HTML で、
// それぞれアダプタ内でレンダリングする。

// maxAssetsShown はメッセージ内に列挙する資産の最大数。1 つの脆弱性に何十もの
// 資産が紐づきうるので、全部並べるとメッセージが溢れるうえ情報価値もない——
// 4 件目以降のドメインは IM では誰も見ない。
const maxAssetsShown = 3

// maxSummaryRunes はサマリーを何文字に圧縮するか。IM メッセージは「詳細を見に
// 行くきっかけ」であり、レポート本体ではない。完全な内容はプラットフォーム側に
// ある。
const maxSummaryRunes = 120

// markdownReservedBytes はメッセージの先頭（集約行 + レベル分布 + 起こりうる
// 切り詰め通知）と末尾（プラットフォームへのリンク）に確保する領域。項目単位で
// 詰め込む際にこの分を予算から差し引き、先頭と末尾が切られないようにする——
// 先頭と末尾が切られると、読者は「これがどのバッチで、あと何件表示されていないか」
// さえ分からなくなる。
const markdownReservedBytes = 320

// markdownEscape は markdown のメタ文字をエスケープする。
//
// なぜ必須か：脆弱性タイトル、サマリー、タイプ、資産表示名はすべて**信頼できない
// 由来**である——タイトルとサマリーはモデルの出力（モデルが読むのはテスト対象の
// 応答）、資産の url はスキャンで得た完全な URL（対象が制御できるクエリ文字列を
// 含む）。エスケープしないと、タイトルが
//
//	ログイン画面 SQL インジェクション\n[緊急：ここをクリックしてアカウント確認](http://attacker.tld)
//
// である脆弱性が、セキュリティエンジニアの DingTalk／Lark で**クリック可能な
// 外部リンク**としてレンダリングされる。しかも `![](http://attacker.tld/beacon)`
// はレンダリング時にクライアントから取得され、「この脆弱性は既に閲覧された」こと
// を通知し、閲覧者の IP まで漏れる。悪意がなくても、注入された太字や引用ブロックは
// 下にある深刻な脆弱性を折りたたみ線の外へ押し出せる。
//
// エスケープ対象はタイトル／リンク／強調／リスト／引用／取り消し線の各類で、
// 構造を変えたりクリック可能な要素を生んだりする文字を網羅する。`\` を最初に
// 処理しないと、後から補ったバックスラッシュを二重にエスケープしてしまう。
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText は信頼できないテキストを 1 行に圧縮してエスケープし、markdown の
// 本文に供する。1 行化はエスケープのもう半分：改行そのものが新しいリスト項目や
// 引用ブロックを偽装でき、エスケープ文字では防げない。
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle はメッセージタイトル（IM プラットフォームのタイトルバー／カード
// タイトル）を返す。内容は**未エスケープの原文**。
//
// ここで意図的にエスケープしないのは、このタイトルを 4 つの文脈のレンダラが
// 共用するため——markdown 本文、Telegram の HTML、Lark カードの plain_text、
// そして汎用 Webhook の JSON とメールの件名。文脈ごとにエスケープ規則が異なり
// （markdown エスケープを HTML に入れると目に見えるバックスラッシュが残り、JSON
// に入れるとデータを汚す）、エスケープは各出力側の責務でなければならない。参照：
// writeItem / feishuItemLines / telegramEscape。かつて共有関数に markdown
// エスケープを足した結果、Telegram メッセージに `\(1\)` のような目に見える
// バックスラッシュが現れた。
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("脆弱性ダイジェスト · 全 %d 件", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "脆弱性通知"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody はメッセージ本文をレンダリングし、本文と**実際に書き込まれた
// 件数**を返す。
//
// 戻り値 kept はこの配信で本当に届いた件数であり、呼び出し側はこれに基づいて
// 先頭 kept 件だけを配信済みとマークする——チャネルの長さ上限で締め出された項目は
// 次のバッチに残すべきで、一緒に成功とマークしてはならない。これこそが「静かな
// 消失」の源：メッセージは切り詰められたのに配信記録は全件配信済みと表示され、
// 後半が一度も送られていないことをどこからも見て取れない。
//
// maxBytes<=0 は制限なし。
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// 単発メッセージは長すぎてもそのまま送る（最終的な切り詰めに任せる）：
		// 脆弱性の部分的な情報でも、1 件も送らないよりまし。
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[プラットフォームですべて表示](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro は集約メッセージの冒頭をレンダリングする：時間窓、件数、
// レベル分布。これがあれば、ダイジェストを受け取った人はプラットフォームに
// 入らずとも、このバッチをすぐ処理すべきか判断できる。
//
// items は**実際に収まった**項目、total はこのバッチ本来の総数。両者が異なるときは
// 「あと何件が次のメッセージにあるか」を必ず明記する——そうしないと読者はメッセージ
// 冒頭の数字が全部だと誤解し、後ろの一度も送られていない項目は画面上にまったく
// 存在しないことになる。
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**直近 %d 分間に %d 件の脆弱性を検出**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**%d 件の脆弱性を検出**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "（このメッセージには先頭 %d 件を表示、残り %d 件は次のメッセージで続きます）", len(items), extra)
	}
	// レベルごとの分布を示し、読者が深刻な項目の有無を一目で見られるようにする。
	// 集計対象は**このメッセージが実際に含む**項目のみで、「深刻 3」と下で数えられる
	// 項目が一致することを保証する。
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem は脆弱性 1 件をレンダリングする。
//
// prefix は集約リストの番号に使う。single=true のときは完全版（サマリーと
// バックリンクを含む）をレンダリングし、集約リストではサマリーを 1 行だけに
// する——さもないと 50 件の集約が長文ドキュメントになる。
//
// 外部由来の内容（タイトル／タイプ／資産／サマリー）はすべて markdownText を
// 通す：1 行化 + エスケープ。バックリンクは管理者が設定した public_base_url を
// 組み立てたもので信頼できない内容ではなく、クリック可能なリンクでなければ
// ならないので、そのまま出力する。
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// 集約モード：1 行で提示し、資産とサマリーを圧縮して後ろに付ける。
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**ステータス変更**：%s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**タイプ**：%s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**資産**：%s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**サマリー**：%s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[詳細を見る](%s)\n", it.DetailURL)
	}
}
