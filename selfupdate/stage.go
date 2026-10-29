package selfupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"
)

// sumsAsset は release.yml が生成するチェックサム一覧で、Release 内のすべての zip を
// カバーする。
const sumsAsset = "SHA256SUMS"

// maxBinarySize は展開後のバイナリサイズを制限し、壊れた zip がディスクを
// 埋め尽くすのを防ぐ。
const maxBinarySize = 512 << 20 // 512 MiB

// Phase はアップグレード中の段階で、SSE イベントの phase フィールドにそのまま使う。
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseDownload Phase = "downloading"
	PhaseVerify   Phase = "verifying"
	PhaseExtract  Phase = "extracting"
	PhaseStaged   Phase = "staged"
	PhaseFailed   Phase = "failed"
)

// Progress は呼び出し側が提供し、進捗をフロントエンドへ送るために使う。pct は
// ダウンロード段階でのみ意味を持つ（0-100）。それ以外の段階では -1 を渡す。
type Progress func(ph Phase, pct int, msg string)

// Stage は指定 Release の現行プラットフォーム向けリリースパッケージをダウンロードし、
// 検証後に新バイナリを artex.new としてステージングする。
//
// 裸のバイナリではなく zip 全体を使う理由は 2 つある：既存 Release の SHA256SUMS は
// もともと zip しかカバーしていないため、zip 経由なら CI を変更せずに済み、既に
// 公開済みの過去バージョンとも互換がある。zip には skills/ も含まれており、将来
// 組み込み skill を同期する余地を残せる。代償は skills の数百 KB を余分に
// ダウンロードするだけ。
//
// 関数が戻ればステージング完了を意味し、呼び出し側はその後グレースフルに終了して
// ExitRestart で終える。
func Stage(ctx context.Context, c *http.Client, rel *Release, currentVersion string, prog Progress) error {
	if prog == nil {
		prog = func(Phase, int, string) {}
	}
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if err := checkWritable(p.Dir); err != nil {
		return err
	}

	name := AssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	asset, ok := rel.FindAsset(name)
	if !ok {
		return fmt.Errorf("このバージョンには %s/%s 向けのリリースパッケージがない（%s が欠落）", runtime.GOOS, runtime.GOARCH, name)
	}

	prog(PhaseDownload, 0, "チェックサム一覧を取得中…")
	sums, err := fetchSums(ctx, c, rel)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s に %s が記載されていないため、検証されていないバイナリのインストールを拒否", sumsAsset, name)
	}

	// 一時ファイルはすべてターゲットディレクトリ内に置き、最後の rename が同一
	// ファイルシステム内のアトミック操作になるようにする（デバイスをまたぐ rename は
	// 失敗し、/tmp は独立したマウントポイントであることが多い）。
	zipPath := p.New + ".zip.part"
	binPath := p.New + ".part"
	defer func() {
		_ = os.Remove(zipPath)
		_ = os.Remove(binPath)
	}()

	prog(PhaseDownload, 0, fmt.Sprintf("%s をダウンロード中（%s）…", name, humanSize(asset.Size)))
	got, err := download(ctx, c, asset, zipPath, prog)
	if err != nil {
		return err
	}

	prog(PhaseVerify, -1, "SHA256 を検証中…")
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256 が一致しない：期待値 %s、実際 %s（ダウンロード破損または改ざん）", short(want), short(got))
	}

	prog(PhaseExtract, -1, "展開してスモークテスト中…")
	if err := extractBinary(zipPath, binPath); err != nil {
		return err
	}
	if err := smokeTest(binPath); err != nil {
		return fmt.Errorf("新バージョンが現在のシステムで実行できない: %w", err)
	}

	// ステージ済みファイル自身の sha256 を別途保存する：次回起動時の入れ替え前に
	// もう一度検証し、ステージングから再起動までの間にファイルが改変・破損するのを防ぐ。
	binSum, err := fileSHA256(binPath)
	if err != nil {
		return fmt.Errorf("新しいバイナリのチェックサム計算: %w", err)
	}
	if err := os.WriteFile(p.Sum, []byte(binSum), 0o644); err != nil {
		return fmt.Errorf("チェックサムの書き込み: %w", err)
	}
	if err := os.Rename(binPath, p.New); err != nil {
		_ = os.Remove(p.Sum)
		return fmt.Errorf("新バージョンのステージング: %w", err)
	}

	if err := writeMarker(p.Marker, marker{
		From:     currentVersion,
		To:       strings.TrimPrefix(rel.TagName, "v"),
		StagedAt: time.Now().Unix(),
	}); err != nil {
		// マーカーは自動ロールバック能力にしか影響しない。ステージ済みファイル自体は
		// すでに配置済みなので、これでアップグレードを中断しない。
		prog(PhaseStaged, -1, "警告：アップグレードマーカーの書き込みに失敗。今回のアップグレードには自動ロールバック保護がない")
	}

	prog(PhaseStaged, 100, "新バージョンの準備完了、再起動中…")
	return nil
}

// fetchSums は SHA256SUMS をダウンロードして解析し、ファイル名 → 16 進ダイジェスト
// のマップを返す。
func fetchSums(ctx context.Context, c *http.Client, rel *Release) (map[string]string, error) {
	asset, ok := rel.FindAsset(sumsAsset)
	if !ok {
		return nil, fmt.Errorf("この Release には %s がないため完全性を検証できず、アップグレードを拒否", sumsAsset)
	}
	body, err := get(ctx, c, asset.URL)
	if err != nil {
		return nil, fmt.Errorf("%s のダウンロード: %w", sumsAsset, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s の読み込み: %w", sumsAsset, err)
	}
	out := parseSums(string(raw))
	if len(out) == 0 {
		return nil, fmt.Errorf("%s の内容が空、または形式を判別できない", sumsAsset)
	}
	return out, nil
}

// parseSums は sha256sum 形式の一覧を解析し、ファイル名 → 16 進ダイジェストのマップを
// 返す。
//
// 最初のフィールドが 64 桁の 16 進でなければ採用しない。「ちょうど 2 フィールド」だけの
// 判定では不十分で、2 単語だけの説明行がすべて正当なエントリと見なされ、ゴミ値が
// ダイジェスト表に混入し、実際のアセットが誤ったダイジェストにマッチし得る。
func parseSums(raw string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(raw) {
		// 形式は "<sha256>  <filename>"（sha256sum は半角スペース 2 つ。shasum の
		// バイナリモードはファイル名に * を付ける）。
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || !isHexSHA256(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" {
			continue
		}
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// download はアセットを dst に書き込み、同時に SHA256 を計算して
// Content-Length に基づき進捗を報告する。
func download(ctx context.Context, c *http.Client, a Asset, dst string, prog Progress) (string, error) {
	body, err := get(ctx, c, a.URL)
	if err != nil {
		return "", fmt.Errorf("%s のダウンロード: %w", a.Name, err)
	}
	defer body.Close()

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("一時ファイルの作成: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	pw := &progressWriter{total: a.Size, prog: prog, name: a.Name, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), body); err != nil {
		return "", fmt.Errorf("ダウンロード中断: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("ディスク書き込み失敗: %w", err)
	}
	if a.Size > 0 && pw.written != a.Size {
		return "", fmt.Errorf("ダウンロードが不完全：期待値 %d バイト、実際 %d バイト", a.Size, pw.written)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get はホワイトリストに制約された GET を発行し、レスポンスボディを返す。
func get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artex-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// extractBinary はリリースパッケージから artex 実行ファイルを取り出す。
//
// パッケージ内の構造は artex-<バージョン>-<os>-<arch>/artex だが、ここでは完全な
// パスを組み立てず**ベース名**で照合する：バージョン番号はパッケージ名に一度しか
// 現れず、1 文字でも間違えるとアップグレード全体が失敗するため、ベース名検索の
// ほうが変更に強い。
func extractBinary(zipPath, dst string) error {
	want := "artex"
	if runtime.GOOS == "windows" {
		want = "artex.exe"
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("リリースパッケージのオープン: %w", err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Base(entry.Name), want) {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("%s の読み込み: %w", entry.Name, err)
		}
		defer rc.Close()

		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("新しいバイナリの書き出し: %w", err)
		}
		defer f.Close()

		n, err := io.Copy(f, io.LimitReader(rc, maxBinarySize+1))
		if err != nil {
			return fmt.Errorf("%s の展開: %w", entry.Name, err)
		}
		if n > maxBinarySize {
			return fmt.Errorf("リリースパッケージ内の実行ファイルが %s を超えているため展開を拒否", humanSize(maxBinarySize))
		}
		if n == 0 {
			return fmt.Errorf("リリースパッケージ内の %s は空ファイル", want)
		}
		return f.Sync()
	}
	return fmt.Errorf("リリースパッケージ内に %s が見つからない", want)
}

// checkWritable は事前にディレクトリが書き込み可能か確認する。この手順がないと、
// 非 root で実行している場合やバイナリがシステムディレクトリに置かれている場合、
// 数十 MB をダウンロードし終えた後、入れ替えの瞬間になって初めて失敗する。
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".artex-update-probe-*")
	if err != nil {
		return fmt.Errorf("プログラムディレクトリ %s に書き込めないため自動更新できない（権限を確認するか手動アップグレードを使用）: %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// progressWriter は書き込み済みバイト数を集計して頻度を抑えて報告し、32KiB ブロックごとに
// SSE を 1 件送ってしまうのを避ける。
type progressWriter struct {
	total   int64
	written int64
	name    string
	prog    Progress
	last    time.Time
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.written += int64(len(b))
	if time.Since(w.last) < 300*time.Millisecond {
		return len(b), nil
	}
	w.last = time.Now()
	pct := -1
	if w.total > 0 {
		pct = int(w.written * 100 / w.total)
	}
	w.prog(PhaseDownload, pct, fmt.Sprintf("ダウンロード中 %s / %s", humanSize(w.written), humanSize(w.total)))
	return len(b), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
