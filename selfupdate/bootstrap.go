package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv はスモークテストで起動したサブプロセスに Bootstrap を直接スキップさせる。
//
// 厳密には付けなくても問題は起きない：サブプロセスの os.Executable() は artex.new で、
// 導出されるパスはすべて .new プレフィックス付きになり、実際のアップグレードファイルには
// 触れない。ただしこのような偶然に頼るのは脆すぎるので、明示的にショートサーキット
// させるほうが分かりやすく、サブプロセスによる無駄なディスク探索も省ける。
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action は Bootstrap が main に返す指示。
type Action int

const (
	// Continue：通常どおり server を起動する。
	Continue Action = iota
	// Restart：直ちに ExitRestart で終了し、デーモンスクリプトに再起動させる。
	Restart
)

// State は今回の起動時点のアップグレード状態を表し、/api/update/check が
// 「前回のアップグレードが成功したか、ロールバックされたか」をフロントエンドに
// 正確に伝えるために使う。
type State struct {
	Pending     bool   // 入れ替え後、まだ安定性を確認していない
	RolledBack  bool   // 今回の起動でたった今自動ロールバックを実行した
	FailedStage bool   // ステージ済みファイルの検証/スモークに失敗し、破棄した
	Detail      string // ユーザー向けの一言説明
}

// Bootstrap は main の冒頭で実行し、ポートの待ち受けやデータベースのオープンより前に
// 呼び出す必要がある。
//
// 3 つの状況：
//
//	① ステージ済みの artex.new がある → 検証 + スモーク。通れば入れ替えて再起動を要求し、
//	                                   通らなければ破棄して旧版のまま続行
//	② マーカーファイルだけがある      → 入れ替え直後と判断。試行回数を 1 回加算し、
//	                                   連続失敗が上限に達したらロールバック
//	③ 何もない                        → 通常起動
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] ブートストラップをスキップ：%v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged は「ステージ済みファイルがある」状況を処理する：検証が通れば入れ替え、
// 失敗すれば破棄する。
//
// ここはアップグレード経路で唯一実行ファイルを上書きする場所であり、最後の関門でもある。
// スモークテストがダウンロード破損、アーキテクチャの取り違え、動的リンクの欠落といった
// 問題を弾く。動かないバイナリを一度通してしまえば、デーモンスクリプトが延々とそれを
// 起動し続け、Go コードは実行される機会すら得られず、自動ロールバックも働かない。
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] ステージ済みの新バージョンが検証に失敗したため破棄し、現行バージョンで続行：%v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "新バージョンの検証に失敗したため破棄：" + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] 入れ替えに失敗したため現行バージョンで続行：%v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "入れ替え失敗：" + err.Error()}
	}

	// 入れ替え成功。マーカーは残し、次回の起動（新版で動く）に安定性の確認を委ねる。
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] アップグレードマーカーの書き込みに失敗（自動ロールバック能力を喪失）：%v", err)
	}
	log.Printf("[update] %s へ入れ替え完了。再起動のため終了（exit %d）", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback は「入れ替え後の起動」を処理する：試行回数を加算し、上限を超えたら
// 旧版に戻す。
//
// カウントは Go コードが動き出してから初めて増えるので、これが対象とするのは
// 「実行はできるが初期化時にクラッシュする」類の障害（設定の非互換、ポート占有、
// DB マイグレーションの失敗）である。「exec すらできない」場合は入れ替え前の
// スモークテストが弾く。両者を合わせて初めて完全になる。
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// ロールバックまで失敗したなら再起動はしない。無限再起動に陥るため。
			// マーカーを消し、現状のままプロセスを起動する——起動できなくても
			// ユーザーは少なくともログで原因を確認できる。
			log.Printf("[update] 新バージョンが %d 回連続で起動に失敗し、ロールバックも失敗：%v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "新バージョンの起動に失敗し、ロールバックも失敗：" + err.Error()}
		}
		log.Printf("[update] 新バージョンが %d 回連続で起動に失敗したため %s にロールバック。再起動のため終了（exit %d）",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("新バージョンの起動に失敗し、%s にロールバック", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] アップグレードマーカーの更新に失敗：%v", err)
	}
	log.Printf("[update] 新バージョン起動中（%d/%d 回目の試行）、安定稼働後にアップグレードを確定",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle は新バージョンが安定稼働したことを確認し、アップグレードマーカーを削除する。
//
// main が HTTP リスンを開始した後に遅延呼び出しする：この時間を生き延びて初めて
// 合格となり、そうでなければマーカーは残ったまま、次回起動時に試行回数の加算が
// 続き、やがてロールバックが発動する。
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // アップグレード後の起動ではないため、何もしない
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] アップグレードマーカーの削除に失敗：%v", err)
		return
	}
	log.Printf("[update] 新バージョンは安定稼働、アップグレード完了（前バージョンは %s として保持）", p.Old)
}

// SettleDelay は「新バージョンが生き延びた」と判定するのに必要な稼働時間。
const SettleDelay = 30 * time.Second

// verifyStaged はステージ済みファイルを検証する：まず SHA256 を照合し、
// 実際に一度起動して動かしてみる。
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("チェックサムの読み込み: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("チェックサムの計算: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 が一致しない（ダウンロード破損または改ざん）")
	}
	return smokeTest(p.New)
}

// smokeTest は -h で新しいバイナリを起動し、現在のシステムで実際に実行できることを
// 確認する。これによりダウンロードの切断、アーキテクチャの取り違え（exec format error）、
// 依存関係の欠落といった一大類の問題を弾ける。
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("実行権限の付与: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("スモークテストがタイムアウト（新しいバイナリが応答しない）")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("スモークテスト失敗: %v: %s", err, snippet)
	}
	return nil
}

// swap は現在のバイナリをステージ済みの新バージョンに置き換える。
//
// Unix も Windows も実行中の実行ファイルの rename は許可されている（Windows が禁じるのは
// 削除と上書きであり、rename は含まれない）。そのためプラットフォーム分岐も、
// 自分自身を先に停止する必要もない。
func swap(p Paths) error {
	// Windows の rename は既存のターゲットを上書きしないため、前回のアップグレードが
	// 残した .old を先に消しておく必要がある。
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("旧バックアップ %s のクリーンアップ: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("現行バージョンのバックアップ: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// 入れ替えに失敗したが現行バージョンはすでに移動済みなので、必ず元に戻す。
		// そうしないと次回起動時に実行ファイルが存在しない。
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("新バージョンの配置に失敗(%v)、かつ現行バージョンの復元にも失敗: %w", err, rerr)
		}
		return fmt.Errorf("新バージョンの配置: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback は swap でバックアップした旧バージョンを戻す。
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("ロールバック可能なバックアップ %s がない: %w", p.Old, err)
	}
	// 起動できない新版は直接削除せず、調査用に .failed へ退避する。
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("失敗したバージョンの移動: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("旧バージョンの復元: %w", err)
	}
	return nil
}

// Rollback は /api/update/rollback の実装：能動的に前バージョンへ戻す。
// 入れ替えのみを行い、再起動は同じくデーモンスクリプトに任せる（呼び出し側が
// その後 ExitRestart で終了する）。
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("ロールバック可能な前バージョンがない（" + p.Old + " が存在しない）")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("前バージョンが実行できないためロールバックを拒否: %w", err)
	}
	// 現行とバックアップを入れ替える：ロールバック後にもう一度戻すこともできる。
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("現行バージョンの移動: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("前バージョンの配置: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] ロールバック後のバックアップ整理に失敗（動作には影響しない）：%v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup はロールバック可能な前バージョンが存在するかを報告し、フロントエンドが
// ロールバックボタンを表示するかどうかの判断に使う。
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "不明なバージョン"
	}
	return s
}
