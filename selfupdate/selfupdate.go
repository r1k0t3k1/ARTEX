// Package selfupdate は ARTEX の画面からのワンクリック更新を実装する：GitHub Release から
// 新バージョンのバイナリを取得し、検証・ステージングして、次回起動時にアトミックに
// 入れ替える。
//
// 全体の分担（start.sh / start.bat を参照）：
//
//	起動スクリプト = 単純な監視ループ。「プロセス終了後、終了コードに応じて再起動するか」だけを担う
//	本パッケージ   = 誤りやすいロジックのすべて（ダウンロード / SHA256 検証 / スモーク / 入れ替え / 失敗時のロールバック）
//
// 入れ替えをスクリプトではなく Go で行うのは、sha256 検証とスモークテストを sh と bat の
// 両方に書くと二重実装になり（sha256sum / shasum / certutil）、しかもそこが最も
// 失敗できない箇所だからだ。動かないバイナリを入れてしまうと、デーモンプロセスが
// 律儀に起動を繰り返し、ユーザーはマシン上で手動復旧するしかなくなる。
//
// 完全なアップグレードは 3 回のプロセス起動を経る：
//
//	① 旧版 server が /api/update/apply を受信 → ダウンロード検証 → artex.new をステージング → exit 75
//	② スクリプトが旧版を再起動 → Bootstrap が artex.new を検出 → 検証+スモーク → 入れ替え → exit 75
//	③ スクリプトが再起動、この時点で新版 → Bootstrap が試行を 1 回記録 → 起動成功後にマーカーを削除
//
// どの段階で失敗しても旧版に戻る：② 検証に通らなければステージ済みファイルを消して旧版で続行、
// ③ マーカー削除までに 3 回連続で到達できなければ（起動できずクラッシュ）自動で artex.old に戻す。
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart は「デーモンに再起動してほしい」ことを示す終了コード（EX_TEMPFAIL）。
// 起動スクリプトはこれを見ると即座に再実行し、クラッシュ時のバックオフには数えない。
// 0 はユーザーによる正常停止（スクリプトのループ終了）、それ以外はすべてクラッシュ扱い。
const ExitRestart = 75

// maxAttempts は入れ替え後に許される起動試行回数。新版は起動のたびにカウントを +1 し、
// settleDelay を生き延びればマーカーを削除する。maxAttempts 回連続でクラッシュすれば
// 新版はまったく起動できないと判断し、自動ロールバックする。
const maxAttempts = 3

// Paths は 1 回のアップグレードで関わるすべてのファイルで、**実行ファイルのある
// ディレクトリ**に統一して置く。CWD は意図的に使わない：サービスとして動かすと
// 作業ディレクトリが / や任意のパスになり得るため、CWD を使うとステージ済みファイルが
// 別の場所に落ち、入れ替えロジックがそのまま機能しなくなる。
type Paths struct {
	Dir     string // 実行ファイルのあるディレクトリ
	Current string // 現在動作中のバイナリ      artex      / artex.exe
	New     string // ステージ済みの新バージョン artex.new  / artex.new.exe
	Sum     string // 新バージョンの sha256（hex） artex.new.sha256 / artex.new.exe.sha256
	Old     string // 入れ替え前の旧バージョン   artex.old  / artex.old.exe
	Marker  string // アップグレード状態マーカー artex.upgrade.json
}

// ResolvePaths は現在の実行ファイルからすべてのアップグレードパスを導出する。
//
// Windows では .new/.old にも .exe 拡張子が必要で、なければスモークテストや
// 入れ替え後の実行が失敗する。そのため拡張子を一度外してから連結し、
// 両プラットフォームの命名を対称にしている。
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("実行ファイルの特定: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // Windows では ".exe"、Unix では通常空
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker は 1 回の入れ替えの進捗を記録し、新版が起動できないときに自動ロールバックを
// 発動するために使う。
type marker struct {
	From     string `json:"from"`     // アップグレード前のバージョン
	To       string `json:"to"`       // 目標バージョン
	Attempts int    `json:"attempts"` // 入れ替え後に起動を試みた回数
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged はステージ済みファイルを消す。入れ替え成功、検証失敗、ユーザーによる
// キャンセルのいずれもこれを通り、残留した artex.new が次回起動時に再試行される
// のを防ぐ。
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions は 2 つのバージョンを比較し -1/0/1 を返す（a<b / a==b / a>b）。
// ok=false は少なくとも一方が比較可能なバージョンではない場合（ローカル開発ビルドの
// "dev" や git describe が出力する "0.3.7-2-gabc1234-dirty" など）。この場合、呼び出し側は
// ワンクリック更新を無効にするべきだ。そうしないと開発中のビルドが正式版に
// 「アップグレード」され、未コミットの変更が上書きされてしまう。
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion は "v0.3.7" / "0.3.7" 形式のバージョンを [3]int に解析する。
//
// 純粋な 3 段形式のみを受け付ける：build.sh は非タグビルド時に git describe で
// "0.3.7-2-gabc1234" のような接尾辞付きバージョンを生成するが、これは 0.3.7 として
// 扱うのではなく比較不可と判定しなければならない。さもないと開発ビルドが
// 「最新」と誤判定されたり、正式版に上書きされたりする。
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker はプロセスがコンテナ内で動いているかを報告する。Docker では入れ替えは
// コンテナの書き込み可能レイヤーに書かれるため、`docker compose up -d` でコンテナを
// 作り直すとイメージ同梱のバージョンに戻る——これは想定内の動作（その時点で
// ユーザーは新イメージを取得している）だが、フロントエンドはこれを踏まえて
// 説明を明確にできる必要がある。
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
