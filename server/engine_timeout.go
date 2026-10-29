package server

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// タスク単位のタイムアウト調停器（docs/任务级超时与收尾设计.md §4/§8.5 を参照）。
// 絶対ウォールクロック方式：timeout を持つタスクごとにタイマー goroutine を 1 本立て、
// 期限が来たら整った順序で締めくくり（wrap-up）シーケンスを駆動する:
//   ① settling → ② worker は新規意図の取得を停止 / ③ planner は通常の notify を破棄
//   ④ 実行中 worker の drain を待つ（grace 付き） → ⑤ 最終ラウンドの planner 判定 → ⑥ 終状態を確定（ガード付き）

const (
	settleDrainGrace     = 90 * time.Second // 実行中 worker の優雅な締めくくりを待つ上限。超えたら強制 cancel
	deadlinePollInterval = 2 * time.Second  // deadline 未スタンプ / LLM 未準備のときのポーリング間隔
	deadlineMaxSleep     = 30 * time.Second // 1 回の最長スリープ（終状態を定期的に見直すため）
)

// ---------- settling 状態 ----------

func (e *Engine) isSettling(taskID string) bool {
	v, _ := e.settling.Load(taskID)
	b, _ := v.(bool)
	return b
}

// markSettling は settling を on にする。最初の呼び出し元だけ true を返す。
func (e *Engine) markSettling(taskID string) bool {
	_, loaded := e.settling.LoadOrStore(taskID, true)
	return !loaded
}

// ---------- 実行中カウント(worker.Execute + planner.Plan)、drain に使用 ----------

func (e *Engine) inflightCounter(taskID string) *int64 {
	v, _ := e.inflight.LoadOrStore(taskID, new(int64))
	return v.(*int64)
}

// beginTaskOperation はタスク所有の操作を原子的に登録する。ただし削除がすでに
// バリアを設置済みなら登録しない。これにより削除ハンドラは inflight==0 を待つだけでよく、
// check-then-start の競合でクリーンアップ後にファイルが再作成される事態を避けられる。
func (e *Engine) beginTaskOperation(taskID string) bool {
	e.deleteMu.RLock()
	defer e.deleteMu.RUnlock()
	if e.IsDeleting(taskID) {
		return false
	}
	atomic.AddInt64(e.inflightCounter(taskID), 1)
	return true
}

func (e *Engine) decInflight(taskID string) { atomic.AddInt64(e.inflightCounter(taskID), -1) }
func (e *Engine) inflightCount(taskID string) int64 {
	return atomic.LoadInt64(e.inflightCounter(taskID))
}

// ---------- deadline ----------

// taskDeadline はタスクの絶対 deadline（unix）を返す。まずプロセス内 map（今回の
// セッションでスタンプ済み）を見て、なければ DB から読み込んだ値（再起動時）に
// フォールバックして map を埋める。0 = タイムアウトなし / 未スタンプ。
func (e *Engine) taskDeadline(t *Task) int64 {
	if v, ok := e.deadline.Load(t.ID); ok {
		return v.(int64)
	}
	deadlineAt := t.lifecycleSnapshot().DeadlineAt
	if deadlineAt > 0 {
		e.deadline.Store(t.ID, deadlineAt)
		return deadlineAt
	}
	return 0
}

// resetTimeoutRevival は PostgreSQL が timeout → running を原子的にコミットし、
// first_run_at/deadline_at をリセットした後に、実行単位のタイムアウト状態だけを
// クリアする。設定済みの TimeoutSeconds は Task に残るので、次に実際の Planner/Worker
// が動いたときに新しい満額の予算がスタンプされる。coordStarted もリセットするのは、
// タイムアウトを発生させた調停器がすでに完了している（または最終 return 経路にある）ため。
func (e *Engine) resetTimeoutRevival(taskID string) {
	e.settling.Delete(taskID)
	e.deadline.Delete(taskID)
	e.stamped.Delete(taskID)
	e.coordStarted.Delete(taskID)
}

// stampFirstRun はタイムアウト対象タスクの最初の実実行（LLM 準備完了）時に
// first_run_at + deadline_at を記録する。プロセスごとに 1 回だけ。タイムアウトのない
// タスクでは何もしない。
func (e *Engine) stampFirstRun(t *Task) {
	if t.TimeoutSeconds <= 0 {
		return
	}
	if _, loaded := e.stamped.LoadOrStore(t.ID, true); loaded {
		return
	}
	dl, err := e.m.StampTaskFirstRun(t.ID)
	if err != nil {
		log.Printf("[deadline] task %s の first_run スタンプに失敗: %v", t.ID, err)
		e.stamped.Delete(t.ID) // 次回の再試行を許可する
		return
	}
	if dl > 0 {
		e.deadline.Store(t.ID, dl)
		log.Printf("[deadline] task %s 初回実行、期限は %s", t.ID, time.Unix(dl, 0).Format("2006-01-02 15:04:05"))
	}
}

// clockCtx はタスクの TaskClock（絶対 deadline）を実行の context に重ね、
// worker/planner がウォールクロック予算をクランプし、実行単位とタスクタイムアウトの
// どちらの締めくくり語を使うか選べるようにする。final は調停器主導の最終 planner
// ラウンドであることを示す。
func (e *Engine) clockCtx(base context.Context, t *Task, final bool) context.Context {
	dl := e.taskDeadline(t)
	if dl <= 0 && !final {
		return base // タイムアウトなし → 挙動は変わらない
	}
	return agent.WithTaskClock(base, agent.TaskClock{DeadlineUnix: dl, Final: final})
}

// ---------- 調停器 ----------

// startDeadlineCoordinator はタスクごとの deadline タイマーを一度だけ起動する（冪等）。
// Run() からも再起動時の再読み込み経路からも呼ばれるので、アクティブでない
// タイムアウト対象タスクも、planner/worker のループが動いていなくても期限後に
// 必ず締めくくられる。
func (e *Engine) startDeadlineCoordinator(ctx context.Context, t *Task) {
	if t == nil || t.TimeoutSeconds <= 0 {
		return
	}
	e.deleteMu.RLock()
	if e.IsDeleting(t.ID) {
		e.deleteMu.RUnlock()
		return
	}
	if _, loaded := e.coordStarted.LoadOrStore(t.ID, true); loaded {
		e.deleteMu.RUnlock()
		return
	}
	rt := e.registerTaskRoutines(ctx, t.ID, 1)
	e.deleteMu.RUnlock()
	runTaskRoutine(rt, func(loopCtx context.Context) { e.deadlineCoordinator(loopCtx, t) })
}

// deadlineCoordinator はタスクの絶対 deadline まで待ち、締めくくりシーケンスを実行する。
// 絶対ウォールクロック方式なので、pause 中もカウントし続ける。
func (e *Engine) deadlineCoordinator(ctx context.Context, t *Task) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if isTerminalStatus(e.m.TaskStatus(t.ID)) {
			return // すでに終了している（ゴール達成 / 失敗）— タイムアウトさせるものはない
		}
		dl := e.taskDeadline(t)
		if dl <= 0 {
			if sleepCtx(ctx, deadlinePollInterval) { // 未スタンプ（タスクがまだ実質実行されていない）
				return
			}
			continue
		}
		if remaining := time.Until(time.Unix(dl, 0)); remaining > 0 {
			nap := remaining
			if nap > deadlineMaxSleep {
				nap = deadlineMaxSleep
			}
			if sleepCtx(ctx, nap) {
				return
			}
			continue
		}
		e.settleTask(ctx, t)
		return
	}
}

// settleTask は順序立てた締めくくりシーケンスを 1 回だけ実行する（§4 の手順①〜⑥）。
func (e *Engine) settleTask(ctx context.Context, t *Task) {
	if !e.markSettling(t.ID) {
		return
	}
	log.Printf("[deadline] task %s がタイムアウト上限に到達、締めくくりシーケンスに入る", t.ID)

	// ④ 実行中の worker/planner の drain を待つ（実行中の run はクランプされた
	// MaxDuration により自力で締めくくりに入る）。grace を超えても空にならなければ
	// → そのタスクの exec ctx を強制 cancel（settling-aware な分岐が正しく分類する）。
	hardStop := time.Now().Add(settleDrainGrace)
	for e.inflightCount(t.ID) > 0 {
		if time.Now().After(hardStop) {
			log.Printf("[deadline] task %s の drain がタイムアウト(%s)、実行中の run を強制キャンセル", t.ID, settleDrainGrace)
			e.cancelExec(t.ID, agent.AbortSettleDrainTimeout)
			_ = sleepCtx(ctx, 3*time.Second) // worker の分岐が保存/分類を終える時間を少し与える
			break
		}
		if sleepCtx(ctx, 500*time.Millisecond) {
			return // エンジン全体が停止した
		}
	}

	// ⑤ 最終ラウンドの planner（タスクタイムアウト用の語、最後のゴール判定、新しい意図は生成しない）。
	met := e.runFinalPlannerRound(ctx, t)
	if !e.beginTaskOperation(t.ID) {
		return
	}
	defer e.decInflight(t.ID)

	// ⑥ 終状態を確定（ガード付き）: met → done(completed)、そうでなければ timeout。
	// 通常経路がすでに done を書いていれば、ガード（SetTaskStatusGuarded）が上書きを
	// 拒否し、completed の意味が保たれる。
	status := "timeout"
	if met {
		status = "done"
	}
	won, err := e.m.SetTaskStatusGuarded(t.ID, status)
	switch {
	case err != nil:
		log.Printf("[deadline] task %s の終状態確定に失敗: %v", t.ID, err)
	case won:
		log.Printf("[deadline] task %s の締めくくり完了、終状態=%s", t.ID, status)
	default:
		log.Printf("[deadline] task %s は締めくくり時にすでに終状態、元の状態を保持", t.ID)
	}
}

// runFinalPlannerRound はタスクタイムアウト用の planner 語を使った終端 planner
// ラウンドをちょうど 1 回だけ駆動する（最終ゴール判定。新しい意図は作らない）。
// LLM が準備できるまで待つ（ctx で有界）ので、達成可能なタスクが誤って timeout と
// 判定されることはない。
func (e *Engine) runFinalPlannerRound(ctx context.Context, t *Task) (met bool) {
	if e.IsDeleting(t.ID) {
		return false
	}
	planner, _ := e.snapshotFor(t)
	for planner == nil {
		if sleepCtx(ctx, deadlinePollInterval) {
			return false
		}
		if isTerminalStatus(e.m.TaskStatus(t.ID)) {
			return false
		}
		if e.IsDeleting(t.ID) {
			return false
		}
		planner, _ = e.snapshotFor(t)
	}
	// 独立した ctx（execCancel を付けず、pause/強制 cancel がこの最終ラウンドを
	// 中断しないようにする）。Final によりタスクタイムアウト用の語を注入する。
	fctx := e.clockCtx(ctx, t, true)
	if !e.beginTaskOperation(t.ID) {
		return false
	}
	defer e.decInflight(t.ID)
	emit := func(r db.Activity) { e.emitActivity(t, r) }
	e.emitActivity(t, db.Activity{Worker: "planner", Kind: "round",
		Summary: fmt.Sprintf("タスクタイムアウトの締めくくり・最終判定（第 %d ラウンド）", e.nextPlannerRound(t.ID))})
	tTaskID, _ := strconv.ParseInt(t.ID, 10, 64)
	e.BeginLLMCall(t.ID)
	met, reason, err := planner.Plan(fctx, tTaskID, e.m.assets, t.Store, t.Goal, t.drainTriggers(), emit)
	e.EndLLMCall(t.ID)
	if err != nil {
		log.Printf("[deadline] task %s の最終プランニングでエラー: %v", t.ID, err)
	} else if met {
		log.Printf("[deadline] task %s の最終判定でゴール達成: %s", t.ID, reason)
	}
	return met
}
