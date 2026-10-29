package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 検出一覧ページの「エクスポート」用レンダリング：findings の行をまとめて
// サマリー Markdown、単票 Markdown、または CSV に変換する。JSON は server 層が
// DTO を直接シリアライズするのでここでは扱わない。

// sortFindingsForExport は深刻度の降順、次に時刻の降順で並べる。
// サマリーレポートのグループ分けと一致させる。
func sortFindingsForExport(fs []*db.DBFinding) {
	sort.SliceStable(fs, func(i, j int) bool {
		ri, rj := sevRank[fs[i].Severity], sevRank[fs[j].Severity]
		if ri != rj {
			return ri < rj // sevRank が小さいほど深刻
		}
		return fs[i].CreatedAt.After(fs[j].CreatedAt)
	})
}

// findingTitle は脆弱性の可読タイトルを返す：名称 → カテゴリ → 「未分類」。
func findingTitle(f *db.DBFinding) string {
	return nz(f.Name, nz(f.VulnClass, "未分類"))
}

// FindingsMarkdown は findings をまとめて 1 本のサマリーレポートにする
// （概要 + 深刻度別のグループ。各項目にはカテゴリ/状態/所属タスク/証拠/詳細レポートを含む）。
func FindingsMarkdown(fs []*db.DBFinding, generatedAt time.Time) string {
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	var b strings.Builder
	b.WriteString("# 脆弱性検出サマリーレポート\n\n")
	fmt.Fprintf(&b, "- **生成日時**：%s\n", generatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- **検出総数**：%d 件\n\n", len(items))

	// 概要：深刻度ごとの件数。
	counts := map[string]int{}
	for _, f := range items {
		counts[f.Severity]++
	}
	b.WriteString("## 概要\n\n")
	b.WriteString("| 深刻度 | 件数 |\n| --- | --- |\n")
	for _, s := range []struct{ key, label string }{
		{"critical", "致命的"}, {"high", "高"}, {"medium", "中"}, {"low", "低"},
	} {
		fmt.Fprintf(&b, "| %s | %d |\n", s.label, counts[s.key])
	}
	b.WriteString("\n")

	if len(items) == 0 {
		b.WriteString("_該当する脆弱性なし。_\n")
		return b.String()
	}

	b.WriteString("## 脆弱性の詳細\n\n")
	for i, f := range items {
		fmt.Fprintf(&b, "### %d. [%s] %s\n\n", i+1, strings.ToUpper(nz(f.Severity, "info")), findingTitle(f))
		if f.VulnClass != "" {
			fmt.Fprintf(&b, "- **カテゴリ**：%s\n", f.VulnClass)
		}
		fmt.Fprintf(&b, "- **状態**：%s\n", nz(f.Status, "pending"))
		if desc := strings.TrimSpace(f.TaskDescription); desc != "" {
			fmt.Fprintf(&b, "- **所属タスク**：%s\n", desc)
		}
		fmt.Fprintf(&b, "- **検出日時**：%s\n\n", f.CreatedAt.Format("2006-01-02 15:04:05"))
		if s := strings.TrimSpace(f.Summary); s != "" {
			fmt.Fprintf(&b, "%s\n\n", s)
		}
		if e := strings.TrimSpace(f.Evidence); e != "" {
			fmt.Fprintf(&b, "**証拠：**\n\n```\n%s\n```\n\n", e)
		}
		if rep := strings.TrimSpace(f.Report); rep != "" {
			b.WriteString("**詳細レポート：**\n\n")
			b.WriteString(rep)
			b.WriteString("\n\n")
		}
		b.WriteString(findingTrafficMarkdown(f, false))
		b.WriteString("---\n\n")
	}
	return b.String()
}

// SingleFindingMarkdown は 1 件の脆弱性を独立した Markdown にする
// （「1 脆弱性 1 ファイル」のパッケージング用）。
func SingleFindingMarkdown(f *db.DBFinding, generatedAt time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# [%s] %s\n\n", strings.ToUpper(nz(f.Severity, "info")), findingTitle(f))
	if f.VulnClass != "" {
		fmt.Fprintf(&b, "- **カテゴリ**：%s\n", f.VulnClass)
	}
	fmt.Fprintf(&b, "- **深刻度**：%s\n", nz(f.Severity, "info"))
	fmt.Fprintf(&b, "- **状態**：%s\n", nz(f.Status, "pending"))
	if desc := strings.TrimSpace(f.TaskDescription); desc != "" {
		fmt.Fprintf(&b, "- **所属タスク**：%s\n", desc)
	}
	fmt.Fprintf(&b, "- **検出日時**：%s\n", f.CreatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- **生成日時**：%s\n\n", generatedAt.Format("2006-01-02 15:04:05"))
	if s := strings.TrimSpace(f.Summary); s != "" {
		fmt.Fprintf(&b, "## 概要\n\n%s\n\n", s)
	}
	if e := strings.TrimSpace(f.Evidence); e != "" {
		fmt.Fprintf(&b, "## 証拠\n\n```\n%s\n```\n\n", e)
	}
	if rep := strings.TrimSpace(f.Report); rep != "" {
		b.WriteString("## 詳細レポート\n\n")
		b.WriteString(rep)
		b.WriteString("\n")
	}
	b.WriteString(findingTrafficMarkdown(f, true))
	return b.String()
}

var unsafeFilenameChars = regexp.MustCompile(`[^\p{Han}\p{L}\p{N}._-]+`)

// FindingFilename は「1 脆弱性 1 ファイル」用の安全な .md ファイル名を生成する。
// 形式は `critical_SQLインジェクション_#123.md`。パス区切り文字と制御文字を除去し、
// zip 内の不正パスを防ぐ。
func FindingFilename(f *db.DBFinding) string {
	sev := nz(f.Severity, "info")
	title := findingTitle(f)
	name := fmt.Sprintf("%s_%s_#%d", sev, title, f.ID)
	name = unsafeFilenameChars.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._")
	if name == "" {
		name = fmt.Sprintf("finding_%d", f.ID)
	}
	// 防御的措置：さらにもう一段パスを剥がして zip slip を防ぐ。
	name = path.Base(name)
	if len(name) > 120 {
		name = name[:120]
	}
	return name + ".md"
}

// FindingsCSV は findings を CSV に変換する（UTF-8 BOM 付き。Excel が日本語を正しく
// 認識できるように）。長文の report/evidence 全文は含めず、概要系のフィールドのみを
// 出力する。全文が必要なら Markdown/JSON エクスポートを使う。
func FindingsCSV(fs []*db.DBFinding) []byte {
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF") // UTF-8 BOM
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"ID", "名称", "カテゴリ", "深刻度", "状態", "所属タスク", "検出日時", "概要", "トラフィック証拠数", "トラフィック証拠ID"})
	for _, f := range items {
		_ = w.Write([]string{
			fmt.Sprintf("%d", f.ID),
			findingTitle(f),
			f.VulnClass,
			nz(f.Severity, "info"),
			nz(f.Status, "pending"),
			f.TaskDescription,
			f.CreatedAt.Format("2006-01-02 15:04:05"),
			strings.TrimSpace(f.Summary),
			fmt.Sprint(len(f.TrafficBindings)), findingTrafficIDs(f),
		})
	}
	w.Flush()
	return buf.Bytes()
}

func findingTrafficIDs(f *db.DBFinding) string {
	ids := make([]string, 0, len(f.TrafficBindings))
	for _, b := range f.TrafficBindings {
		ids = append(ids, fmt.Sprint(b.ID))
	}
	return strings.Join(ids, ",")
}

func findingTrafficMarkdown(f *db.DBFinding, attachments bool) string {
	stale := f.Report != "" && f.EvidenceVersion != f.ReportEvidenceVersion
	if len(f.TrafficBindings) == 0 && !stale {
		return ""
	}
	var out strings.Builder
	out.WriteString("\n## 関連トラフィック証拠\n\n")
	fmt.Fprintf(&out, "証拠バージョン：%d；紐付け件数：%d。\n\n", f.EvidenceVersion, len(f.TrafficBindings))
	if stale {
		out.WriteString("証拠が変更されたため、詳細レポートは更新待ち。\n\n")
	}
	for i, b := range f.TrafficBindings {
		fmt.Fprintf(&out, "%d. **証拠 #%d · %s** — `%s %s`、ステータスコード %d\n", i+1, b.ID, b.Role, b.Snapshot.Method, strings.ReplaceAll(b.Snapshot.URL, "`", "%60"), b.Snapshot.Status)
		if b.Note != "" {
			fmt.Fprintf(&out, "   %s\n", strings.ReplaceAll(b.Note, "\n", "\n   "))
		}
		if attachments {
			fmt.Fprintf(&out, "   [リクエストメッセージ](evidence/%d/%d/request.http) · [レスポンスメッセージ](evidence/%d/%d/response.http)\n", f.ID, b.ID, f.ID, b.ID)
		}
	}
	out.WriteString("\n")
	return out.String()
}
