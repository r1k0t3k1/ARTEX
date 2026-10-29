package selfupdate

import (
	"archive/zip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testPaths は隔離されたアップグレード用ディレクトリを作る。ResolvePaths() を直接
// 使ってはならない——テストバイナリ自身を指してしまい、実行すると go test の
// 実行ファイルの名前を変えてしまう。
func testPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		Dir:     dir,
		Current: filepath.Join(dir, "artex"),
		New:     filepath.Join(dir, "artex.new"),
		Sum:     filepath.Join(dir, "artex.new.sha256"),
		Old:     filepath.Join(dir, "artex.old"),
		Marker:  filepath.Join(dir, "artex.upgrade.json"),
	}
}

// fakeBin は実行可能なシェルスクリプトを artex の代役として書き出す。smokeTest は
// -h で起動して終了コードを見るだけなのでスクリプトで十分であり、本物のバイナリを
// ビルドするよりずっと速い。
func fakeBin(t *testing.T, path, marker string, exitCode int) {
	t.Helper()
	script := "#!/bin/sh\necho " + marker + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("ダミーバイナリ %s の書き込み: %v", path, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// stage は bin を「ステージ済み・入れ替え待ち」の状態に整える：artex.new とその
// チェックサムを書き込む。
func stage(t *testing.T, p Paths, marker string, exitCode int) {
	t.Helper()
	fakeBin(t, p.New, marker, exitCode)
	sum, err := fileSHA256(p.New)
	if err != nil {
		t.Fatalf("チェックサムの計算: %v", err)
	}
	if err := os.WriteFile(p.Sum, []byte(sum), 0o644); err != nil {
		t.Fatalf("チェックサムの書き込み: %v", err)
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s の読み込み: %v", path, err)
	}
	return string(b)
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("ダミーバイナリは sh スクリプトのため Windows では実行できない")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b       string
		want       int
		comparable bool
	}{
		{"0.3.7", "0.3.8", -1, true},
		{"0.3.8", "0.3.7", 1, true},
		{"0.3.7", "0.3.7", 0, true},
		{"v0.3.7", "0.3.8", -1, true}, // build.sh は v を外し tag は v 付き。両方を受け付ける必要がある
		{"0.3.7", "v0.3.7", 0, true},
		{"0.9.0", "0.10.0", -1, true}, // 辞書順ではなく数値で比較する
		{"1.0.0", "0.99.99", 1, true},
		// 開発ビルドは比較不可と判定しなければならない。さもないと正式版に上書きされ、
		// 未コミットの変更が失われる。
		{"dev", "0.3.8", 0, false},
		{"0.3.7-2-gabc1234", "0.3.8", 0, false},
		{"0.3.7-dirty", "0.3.8", 0, false},
		{"0.3", "0.3.8", 0, false},
		{"", "0.3.8", 0, false},
	}
	for _, c := range cases {
		got, ok := CompareVersions(c.a, c.b)
		if ok != c.comparable {
			t.Errorf("CompareVersions(%q,%q) comparable=%v、%v であるべき", c.a, c.b, ok, c.comparable)
			continue
		}
		if ok && got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d、%d であるべき", c.a, c.b, got, c.want)
		}
	}
}

func TestResolvePathsNaming(t *testing.T) {
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	// 重要な不変条件：すべてのアップグレード用ファイルが実行ファイルと同じディレクトリにある。
	// CWD に落ちると、サービスとしての実行（作業ディレクトリが / になり得る）時に
	// 入れ替えが完全に機能しなくなる。
	for name, path := range map[string]string{"New": p.New, "Sum": p.Sum, "Old": p.Old, "Marker": p.Marker} {
		if filepath.Dir(path) != p.Dir {
			t.Errorf("%s が実行ファイルのディレクトリにない: %s（期待値 %s）", name, path, p.Dir)
		}
	}
	// Windows では .new/.old に .exe を残す必要がある。さもないとスモークテストと
	// 入れ替え後の実行が失敗する。
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(p.New, ".exe") || !strings.HasSuffix(p.Old, ".exe") {
			t.Errorf("Windows では .new/.old が .exe で終わる必要がある: new=%s old=%s", p.New, p.Old)
		}
	}
}

func TestVerifyStagedRejectsTamperedBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "new", 0)

	// チェックサム確定後にファイルを改変し、ダウンロード破損 / すり替えを再現する。
	fakeBin(t, p.New, "tampered", 0)
	if err := verifyStaged(p); err == nil {
		t.Fatal("SHA256 不一致で拒否されるはずが、通過した")
	}
}

func TestVerifyStagedRejectsUnrunnableBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "broken", 1) // 実行はできるが終了コードが非 0

	if err := verifyStaged(p); err == nil {
		t.Fatal("スモークテスト失敗で拒否されるはずが、通過した")
	}
}

func TestApplyStagedHappyPath(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8"}); err != nil {
		t.Fatalf("マーカーの書き込み: %v", err)
	}

	action, st := applyStaged(p)
	if action != Restart {
		t.Fatalf("Restart を期待したが %v を取得", action)
	}
	if !st.Pending {
		t.Error("入れ替え後の状態は Pending であるべき")
	}
	if !strings.Contains(readAll(t, p.Current), "new") {
		t.Error("artex は新バージョンに置き換えられているべき")
	}
	if !strings.Contains(readAll(t, p.Old), "old") {
		t.Error("旧バージョンは artex.old にバックアップされているべき")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("入れ替え後は artex.new が消えているべき")
	}
	if _, err := os.Stat(p.Sum); !os.IsNotExist(err) {
		t.Error("入れ替え後はチェックサムファイルがクリーンアップされているべき")
	}
	// マーカーは残さなければならない。次回起動（新版で動く）がこれで回数を数え、
	// 必要時にロールバックする。
	if _, ok := readMarker(p.Marker); !ok {
		t.Error("入れ替え後はアップグレードマーカーが残っているべき")
	}
}

func TestApplyStagedKeepsCurrentWhenVerifyFails(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	fakeBin(t, p.New, "tampered", 0) // チェックサムを破壊する

	action, st := applyStaged(p)
	if action != Continue {
		t.Fatalf("検証失敗時は Continue を期待したが %v を取得", action)
	}
	if !st.FailedStage {
		t.Error("状態は FailedStage とマークされるべき")
	}
	if !strings.Contains(readAll(t, p.Current), "old") {
		t.Fatal("検証失敗時に現行バージョンへ触れてはならない")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("検証に失敗したステージ済みファイルは削除されるべき。そうでないと次回起動時に再試行される")
	}
}

func TestSwapOverwritesPreviousBackup(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0) // 前回のアップグレードが残したバックアップ
	stage(t, p, "v3", 0)

	if err := swap(p); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if !strings.Contains(readAll(t, p.Current), "v3") {
		t.Error("v3 へ入れ替えられるべき")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("バックアップは直前に外した v2 に更新されるべき")
	}
}

func TestConfirmCountsAttemptsThenRollsBack(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "broken-new", 0)
	fakeBin(t, p.Old, "good-old", 0)
	m := marker{From: "0.3.7", To: "0.3.8"}

	// maxAttempts 回までの起動はカウントを加算するだけで、新版が自力で安定する
	// 機会を与える。
	for i := 1; i <= maxAttempts; i++ {
		action, st := confirmOrRollback(p, m)
		if action != Continue {
			t.Fatalf("%d 回目の試行では Continue を期待したが %v を取得", i, action)
		}
		if !st.Pending {
			t.Errorf("%d 回目の試行の状態は Pending であるべき", i)
		}
		got, ok := readMarker(p.Marker)
		if !ok || got.Attempts != i {
			t.Fatalf("%d 回目の試行後の attempts=%d（ok=%v）、%d であるべき", i, got.Attempts, ok, i)
		}
		m = got
	}

	// もう一度クラッシュすれば上限超過となり、自動で旧版に戻す。
	action, st := confirmOrRollback(p, m)
	if action != Restart {
		t.Fatalf("試行上限超過時は Restart を期待したが %v を取得", action)
	}
	if !st.RolledBack {
		t.Error("状態は RolledBack とマークされるべき")
	}
	if !strings.Contains(readAll(t, p.Current), "good-old") {
		t.Fatal("旧バージョンへロールバックされているべき")
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("ロールバック後はマーカーが削除されるべき。そうでないと無限にロールバックする")
	}
	// 起動できなかったバージョンは調査用に残し、直接削除しない。
	if _, err := os.Stat(p.Current + ".failed"); err != nil {
		t.Error("失敗したバージョンは調査用に .failed として残されるべき")
	}
}

func TestManualRollbackIsReversible(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0)

	// Rollback() は ResolvePaths() を通るため、ここでは下層の交換セマンティクスを
	// 直接テストする。
	tmp := p.Current + ".swap"
	if err := os.Rename(p.Current, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readAll(t, p.Current), "v1") {
		t.Error("ロールバック後の現行バージョンは v1 であるべき")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("ロールバック後のバックアップは v2 になるべき（再度戻せるように）")
	}
}

func TestParseSums(t *testing.T) {
	const (
		linuxSum = "1111111111111111111111111111111111111111111111111111111111111111"
		winSum   = "ABCDEF0000000000000000000000000000000000000000000000000000000000"
	)
	// sha256sum の出力は半角スペース 2 つ区切り。shasum -a 256 のバイナリモードは
	// ファイル名に * を付ける。
	raw := linuxSum + "  artex-0.3.8-linux-amd64.zip\n" +
		winSum + " *artex-0.3.8-windows-amd64.zip\n" +
		"\n" +
		"garbage line\n" + // フィールドはちょうど 2 つだが、最初がダイジェストではない
		"deadbeef  artex-0.3.8-darwin-arm64.zip\n" // ダイジェストの長さが不正

	out := parseSums(raw)
	if out["artex-0.3.8-linux-amd64.zip"] != linuxSum {
		t.Errorf("linux エントリの解析が不正: %v", out)
	}
	// ダイジェストは小文字に統一する。そうしないと比較時に大文字小文字の違いで
	// 不一致と誤判定される。
	if got := out["artex-0.3.8-windows-amd64.zip"]; got != strings.ToLower(winSum) {
		t.Errorf("windows エントリが不正（* プレフィックスを除去し、ダイジェストを小文字化すべき）: %q", got)
	}
	if len(out) != 2 {
		t.Errorf("空行、ダイジェストでない行、長さの不正な行は無視されるべき。得られた値 %v", out)
	}
}

func TestExtractBinaryFindsNestedEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("パッケージ内のベース名は Windows では artex.exe のため、この用例は Unix の命名で構築している")
	}
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// 実際のリリースパッケージの構造：artex-<バージョン>-<os>-<arch>/artex に加え、
	// いくつかのダミーファイル。
	for name, body := range map[string]string{
		"artex-0.3.8-linux-amd64/README.md":           "readme",
		"artex-0.3.8-linux-amd64/skills/a.md":         "skill",
		"artex-0.3.8-linux-amd64/artex":               "#!/bin/sh\nexit 0\n",
		"artex-0.3.8-linux-amd64/config.example.json": "{}",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := extractBinary(zipPath, dst); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got := readAll(t, dst); !strings.Contains(got, "exit 0") {
		t.Errorf("展開されたものが artex 実行ファイルではない: %q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("展開されたバイナリには実行ビットが必要")
	}
}

func TestExtractBinaryMissingEntry(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("artex-0.3.8-linux-amd64/README.md")
	_, _ = w.Write([]byte("readme"))
	_ = zw.Close()
	f.Close()

	if err := extractBinary(zipPath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("パッケージ内に実行ファイルがない場合はエラーになるべき")
	}
}

func TestCheckURLRejectsNonGitHub(t *testing.T) {
	bad := []string{
		"http://github.com/x",           // HTTPS ではない
		"https://evil.com/artex.zip",    // ドメインがホワイトリストにない
		"https://github.com.evil.com/x", // 接尾辞による偽装
		"https://raw.githubusercontent.com.evil.com/x",
	}
	for _, raw := range bad {
		u := mustParse(t, raw)
		if err := checkURL(u); err == nil {
			t.Errorf("checkURL(%q) は拒否すべき", raw)
		}
	}
	good := []string{
		"https://api.github.com/repos/x/releases/latest",
		"https://objects.githubusercontent.com/blah",
		"https://GitHub.com/x", // ドメインは大文字小文字を区別しない
	}
	for _, raw := range good {
		u := mustParse(t, raw)
		if err := checkURL(u); err != nil {
			t.Errorf("checkURL(%q) は許可すべきだがエラー: %v", raw, err)
		}
	}
}

func TestAssetNameMatchesBuildScript(t *testing.T) {
	// build.sh の package_binary は artex-<バージョン>-<os>-<arch>.zip を使い、
	// バージョンから v プレフィックスを外している。ここで 1 文字でも間違えると、
	// すべてのプラットフォームのワンクリック更新でアセットが見つからなくなる。
	if got := AssetName("v0.3.8", "linux", "amd64"); got != "artex-0.3.8-linux-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName("0.3.8", "windows", "amd64"); got != "artex-0.3.8-windows-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%q の解析: %v", raw, err)
	}
	return u
}

func TestSettleClearsMarkerAndStopsRollback(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "new", 0)
	fakeBin(t, p.Old, "old", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: 2}); err != nil {
		t.Fatal(err)
	}

	settle(p)

	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Fatal("安定確認後はアップグレードマーカーを削除しなければならない")
	}
	// マーカーがなくなれば、以降の通常再起動で回数が加算されず、誤ってロールバックも
	// 発動しない。
	if _, ok := readMarker(p.Marker); ok {
		t.Error("マーカーの読み取りは失敗すべき")
	}
	// バックアップは残す。ユーザーが手動でロールバックできるように。
	if _, err := os.Stat(p.Old); err != nil {
		t.Error("安定確認後も前バージョンのバックアップは残すべき")
	}
}

func TestSettleIsNoopWithoutMarker(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "cur", 0)
	settle(p) // 通常起動経路。panic もファイル操作も起きてはならない
	if _, err := os.Stat(p.Current); err != nil {
		t.Error("マーカーなしの場合、settle はどのファイルにも影響してはならない")
	}
}
