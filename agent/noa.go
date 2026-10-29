package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa は norma v0.4.0 で導入された「モデル駆動のコンテキスト圧縮」機構で、プラットフォームの実験機能として
// ユーザーがシステム設定でオン/オフする。組み込みの compaction とは排他である：noaadapter.Enable が唯一の入口で、
// コンテキスト接管器(Compactor)、Compress ツール、3 段の常駐プロンプトを一度に装着する。Enable を呼ばなければオフであり
// （組み込み compaction は従来どおり動作する）、スイッチは各 agent が注入する noaEnabledFn が解析し、run ごとに 1 回読む。
// そのため切り替えは以後に起動する run にのみ影響し、agent の再構築は不要である。

// enableNoa は、リゾルバがオンを報告したときに noa を opts へ接続する。archiveRoot は圧縮原文の永続化基点ディレクトリ
// （グローバルな workDir を取り、各 agent は <workDir>/noa 配下に統一して置く。タスク/意図のディレクトリごとに分散しない）。sessionID は
// その配下のアーカイブサブディレクトリを命名する（グローバルに一意なので、同じ基点ディレクトリ内で衝突しない）。
//
// noa は実験機能である：接続の失敗で実際のタスクを中断してはならない。エラー発生時は onWarn で報告し、組み込み圧縮へフォールバックする。
// 有効化に成功したら opts.Compaction をクリアし、agentcore が「2 つのコンテキスト管理器が同時に設定されている」と警告するのを避ける。
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa 圧縮の有効化に失敗、組み込み圧縮へフォールバック: " + err.Error())
		}
		return
	}
	// Compactor は Compaction を上書きするが、両者が併存すると agentcore が毎回警告するため、明示的にクリアする。
	opts.Compaction = nil
}
