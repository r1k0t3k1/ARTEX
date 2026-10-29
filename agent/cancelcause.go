package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "ユーザーがタスクを一時停止した",
		"ユーザーがタスク制御 API（POST /api/tasks/{id}/control、action=pause）でタスクを一時停止した。今回の Planner/Worker 実行は能動的にキャンセルされ、実行中の意図は frontier(open) に戻る。タスクを再開すると再取得され、最初から実行される")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "オーケストレーション Agent がタスクを一時停止した",
		"オーケストレーション Agent が pause_task ツールを呼び出して本タスクを一時停止した。今回の Planner/Worker 実行は能動的にキャンセルされ、実行中の意図は frontier(open) に戻る。再開後に再実行される")
	AbortTaskDeleted = cause("task_deleted", "タスクが削除された",
		"タスクを削除中（DELETE /api/tasks/{id}）。削除バリアがこのタスクで実行中の Planner、Worker、主 Agent をキャンセルした。今回の実行結果はもう使用されない")
	AbortPausedOnReload = cause("paused_on_reload", "バックエンドがタスクの一時停止状態を復元した",
		"バックエンド起動時に、データベースへ永続化された状態に基づいてタスクの一時停止を復元した。今回の実行はキャンセルされた。通常、復元段階で実行中の Agent は存在しない")
	AbortGoalMet = cause("goal_met", "プランナーがタスク目標の達成を判定した",
		"プランナーがタスク目標の達成を判定し、タスクを done にしたうえで、まだ実行中の Worker をキャンセルした。これらの意図は失敗ではなく stopped としてマークされる")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "タスクのタイムアウト時の締めくくりの待機時間が尽きた",
		"タスクが timeout に達した後、実行中の Worker の正常な締めくくりを待ったが、90 秒の drain 猶予でも足りなかったためハードキャンセルを実行した。意図は exhausted としてマークされ、締めくくり段階で書き込み済みの事実と資産は保持される")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "プランナーがこの意図を終了させた",
		"プランナーが kill_work を呼び出してこの意図を能動的に終了させた。通常、方向性がずれたか継続価値がないことを意味する。意図は stopped としてマークされ、自動で再取得されることはない")
	AbortWorkPausedByUser = cause("work_paused_by_user", "ユーザーがこの Worker 意図を一時停止した",
		"ユーザーが実行中の Worker を一時停止した。今回の呼び出しはキャンセルされ、意図は paused に変わる。登録済みの意図、事実、脆弱性、アクティビティ記録はすべて保持され、再開後は最初から再実行される")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "ユーザーがこの Worker 意図を削除した",
		"ユーザーが実行中の Worker を削除した。今回の呼び出しはキャンセルされる。Worker が書き込み区間を抜けた後、サーバーはユーザーが選択した削除モードでこの意図を処理する。論理削除は削除済みとマークするだけで全成果を保持し、物理削除はこの意図と、それだけに支えられた下流ノードを連鎖的に削除する")
	AbortWorkFinished = cause("work_finished", "Worker が正常終了し context を解放した",
		"Worker が正常終了し、エンジンが detachWork でその context リソースを解放した。これは実行中断ではない。もし中断メッセージに現れた場合、キャンセルと終了イベントが競合したことを意味する")
	AbortPausedRaceGuard = cause("paused_race_guard", "タスク一時停止中は新しい実行の開始を拒否した",
		"タスクが一時停止中の場合、エンジンは新しい実行 context の発行を拒否する。claim と一時停止の競合によって Worker が起動し続けるのを防ぐためで、取得済みの意図は frontier に戻る")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "ユーザーが今回の対話を停止した",
		"ユーザーが停止を押し、今回の主 Agent またはセッション Agent の実行を能動的に中止した。生成済みのアクティビティ記録は保持され、次のメッセージを送信できる")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "タスク一時停止に伴い主 Agent の対話も中止された",
		"ユーザーがタスクを一時停止した際、実行中の主 Agent の対話も同時にキャンセルされた。生成済みのアクティビティ記録は保持される。タスク再開後も今回のメッセージが自動で再生されることはない")
	AbortChatTurnFinished = cause("chat_turn_finished", "今回の対話が正常終了し context を解放した",
		"今回の対話が正常終了し、サーバーがそのターンの context リソースを解放している。これは実行中断ではない。もし中断メッセージに現れた場合、キャンセルと終了イベントが競合したことを意味する")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "バックエンドプロセスが終了中",
		"バックエンドプロセスが SIGINT または SIGTERM を受信し、再起動・更新・終了のいずれかを行っている。実行中のすべての Agent がキャンセルされる。再起動後に残った running の意図は open にリセットされ、再実行される")
	AbortRunHardTimeout = cause("run_hard_timeout", "単一実行のハードタイムアウトのバックストップが発動した",
		"単一実行がソフトな実時間予算と追加猶予を超えた。モデル要求またはあるツールが長時間応答を返さず、通常のターン境界での締めくくりを実行できなかったことを意味する。中断前の最後の未応答ツール呼び出しを重点的に確認すること")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "上流の context が deadline に到達",
			"上流の context が deadline に到達したが、設定側が WithTimeoutCause で具名理由を付加していない: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "キャンセル側が具名理由を付加していない",
			"上流の context がキャンセルされたが、キャンセル側が context.WithCancelCause で具名理由を付加していない；agent/cancelcause.go に理由を登録し、そのキャンセル箇所に接続すること", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
