package notify

import (
	"fmt"
	"strings"
)

// 本ファイルはメールの HTML 本文をレンダリングする。現代的な CSS ではなく
// インラインスタイル + シンプルなテーブルレイアウトを意図的に使う：メール
// クライアント（とりわけ Outlook や国内の企業メール）は <style> ブロックや
// flex/grid の対応が大きく異なり、インラインスタイルが各社で正しく表示される
// 唯一の書き方だから。

// htmlSeverityColor はレベルに対応するアクセント色を返す。左側の色帯とタイトルに使う。
func htmlSeverityColor(severity string) string {
	switch severity {
	case "critical":
		return "#d32029"
	case "high":
		return "#e8830c"
	case "medium":
		return "#d4b106"
	case "low":
		return "#1677ff"
	default:
		return "#8c8c8c"
	}
}

// htmlTitle はメールの件名を返す。
func htmlTitle(m Message) string {
	return markdownTitle(m)
}

// htmlBody はメール本文の HTML をレンダリングする。maxRunes<=0 は切り詰めなし。
func htmlBody(m Message, maxRunes int) string {
	var b strings.Builder
	b.WriteString(`<div style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','PingFang SC','Microsoft YaHei',sans-serif;font-size:14px;color:#262626;line-height:1.6;">`)
	if m.Batch {
		b.WriteString(htmlBatchIntro(m))
		for _, it := range m.Items {
			b.WriteString(htmlItem(it, false))
		}
	} else if len(m.Items) > 0 {
		b.WriteString(htmlItem(m.Items[0], true))
	}
	if m.HomeURL != "" {
		fmt.Fprintf(&b, `<p style="margin:16px 0 0;"><a href="%s" style="color:#1677ff;">プラットフォームですべて表示</a></p>`, htmlEscapeAttr(m.HomeURL))
	}
	b.WriteString(`</div>`)
	return TruncateHTML(b.String(), maxRunes)
}

// htmlBatchIntro は集約メールの冒頭をレンダリングする：件数とレベル分布。
func htmlBatchIntro(m Message) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, `<h2 style="font-size:16px;margin:0 0 4px;">直近 %d 分間に %d 件の脆弱性を検出</h2>`, m.WindowMinutes, len(m.Items))
	} else {
		fmt.Fprintf(&b, `<h2 style="font-size:16px;margin:0 0 4px;">%d 件の脆弱性を検出</h2>`, len(m.Items))
	}
	counts := map[string]int{}
	for _, it := range m.Items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf(`<span style="color:%s;font-weight:600;">%s %d</span>`,
				htmlSeverityColor(sev), htmlEscape(SeverityLabel(sev)), n))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(&b, `<p style="margin:0 0 12px;">%s</p>`, strings.Join(parts, " &middot; "))
	}
	return b.String()
}

// htmlItem は脆弱性 1 件をレンダリングする。full=true のときはサマリーと
// バックリンクを含み（単発配信）、false のときは 1 行に圧縮する（集約リスト）。
func htmlItem(it Item, full bool) string {
	color := htmlSeverityColor(it.Severity)
	var b strings.Builder
	if full {
		fmt.Fprintf(&b, `<div style="border-left:4px solid %s;padding:8px 0 8px 12px;margin-bottom:12px;">`, color)
	} else {
		fmt.Fprintf(&b, `<div style="border-left:3px solid %s;padding:4px 0 4px 10px;margin-bottom:8px;">`, color)
	}
	fmt.Fprintf(&b, `<div style="font-weight:600;">%s &middot; %s</div>`,
		htmlEscape(SeverityLabel(it.Severity)), htmlEscape(it.Title()))

	if !full {
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, htmlEscape(a))
		}
		if it.Summary != "" {
			extras = append(extras, htmlEscape(OneLine(it.Summary, 60)))
		}
		if len(extras) > 0 {
			fmt.Fprintf(&b, `<div style="color:#595959;font-size:13px;">%s</div>`, strings.Join(extras, " &middot; "))
		}
		b.WriteString(`</div>`)
		return b.String()
	}

	if it.IsStatusChange() {
		fmt.Fprintf(&b, `<div><b>ステータス変更</b>：%s → %s</div>`,
			htmlEscape(StatusLabel(it.FromStatus)), htmlEscape(StatusLabel(it.ToStatus)))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(&b, `<div><b>タイプ</b>：%s</div>`, htmlEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(&b, `<div><b>資産</b>：%s</div>`, htmlEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		fmt.Fprintf(&b, `<div><b>サマリー</b>：%s</div>`, htmlEscape(s))
	}
	if it.DetailURL != "" {
		fmt.Fprintf(&b, `<div style="margin-top:6px;"><a href="%s" style="color:#1677ff;">詳細を見る</a></div>`, htmlEscapeAttr(it.DetailURL))
	}
	b.WriteString(`</div>`)
	return b.String()
}

// htmlEscape は HTML のテキスト内容をエスケープする。脆弱性タイトルとサマリーは
// テスト対象とモデル出力に由来する信頼できない内容——エスケープしないのは、
// 任意の HTML（外部リンク画像を含む）をメールに注入させるに等しい。
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// htmlEscapeAttr は HTML 属性値をエスケープする（テキストのエスケープに加えて
// 引用符も処理し、URL 内の引用符が href 属性を早く閉じてしまうのを防ぐ）。
func htmlEscapeAttr(s string) string {
	s = htmlEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
