package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/jackc/pgx/v5/pgconn"
)

// isFKViolation は err が Postgres の外部キー違反（SQLSTATE 23503）かどうかを返す。
// 例えば親行を持たない exploration_id を持つ activity の INSERT など。
func isFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// dropReason は activity の書き込みがなぜ捨てられたかを分類する。ログを生のエラー
// テキストではなく原因ごとにまとめて分析できるようにするため。
func dropReason(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503":
			return "fk_violation(23503,親explorationなし)"
		case "23505":
			return "unique_violation(23505)"
		default:
			return "pg_error(" + pgErr.Code + ")"
		}
	}
	return "write_error"
}

// bumpDrop はタスクの、永続化できずに捨てられた activity レコードの累積数を
// インクリメントして返す。planner と worker が並行して emit するためここは競合する。
// そこで sync.Map の裏に atomic カウンタを置く。ログの件数で損失規模が一目で分かり、
// grep して数える手間が省ける。
func (e *Engine) bumpDrop(taskID string) int64 {
	v, _ := e.dropCnt.LoadOrStore(taskID, new(int64))
	return atomic.AddInt64(v.(*int64), 1)
}

// preview は改行を潰し、s を 1 行ログ用の短いルーン安全な断片に縮める
// （数 KB の summary/detail をそのままログに吐かないようにする）。
func preview(s string, n int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

// model_error（provider/API の障害：LLM 層の一時リトライを使い切った、あるいは
// ストリーム開始後に途中で切れた）で終わった work は「やってみたが終わらなかった」
// でも本当の失敗でもなく、外部の一時的な揺らぎである。既定でこれを恒久的な
// blocked 扱いにすると意図を 1 本無駄に失う。そこでこの終状態では追加で数回
// 再実行し、その間にバックオフを挟んで provider の復旧を待つ。リトライ中に
// pause/停止/キャンセルされたら即座に対応する分岐へ譲る。
const (
	modelErrorRetries      = 2               // model_error 終了後に追加で再試行する回数
	modelErrorRetryBackoff = 3 * time.Second // 各再試行前のバックオフ
	workControlWaitTimeout = 30 * time.Second
)

var errWorkControlConflict = errors.New("work control conflict")

// retryableWorkerModelError はタスクルーターがすでに処理したエラーを除外する。
// 特に、部分的なストリーミング後のクォータエラーは次の LLM 呼び出しのために
// タスクのカーソルを進めるが、この意図全体をバックアップ側で再生してはならない。
func retryableWorkerModelError(reason harness.TerminalReason, err error) bool {
	return reason == harness.ReasonModelError && !isTaskLLMRuntimeError(err)
}

// Engine は実 LLM エージェントによるイベント駆動の探索ループを駆動する（docs §4.3/§4.4）。
// 資産/探索グラフの変化（debounce 済み）で planner を起こし、planner はルートを読み、
// 資産を問い合わせ、ゴールを判定して意図を出す。N 本の並行 work エージェントが
// 意図をクレームして実行する。シミュレーションモードはなく、LLM provider が必須。
// planner/worker は実行時に（再）インストールできる（UI から LLM を設定）。
// ループは常に動くが、LLM が設定されるまでアイドルする。
type Engine struct {
	m        *Manager
	debounce time.Duration

	bc *Broadcaster // live activity pub/sub (SSE)

	started  sync.Map // taskID -> bool, so Run is idempotent per task
	lastAct  sync.Map // taskID -> int64 unix, last planner/worker activity (heartbeat)
	llmCalls sync.Map // taskID -> *int64, actual planner/worker/main-agent LLM calls
	paused   sync.Map // taskID -> bool, user-paused (planner + workers idle but loops alive)
	deleting sync.Map // taskID -> bool, delete barrier (no new task-owned writes)
	dropCnt  sync.Map // taskID -> *int64, running count of dropped (unpersistable) activity records

	// deleteMu makes installing the delete barrier atomic with registering a new
	// task operation. Once BeginDelete returns, every admitted writer is reflected
	// in inflight and every later writer is rejected.
	deleteMu sync.RWMutex

	// Every long-lived task goroutine (planner, workers and deadline coordinator)
	// runs under one task-scoped context. Successful deletion cancels that context,
	// waits for all goroutines, then releases every task-level Engine reference.
	runtimeMu sync.Mutex
	runtimes  map[string]*taskRuntime

	// per-task execution context: each planner.Plan / worker.Execute runs under it,
	// so pausing can CANCEL an in-flight run (not just skip the next one). Recreated
	// on resume since cancelling is one-shot. Every cancellation carries a named
	// cause so the activity trace can identify the initiating control path.
	execMu     sync.Mutex
	execCancel map[string]context.CancelCauseFunc
	execCtx    map[string]context.Context

	// Per-work control lets the planner kill a worker and lets the UI pause/cancel
	// one intent without pausing the whole task. The done channel closes only after
	// runWorkerStep has stopped writing and committed its final state.
	workMu sync.Mutex
	work   map[int64]*workExecution

	// steerBox queues planner course-corrections for a running work (keyed by intent
	// id). The worker's PreToolUse hook drains it before its next tool call and hands
	// the message to the model (blocking that call) so it re-plans — no kill needed.
	steerMu  sync.Mutex
	steerBox map[int64][]string

	plannerRound sync.Map // taskID -> int, planner round counter (for UI round separators)

	// タスク単位のタイムアウト（docs/任务级超时与收尾设计.md を参照）:
	settling     sync.Map // taskID -> bool, タスクが締めくくりシーケンスに入った（新規意図の割り当て/取得を停止）
	deadline     sync.Map // taskID -> int64 unix, 絶対期限（初回実行時にスタンプ;0/省略=無制限）
	stamped      sync.Map // taskID -> bool, first_run_at をスタンプ済みか（本プロセス内で 1 回だけ）
	inflight     sync.Map // taskID -> *int64, 実行中の planner.Plan + worker.Execute のカウント（drain 用）
	coordStarted sync.Map // taskID -> bool, deadline 調停器を起動済みか（Run/reload の重複排除）

	// resolve returns a task's dedicated planner/worker (wired by the server as the
	// authoritative task-router). nil,nil means this task is deliberately unavailable
	// (for example an exhausted failover chain) — there is no global-pair fallback.
	resolve              func(t *Task) (*agent.Planner, *agent.Worker)
	resolveAuthoritative bool
	// readiness reports whether a global LLM provider is configured — the signal behind
	// Ready()/the llm_configured indicator. Wired once at startup; nil → not ready.
	readiness func() bool
}

type taskRuntime struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type workExecution struct {
	cancel context.CancelCauseFunc
	done   chan error
	action string // user action: pause | cancel
}

// nextPlannerRound returns the next planner round number for a task (1-based).
func (e *Engine) nextPlannerRound(taskID string) int {
	v, _ := e.plannerRound.LoadOrStore(taskID, 0)
	n := v.(int) + 1
	e.plannerRound.Store(taskID, n)
	return n
}

// Pause stops a task: marks it paused AND cancels any in-flight planner/worker run
// for it (a long worker.Execute would otherwise keep going until it finishes).
func (e *Engine) Pause(taskID string, cause error) {
	e.paused.Store(taskID, true)
	e.cancelExec(taskID, cause)
}

// BeginDelete installs an execution barrier before task data/files are removed.
// The temporary pause is not a user pause. The server serializes this transition
// with lifecycle admission and tells AbortDelete whether the persisted task is
// paused/queued if cleanup fails.
func (e *Engine) BeginDelete(taskID string) bool {
	e.deleteMu.Lock()
	if _, loaded := e.deleting.LoadOrStore(taskID, true); loaded {
		e.deleteMu.Unlock()
		return false
	}
	e.paused.Store(taskID, true)
	e.deleteMu.Unlock()
	e.cancelExec(taskID, agent.AbortTaskDeleted)
	return true
}

func (e *Engine) AbortDelete(taskID string, keepPaused bool) {
	e.deleteMu.Lock()
	if !e.IsDeleting(taskID) {
		e.deleteMu.Unlock()
		return
	}
	e.deleting.Delete(taskID)
	if !keepPaused {
		e.paused.Delete(taskID)
	}
	e.deleteMu.Unlock()
	if !keepPaused && e.m != nil {
		if t, ok := e.m.Task(taskID); ok {
			t.Notify()
		}
	}
}

func (e *Engine) IsDeleting(taskID string) bool {
	_, ok := e.deleting.Load(taskID)
	return ok
}

// registerTaskRoutines reserves count goroutines in the task runtime. Callers
// hold deleteMu for reading so StopTask cannot race WaitGroup.Add with Wait.
func (e *Engine) registerTaskRoutines(parent context.Context, taskID string, count int) *taskRuntime {
	e.runtimeMu.Lock()
	defer e.runtimeMu.Unlock()
	rt := e.runtimes[taskID]
	if rt == nil {
		ctx, cancel := context.WithCancel(parent)
		rt = &taskRuntime{ctx: ctx, cancel: cancel}
		e.runtimes[taskID] = rt
	}
	rt.wg.Add(count)
	return rt
}

func runTaskRoutine(rt *taskRuntime, fn func(context.Context)) {
	go func() {
		defer rt.wg.Done()
		fn(rt.ctx)
	}()
}

// StopTask permanently stops every long-lived goroutine and removes all Engine
// state for a successfully deleted task. The delete barrier remains installed
// until cleanup finishes, so no new task operation can race the teardown.
func (e *Engine) StopTask(taskID string) {
	e.deleteMu.Lock()
	e.deleting.Store(taskID, true)
	e.deleteMu.Unlock()

	e.cancelExec(taskID, agent.AbortTaskDeleted)
	e.runtimeMu.Lock()
	rt := e.runtimes[taskID]
	if rt != nil {
		rt.cancel()
	}
	e.runtimeMu.Unlock()
	if rt != nil {
		rt.wg.Wait()
	}

	e.execMu.Lock()
	if cancel := e.execCancel[taskID]; cancel != nil {
		cancel(agent.AbortTaskDeleted)
	}
	delete(e.execCancel, taskID)
	delete(e.execCtx, taskID)
	e.execMu.Unlock()

	e.runtimeMu.Lock()
	if e.runtimes[taskID] == rt {
		delete(e.runtimes, taskID)
	}
	e.runtimeMu.Unlock()

	e.started.Delete(taskID)
	e.lastAct.Delete(taskID)
	e.llmCalls.Delete(taskID)
	e.paused.Delete(taskID)
	e.dropCnt.Delete(taskID)
	e.plannerRound.Delete(taskID)
	e.settling.Delete(taskID)
	e.deadline.Delete(taskID)
	e.stamped.Delete(taskID)
	e.inflight.Delete(taskID)
	e.coordStarted.Delete(taskID)
	e.deleteMu.Lock()
	e.deleting.Delete(taskID)
	e.deleteMu.Unlock()
}

// cancelExec cancels a task's current per-task exec context (any in-flight
// planner.Plan / worker.Execute), if present. Shared by Pause and the settle
// sequence's hard-drain backstop.
func (e *Engine) cancelExec(taskID string, cause error) {
	e.execMu.Lock()
	if cancel := e.execCancel[taskID]; cancel != nil {
		cancel(cause)
	}
	e.execMu.Unlock()
}

// Resume un-pauses a task and nudges a fresh planning round. The next exec under
// it gets a fresh (uncancelled) context.
func (e *Engine) Resume(t *Task) {
	// BeginDelete owns the pause barrier once deletion starts. A concurrent
	// resume must never clear it and let a planner/worker re-enter while cleanup
	// is waiting for task operations to drain.
	if t == nil {
		return
	}
	e.deleteMu.RLock()
	defer e.deleteMu.RUnlock()
	if e.IsDeleting(t.ID) {
		return
	}
	e.paused.Delete(t.ID)
	t.Notify()
}

// execContextFor returns a live per-task context derived from parent, recreating
// it if a prior pause cancelled it.
func (e *Engine) execContextFor(parent context.Context, taskID string) context.Context {
	e.execMu.Lock()
	defer e.execMu.Unlock()
	if e.IsPaused(taskID) {
		// never hand out a live context while paused (guards the claim→Execute race)
		c, cancel := context.WithCancelCause(parent)
		cancel(agent.AbortPausedRaceGuard)
		return c
	}
	if c := e.execCtx[taskID]; c != nil && c.Err() == nil {
		return c
	}
	c, cancel := context.WithCancelCause(parent)
	e.execCtx[taskID] = c
	e.execCancel[taskID] = cancel
	return c
}

// IsPaused reports whether a task is user-paused.
func (e *Engine) IsPaused(taskID string) bool {
	v, ok := e.paused.Load(taskID)
	return ok && v.(bool)
}

// Started reports whether the engine loops are running for a task.
func (e *Engine) Started(taskID string) bool {
	_, ok := e.started.Load(taskID)
	return ok
}

// LastActivity returns the unix time of the last planner/worker activity for a
// task (0 if none yet).
func (e *Engine) LastActivity(taskID string) int64 {
	if v, ok := e.lastAct.Load(taskID); ok {
		return v.(int64)
	}
	return 0
}

// BeginLLMCall/EndLLMCall track actual provider calls separately from the
// scheduler's task-operation counter. A task can have live loops while all of
// them are waiting for a trigger; that state must remain idle in the UI.
func (e *Engine) BeginLLMCall(taskID string) {
	v, _ := e.llmCalls.LoadOrStore(taskID, new(int64))
	atomic.AddInt64(v.(*int64), 1)
}

func (e *Engine) EndLLMCall(taskID string) {
	if v, ok := e.llmCalls.Load(taskID); ok {
		p := v.(*int64)
		if atomic.AddInt64(p, -1) <= 0 {
			atomic.StoreInt64(p, 0)
		}
	}
}

func (e *Engine) ActiveLLMCalls(taskID string) int64 {
	if v, ok := e.llmCalls.Load(taskID); ok {
		return atomic.LoadInt64(v.(*int64))
	}
	return 0
}

func (e *Engine) touch(taskID string) { e.lastAct.Store(taskID, time.Now().Unix()) }

func NewEngine(m *Manager) *Engine {
	return &Engine{m: m, debounce: 800 * time.Millisecond, bc: NewBroadcaster(),
		execCancel: map[string]context.CancelCauseFunc{}, execCtx: map[string]context.Context{},
		work: map[int64]*workExecution{}, steerBox: map[int64][]string{},
		runtimes: map[string]*taskRuntime{}}
}

// registerWork records the cancel for the work currently running intentID.
func (e *Engine) registerWork(intentID int64, cancel context.CancelCauseFunc) {
	e.workMu.Lock()
	e.work[intentID] = &workExecution{cancel: cancel, done: make(chan error, 1)}
	e.workMu.Unlock()
}

// detachWork removes the live control handle once Execute has returned. complete
// must be called after the final intent state write so a waiting cancel handler can
// safely delete the worker's blackboard output without racing a late write.
func (e *Engine) detachWork(intentID int64) (action string, complete func(error)) {
	e.workMu.Lock()
	run := e.work[intentID]
	if run != nil {
		delete(e.work, intentID)
		action = run.action
		run.cancel(agent.AbortWorkFinished) // release resources (no-op if already cancelled)
	}
	e.workMu.Unlock()
	e.steerMu.Lock()
	delete(e.steerBox, intentID) // drop any undelivered steering for a finished work
	e.steerMu.Unlock()
	if run == nil {
		return action, func(error) {}
	}
	return action, func(err error) { run.done <- err }
}

// ControlWork requests a user-visible pause or cancellation and waits until the
// worker has fully stopped writing. Cancellation cleanup is performed by the API
// handler after this returns; pause state is committed by runWorkerStep itself.
func (e *Engine) ControlWork(ctx context.Context, intentID int64, action string) error {
	if action != "pause" && action != "cancel" {
		return fmt.Errorf("unsupported work action %q", action)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	e.workMu.Lock()
	run := e.work[intentID]
	if run == nil {
		e.workMu.Unlock()
		return fmt.Errorf("%w: 意図 %d には現在実行中の work がない（すでに終了したか、まだ取得されていない）", errWorkControlConflict, intentID)
	}
	if run.action != "" {
		e.workMu.Unlock()
		return fmt.Errorf("%w: 意図 %d は現在 %s 操作を実行中", errWorkControlConflict, intentID, run.action)
	}
	run.action = action
	done := run.done
	cause := error(agent.AbortWorkPausedByUser)
	if action == "cancel" {
		cause = agent.AbortWorkCancelledByUser
	}
	run.cancel(cause)
	e.workMu.Unlock()

	timer := time.NewTimer(workControlWaitTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		e.releaseWorkControl(intentID, run, action)
		return fmt.Errorf("意図 %d の %s 完了を待機中: %w", intentID, action, ctx.Err())
	case <-timer.C:
		e.releaseWorkControl(intentID, run, action)
		return fmt.Errorf("意図 %d の %s 完了を待機中: %w", intentID, action, context.DeadlineExceeded)
	}
}

// releaseWorkControl は待機をキャンセルされた呼び出し元自身の予約だけを外す。
// work の context はキャンセルされたまま残り、runWorkerStep が名前付きの
// キャンセル原因を見て、HTTP 呼び出し元が消えていても復旧可能な paused 状態へ
// 意図を落ち着かせる。
func (e *Engine) releaseWorkControl(intentID int64, run *workExecution, action string) {
	e.workMu.Lock()
	if current := e.work[intentID]; current == run && current.action == action {
		current.action = ""
	}
	e.workMu.Unlock()
}

func transitionIntentState(store *db.ExplorationStore, intentID int64, expected, state string) error {
	changed, err := store.CompareAndSetIntentState(intentID, expected, state)
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("%w: 意図 %d は %s 状態ではなくなった", db.ErrIntentStateConflict, intentID, expected)
	}
	return nil
}

// SteerWork は intentID を実行中の work に対する実行中の方向修正をキューに入れる
// （planner の steer_work ツール）。worker は次のツール呼び出し前にそれを届けて
// 再計画する——kill はしない。その意図を実行中の work がなければエラー。
func (e *Engine) SteerWork(intentID int64, msg string) error {
	if strings.TrimSpace(msg) == "" {
		return fmt.Errorf("方向修正メッセージを空にはできません")
	}
	e.workMu.Lock()
	running := e.work[intentID] != nil
	e.workMu.Unlock()
	if !running {
		return fmt.Errorf("意図 %d には現在実行中の work がない（すでに終了したか、まだ取得されていない）", intentID)
	}
	e.steerMu.Lock()
	e.steerBox[intentID] = append(e.steerBox[intentID], msg)
	e.steerMu.Unlock()
	return nil
}

// drainSteer は intentID にキューされた最も古い方向修正メッセージを（FIFO で）取り出す。
func (e *Engine) drainSteer(intentID int64) (string, bool) {
	e.steerMu.Lock()
	defer e.steerMu.Unlock()
	q := e.steerBox[intentID]
	if len(q) == 0 {
		return "", false
	}
	msg := q[0]
	if len(q) == 1 {
		delete(e.steerBox, intentID)
	} else {
		e.steerBox[intentID] = q[1:]
	}
	return msg, true
}

// steerHooks は guard の hook ランナーを包み、planner が実行中の work を操舵できる
// ようにする。ツール呼び出しのたびにキューの方向修正があれば取り出し、その呼び出しを
// ブロックしてメッセージをモデルに返す——モデルはツールを実行する代わりに次の手を
// 再計画する。キューが空なら guard は従来とまったく同じに振る舞う。
// 「空転ターン」の継続実行も合わせて担当する（Stop を参照）。
type steerHooks struct {
	inner harness.HookRunner
	drain func() (string, bool)
	// nudges はこの意図で注入済みの空転継続回数で、上限は limit。ポインタなのは
	// harness が steerHooks の値コピーを保持するため、カウンタを同一実体で共有する必要があるから。
	nudges *atomic.Int64
	// limit は空転継続の回数上限で、Engine.emptyTurnNudgeLimit() が「空応答リトライ
	// 回数」から解決する。<=0 = 介入しない（ユーザーがこの層を明示的に切った）。
	limit int
	// label は "worker-1 · #42" のような形式で、ログ専用。
	label string
}

// 空転ターン（思考だけで、本文もツール呼び出しもない）の継続回数の既定値。SDK の
// 空応答リトライの組み込み既定（norma/llm/openai.go の emptyResponseRetries）と
// 揃えてある——2 層が同じダイヤルを共有するので、未設定時の挙動も一致すべき。
// 解決は Engine.emptyTurnNudgeLimit を参照。
//
// この数は「1 つの意図の総量」であって「連続何回」ではない点に注意。harness 自身の
// stopHookActive が連続空転を 1 回だけに制限している——1 回押してなお空転なら Stop
// フックはもう呼ばれず、run はそのまま終わる。クォータが回復するのは実際にツール
// ターンが 1 回あったときだけ（norma/harness/query.go:534）。したがってこのゲートが
// 防ぐのは「ツール → 空転 → 押す → ツール → 空転」という病的なループであり、
// 意図の予算を使い切らせないためのもの。
const defaultEmptyTurnNudges = 2

// emptyTurnNudge は空転ターンに注入する継続指示。
//
// このターンは harness から見ると自然終了（stop_reason=end_turn で tool_use なし）で、
// LLM の五層リトライはいずれも該当しない——エラーではなく、モデルが「考え終えたが
// 手を動かさなかった」だけだから。SDK の空応答リトライも届かない。あちらは
// 「イベントを yield したか」で空を判定するが、思考デルタ自体がイベント
// （norma/llm/openai.go の SEThinkingDelta）なので thinking-only は空とみなされない。
// しかもあの層はプロンプト全体をそのまま再送するが、コンテキストの形に起因するこの
// 種の空転では、再送してもモデルがもう一度考えるだけだ。ここでは指示を 1 本追記して、
// すでに出た思考を携えて続行させる——入力が変われば違う振る舞いをする理由ができる。
const emptyTurnNudge = "【空転の注意】あなたの前のターンは思考過程だけを出力し、本文の返答もツール呼び出しも行いませんでした。" +
	"このターンは何も生み出していないのと同じです。先ほど考えた次の一手をそのまま実行してください：ツールを呼び出すか、結論の文章を出すか。考え直しは不要です。"

// isThinkingOnlyTurn は直近の assistant ターンが本文もツール呼び出しも生み出さなかった
// か——つまりモデルがラウンド全体を思考に費やしたか——を返す。
func isThinkingOnlyTurn(messages []llm.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		return strings.TrimSpace(m.Text()) == "" && len(m.ToolUses()) == 0
	}
	return false
}

func (h steerHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if msg, ok := h.drain(); ok {
		return true, "【プランナーによるリアルタイム方向修正】" + msg +
			"\n（これはプランナーのこの意図に対する即時指示です。今回のツール呼び出しは実行されていません。これに従って次の一手を調整してください。現在の予定と衝突する場合はこちらを優先してください。）", nil
	}
	if h.inner != nil {
		return h.inner.PreToolUse(ctx, name, input)
	}
	return false, "", nil
}

func (h steerHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if h.inner != nil {
		h.inner.PostToolUse(ctx, name, input, result, isErr)
	}
}

// Stop は guard 本来の意味に加えて「空転ターン」の継続実行を重ねる。モデルが思考だけを
// 出力し本文もツールも出さなかったとき、harness はそれを自然終了とみなして空の summary で
// 終える（query.go の ReasonCompleted + asst.Text()）。まだ終わっていない意図がこうして
// 途中で切れてしまう。そこで継続指示を 1 本注入し、モデルに既存の思考を携えて先へ進ませる。
func (h steerHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	var (
		prevent  bool
		blocking []string
		msg      string
	)
	if h.inner != nil {
		prevent, blocking, msg = h.inner.Stop(ctx, messages)
	}
	// inner がすでにハードストップを決めている、または自前の継続メッセージを注入する
	// つもりなら → それを尊重し、重ねない。
	// limit<=0 = ユーザーが「空応答リトライ回数」を -1 にして、この層を明示的に切っている。
	if prevent || len(blocking) > 0 || h.nudges == nil || h.limit <= 0 || !isThinkingOnlyTurn(messages) {
		return prevent, blocking, msg
	}
	n := h.nudges.Add(1)
	if n > int64(h.limit) {
		log.Printf("[work %s] 空転ターン（思考のみ、本文もツールもなし）が継続上限 %d に達したため、そのまま終了させる", h.label, h.limit)
		return prevent, blocking, msg
	}
	log.Printf("[work %s] 空転ターン（思考のみ、本文もツールもなし）、継続指示を注入 (%d/%d)", h.label, n, h.limit)
	return false, []string{emptyTurnNudge}, ""
}

// KillWork は intentID を実行中の work をキャンセルする（planner の kill_work ツール）。
// work の agent-core セッションは ctx のキャンセルに従い、速やかに中断する。
func (e *Engine) KillWork(intentID int64) error {
	e.workMu.Lock()
	run := e.work[intentID]
	e.workMu.Unlock()
	if run == nil {
		return fmt.Errorf("意図 %d には現在実行中の work がない（すでに終了したか、まだ取得されていない）", intentID)
	}
	run.cancel(agent.AbortKilledByPlanner)
	return nil
}

// Broadcaster はエンジンのライブ activity pub/sub を公開する（SSE ハンドラが使う）。
func (e *Engine) Broadcaster() *Broadcaster { return e.bc }

// emitActivity は取得した 1 ステップを永続化し、同時にライブ購読者へ配信する。
// 単一の場所から行うことで、ストレージと SSE ストリームが食い違わないようにする。
func (e *Engine) emitActivity(t *Task, r db.Activity) db.Activity {
	id, err := e.appendActivity(t, r)
	if err != nil {
		// もはや沈黙しない:レコードを捨てると trace の command↔result 対応が壊れる——
		// tool_result を失った tool_use は永久に「実行中」と表示され、'result'/'round'
		// レコードを失ったセッションは summary なし（「まとめなし」）になる。根本原因の
		// 分析に必要な情報を 1 行の error レベルログに詰め込む:理由クラス、summary の
		// プレビュー、このタスクの累積ドロップ件数、そして FK の場合は親 exploration に
		// 到達できない理由のライブ調査結果。
		n := e.bumpDrop(t.ID)
		diag := ""
		// FK 親失敗(23503)では DB をライブで調べ、exploration に到達できない理由
		// （行が消えた / expID が違う）までログに残す。単に「到達できない」だけでは不十分。
		if isFKViolation(err) {
			storeID := t.Store.ID()
			if exists, refs, maxID, dErr := e.m.pg.ExplorationDiag(storeID); dErr != nil {
				diag = fmt.Sprintf(" | FK 診断クエリの失敗(store.expID=%d task.ExpID=%d): %v", storeID, t.ExpID, dErr)
			} else {
				diag = fmt.Sprintf(" | FK 診断: store.expID=%d task.ExpID=%d exploration存在=%v これを参照するtask数=%d MAX(exploration.id)=%d",
					storeID, t.ExpID, exists, refs, maxID)
			}
		}
		log.Printf("[activity] task %s 活動レコードを破棄（このタスク累計 %d 件） worker=%s kind=%s tool=%s tuid=%s reason=%s summary=%q: %v%s",
			t.ID, n, r.Worker, r.Kind, r.Tool, r.ToolUseID, dropReason(err), preview(r.Summary, 80), err, diag)
		e.touch(t.ID)
		return r
	}
	r.ID = id
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	e.bc.Publish(t.ID, r)
	e.touch(t.ID)
	return r
}

// appendActivity は activity 行を 1 件永続化し、書き込み失敗時に少しリトライする。
// planner と worker が同じ exploration の activity ログに並行して INSERT すると
// たまに失敗するが、数回の素早いリトライで大半は回復する。重要なのは、すべての失敗が
// 今は LOG に出る（以前は `if err == nil` に飲み込まれていた）ことで、根本的な DB
// エラーがようやく診断できるようになった。
func (e *Engine) appendActivity(t *Task, r db.Activity) (int64, error) {
	var id int64
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if id, err = t.Store.AppendActivity(r); err == nil {
			if attempt > 1 {
				log.Printf("[activity] task %s への書き込みが %d 回目のリトライで成功 (worker=%s kind=%s tool=%s)",
					t.ID, attempt, r.Worker, r.Kind, r.Tool)
			}
			return id, nil
		}
		log.Printf("[activity] task %s への書き込みに失敗 (%d/3 回目, worker=%s kind=%s tool=%s expID=%d): %v",
			t.ID, attempt, r.Worker, r.Kind, r.Tool, t.Store.ID(), err)
		time.Sleep(time.Duration(attempt) * 25 * time.Millisecond)
	}
	return 0, err
}

// SetReadiness wires the global "an LLM provider is configured" predicate (read by
// Ready() / the llm_configured indicator). Called once at startup.
func (e *Engine) SetReadiness(fn func() bool) { e.readiness = fn }

// SetAgentResolver installs a per-task planner/worker resolver (wired by the server).
// Called once at startup before any task loop runs, so no lock is needed on reads.
func (e *Engine) SetAgentResolver(fn func(t *Task) (*agent.Planner, *agent.Worker)) {
	e.resolve = fn
	e.resolveAuthoritative = false
}

// SetAuthoritativeAgentResolver installs a resolver whose nil result must not
// fall through to the global provider. Task-level failover chains use this so a
// fully exhausted chain cannot silently bypass its configured boundary.
func (e *Engine) SetAuthoritativeAgentResolver(fn func(t *Task) (*agent.Planner, *agent.Worker)) {
	e.resolve = fn
	e.resolveAuthoritative = true
}

// snapshotFor returns the planner/worker a task should run on, from the task-router
// resolver. nil,nil means the task is deliberately unavailable (e.g. an exhausted
// failover chain); there is no global-pair fallback.
func (e *Engine) snapshotFor(t *Task) (*agent.Planner, *agent.Worker) {
	if e.resolve != nil {
		p, w := e.resolve(t)
		if (p != nil && w != nil) || e.resolveAuthoritative {
			return p, w
		}
	}
	return nil, nil
}

// Ready reports whether a global LLM provider is configured (via the readiness
// predicate wired at startup).
func (e *Engine) Ready() bool {
	return e.readiness != nil && e.readiness()
}

// ReadyFor reports whether a specific task can resolve a planner/worker pair.
// An explicit task profile chain can be runnable even when no global default
// provider is configured, so task status must not rely on Ready alone.
func (e *Engine) ReadyFor(t *Task) bool {
	p, w := e.snapshotFor(t)
	return p != nil && w != nil
}

// Run starts the planner loop + N worker loops for a task. The loops always run
// but no-op until an LLM is configured (so a task created while idle picks up
// automatically once LLM is set from the UI).
func (e *Engine) Run(ctx context.Context, t *Task) {
	workers := e.m.Workers()
	e.deleteMu.RLock()
	if e.IsDeleting(t.ID) {
		e.deleteMu.RUnlock()
		return
	}
	if _, loaded := e.started.LoadOrStore(t.ID, true); loaded {
		e.deleteMu.RUnlock()
		t.Notify() // already running — just nudge a planning round
		return
	}
	rt := e.registerTaskRoutines(ctx, t.ID, 1+workers)
	e.deleteMu.RUnlock()
	e.touch(t.ID)
	runTaskRoutine(rt, func(loopCtx context.Context) { e.plannerLoop(loopCtx, t) })
	for i := 0; i < workers; i++ {
		name := fmt.Sprintf("work#%d", i+1)
		runTaskRoutine(rt, func(loopCtx context.Context) { e.workerLoop(loopCtx, t, name) })
	}
	e.startDeadlineCoordinator(ctx, t) // タスク単位のタイムアウトタイマー（timeout>0 のみ。重複排除）
	// 「活動中の意図(open+running)がまったくない」場合にのみ初回プランニングを kick する。
	// シード意図付きのタスクは、シードがすでに open か、上で起動した worker に先に claim されて
	// running になっている——どちらも「やることがある」なので初回 planner を一律スキップし、
	// worker がシード意図を直接取得して走り、終わったら NotifyDone/ハートビートで planner を起こす。
	// ⚠️ Frontier(open だけ数える)は使えない:worker の取得(open→running)とこのチェックには
	// 競合があり、誤って kick してしまう。再起動からの自動復帰時も running 意図しか残って
	// いないことがあり、同様にスキップすべき。
	if has, _ := t.Store.HasActiveIntent(); !has {
		t.Notify() // kick the first planning round (acted on once LLM is ready)
	}
}

// plannerHeartbeatInterval はタスクの planner ハートビート間隔を解決する。db.CreateTask で
// すでに正規化済み（600 未満は一律 600 に引き上げ）。ここでもう一度下限を掛け、
// メモリ上の異常値に備える。
func plannerHeartbeatInterval(t *Task) time.Duration {
	sec := t.PlanHeartbeatSeconds
	if sec < db.MinPlanHeartbeatSeconds { // 下限=既定=600(10min)
		sec = db.MinPlanHeartbeatSeconds
	}
	return time.Duration(sec) * time.Second
}

// resetPlannerTimer は発火済みかもしれない Timer を安全に再アームする
// （標準的な Stop→drain→Reset パターン）。
func resetPlannerTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

func (e *Engine) plannerLoop(ctx context.Context, t *Task) {
	interval := plannerHeartbeatInterval(t)
	// ハートビートタイマーはループ入口でアームする = タスク開始から計時する。初回 planner を
	// スキップしたシードタスク（Run で frontier が空でないため初回を kick しない）でも、
	// ここでずっとブロックしていれば「タスク開始 + interval」でハートビートが初回プランニングを
	// 発火する。以降はウェイクアップ（エッジ/ハートビート）のたびに再アームする = 前回の
	// 任意のプランニング発火からの経過時間。
	heartbeat := time.NewTimer(interval)
	defer heartbeat.Stop()

	// runRound は 1 ラウンドのプランニングを実行する（debounce による合流 + 各種 guard を含む）。
	// src はログで発火元を区別するためだけに使う。
	runRound := func(src string) {
		// debounce: coalesce a burst of changes into one planning round
		timer := time.NewTimer(e.debounce)
	drain:
		for {
			select {
			case <-t.notify:
			case <-timer.C:
				break drain
			}
		}
		planner, _ := e.snapshotFor(t)
		if planner == nil {
			return // idle until LLM configured
		}
		if e.IsPaused(t.ID) {
			return // user-paused: don't plan
		}
		if e.IsDeleting(t.ID) {
			return
		}
		// terminal task (goals all met → done, or failed): the run is over. A
		// resume/nudge — e.g. auto-resume of the active task on restart — must NOT
		// re-plan (it would burn an LLM round and re-confirm a settled result).
		if isTerminalStatus(t.lifecycleSnapshot().Status) {
			return
		}
		// タスクタイムアウトの締めくくり中:通常のウェイクアップを捨てる——worker の締めくくり
		// 書き戻しも Resume の Notify も通常のプランニングラウンドを起こさない。最終ラウンドは
		// 調停器（settleTask）が直接駆動し、ここは通らない。
		if e.isSettling(t.ID) {
			return
		}
		// goalless（人手直接投入）分岐：タスクに open なゴールがなければ planner は走らない。
		// 走ると met を再判定して cancelExec し、ユーザーが主エージェント経由で直接投入した
		// 意図を殺してしまう。終了するかどうかは frontier が決める：open/running の意図が
		// 残っていれば running のまま静かに待ち、意図がすべて枯れたら done にする。
		// 全体が純粋な Go で、LLM 呼び出しもせず、プランニングラウンドのマーカーも打たない。
		if open, err := t.Store.HasOpenGoal(); err == nil && !open {
			t.drainTriggers() // 溜まった done/finding トリガーを捨て、goalless な長時間セッションでの無制限増加を防ぐ
			if active, err := t.Store.HasActiveIntent(); err == nil && !active {
				// frontier が枯渇し、実行中の意図もない → 締めくくり。Guarded 版で CAS し、
				// 並行する pause/delete/タイムアウト締めくくりの状態遷移を踏まないようにする。
				if won, err := e.m.SetTaskStatusGuarded(t.ID, "done"); err != nil {
					log.Printf("[goalless] task %s の締めくくりで done 確定に失敗: %v", t.ID, err)
				} else if won {
					e.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
						Summary: "ゴールはすべて達成済み、直接投入された意図も実行完了、タスク終了"})
				}
			}
			return // goalless 分岐は決して planner.Plan に入らない
		}
		if !e.beginTaskOperation(t.ID) {
			return
		}
		defer e.decInflight(t.ID)
		e.stampFirstRun(t) // 初回の実プランニング → first_run_at をスタンプ + deadline を算出（timeout 付きタスクのみ）
		e.touch(t.ID)
		emit := func(r db.Activity) { e.emitActivity(t, r) }
		ectx := e.clockCtx(e.execContextFor(ctx, t.ID), t, false) // cancellable by Pause; タスク deadline 付き
		if ectx.Err() != nil || e.IsDeleting(t.ID) {
			return
		}
		log.Printf("[planner] task %s プランニング中…(%s トリガー)", t.ID, src)
		// round marker: each Plan() is one planner round; emit a boundary so the
		// UI can separate rounds in the transcript (kind='round').
		e.emitActivity(t, db.Activity{Worker: "planner", Kind: "round",
			Summary: fmt.Sprintf("第 %d ラウンドのプランニング", e.nextPlannerRound(t.ID))})
		// what fired this round (worker done / finding; may be several — debounce
		// coalesces a burst; empty for time/heartbeat wakes).
		triggers := t.drainTriggers()
		taskIDInt, _ := strconv.ParseInt(t.ID, 10, 64)
		e.BeginLLMCall(t.ID)
		met, reason, err := planner.Plan(ectx, taskIDInt, e.m.assets, t.Store, t.Goal, triggers, emit)
		e.EndLLMCall(t.ID)
		switch {
		case err != nil && ectx.Err() == nil:
			log.Printf("[planner] task %s プランニングエラー: %v", t.ID, err)
		case met:
			log.Printf("[planner] task %s ゴール達成と判定: %s", t.ID, reason)
			// すべてのゴール達成 → タスク状態を done として永続化（フロントの DTO はこの終状態を優先表示する）。
			if err := e.m.SetTaskStatus(t.ID, "done"); err != nil {
				log.Printf("[planner] task %s の完了マークを DB に保存できず: %v", t.ID, err)
			}
			// タスクが完了と判定された → 実行中の worker を即座にキャンセルする。手元の意図を
			// 実行してももう意味がない。次の worker ループは終状態ゲートに当たって新しい意図を
			// 取得せず、キャンセルされたものは下の「タスク完了」分岐で stopped（blocked ではない）に分類される。
			e.cancelExec(t.ID, agent.AbortGoalMet)
		default:
			log.Printf("[planner] task %s プランニング完了", t.ID)
		}
		e.touch(t.ID)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.notify:
			runRound("edge") // worker 終了 / finding / kill / resume / seed 初回
		case <-heartbeat.C:
			// 周期的なフォールバック:デッドロックのフォールバック + 飛行中の worker の監督（steer/kill）
			// のための起床 + 周期的な見直し。
			runRound("heartbeat")
		}
		// ウェイクアップ（エッジまたはハートビート）のたびにハートビートを再アームする。
		// プランニングのトリガーが何であれ、この静置タイマーを計算し直す。
		resetPlannerTimer(heartbeat, interval)
	}
}

func (e *Engine) workerLoop(ctx context.Context, t *Task, name string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_, worker := e.snapshotFor(t)
		if worker == nil {
			if sleepCtx(ctx, 1500*time.Millisecond) {
				return
			}
			continue
		}
		if e.IsPaused(t.ID) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // user-paused: don't claim/execute intents
		}
		if e.IsDeleting(t.ID) {
			return
		}
		if e.isSettling(t.ID) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // タスクタイムアウトの締めくくり中:新しい意図を取得しない（実行中のものは自力で締め、調停器が drain を待つ）
		}
		if isTerminalStatus(e.m.TaskStatus(t.ID)) {
			if sleepCtx(ctx, 1000*time.Millisecond) {
				return
			}
			continue // タスクが終状態(done/failed/timeout):残った意図の取得を止め、完了後に frontier を空回りさせない
		}
		if !e.beginTaskOperation(t.ID) {
			return
		}
		claimed := e.runWorkerStep(ctx, t, name, worker)
		e.decInflight(t.ID)
		if !claimed && sleepCtx(ctx, 800*time.Millisecond) {
			return
		}
	}
}

// runWorkerStep は frontier から意図を 1 本取得し、runIntent 経由で完全に片付ける。
// 取得できるものがなければ false を返す。呼び出し元はプールの worker ループのみ。
func (e *Engine) runWorkerStep(ctx context.Context, t *Task, name string, worker *agent.Worker) bool {
	intent := e.claimNext(t, name)
	if intent == nil {
		return false
	}
	log.Printf("[worker %s] task %s 意図 #%d を取得", name, t.ID, intent.ID)
	return e.runIntent(ctx, t, name, worker, intent, "", "")
}

// runIntent はすでに取得済み（state=running）の意図を 1 本実行し、完全に片付ける。
// プールの worker ループ（runWorkerStep 経由）と人手メッセージハンドラ（runDetachedIntent、
// worker プール外の専用 goroutine）の両方が呼ぶので、実行/リトライ/状態書き込みの
// ロジックは 1 か所だけにある。message が非空なら ExecuteWithMessage でこのターンの
// 入力として注入する。requestID は転記のマーカーをキーし、model_error リトライをまたいだ
// 再注入を重複排除する。呼び出し元はシーケンス全体を通してタスク操作のアドミッションを
// 1 つ保持していること。そうすれば LLM の返却から最終 DB 書き込みまでの間に delete が
// 静止状態を観測することはない。
func (e *Engine) runIntent(ctx context.Context, t *Task, name string, worker *agent.Worker, intent *db.Node, requestID, message string) bool {
	hasChatMessage := message != ""
	e.stampFirstRun(t) // 初回の実実行 → first_run_at をスタンプ + deadline を算出（timeout 付きタスクのみ）
	e.touch(t.ID)
	emit := func(r db.Activity) { e.emitActivity(t, r) }
	ectx := e.clockCtx(e.execContextFor(ctx, t.ID), t, false) // cancellable by Pause; タスク deadline 付き
	if ectx.Err() != nil || e.IsDeleting(t.ID) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "open"); err != nil {
			log.Printf("[worker %s] task %s 意図 #%d の取得後巻き戻しに失敗: %v", name, t.ID, intent.ID, err)
		}
		return true
	}
	// work 単位の子 context。planner の kill_work がこの work だけを止められるようにする。
	workCtx, workCancel := context.WithCancelCause(ectx)
	e.registerWork(intent.ID, workCancel)
	// guard のフックを包み、steer_work が「この」意図に対して実行中の方向修正を
	// 注入できるようにする（worker の次のツール呼び出し前に取り出される）。
	iid := intent.ID
	taskEmit := func(a db.Activity) {
		nid := iid
		a.NodeID, a.Worker = &nid, name
		emit(a)
	}
	label := fmt.Sprintf("%s · #%d", name, iid)
	workCtx = intercept.WithTaskContext(workCtx, t.ID, label, taskEmit)
	// nudges は意図的に model_error の再実行ループの外に置く。空転継続の上限は
	// 「この意図」の総量であり、1 ラウンド再実行するたびにクォータをゼロに戻すべきではない。
	hooks := steerHooks{
		inner:  t.Guard.Hooks(),
		drain:  func() (string, bool) { return e.drainSteer(iid) },
		nudges: &atomic.Int64{},
		limit:  e.emptyTurnNudgeLimit(),
		label:  label,
	}
	wTaskID, _ := strconv.ParseInt(t.ID, 10, 64)
	e.BeginLLMCall(t.ID)
	var reason harness.TerminalReason
	var wrote agent.WriteCounts
	var err error
	if hasChatMessage {
		reason, wrote, err = worker.ExecuteWithMessage(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding, requestID, message)
	} else {
		reason, wrote, err = worker.Execute(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding)
	}
	e.EndLLMCall(t.ID)
	// model_error で終わった → 追加で数回再実行（バックオフ後に再試行）。意図がまだ本 work に
	// 属し、タスクが pause/停止/キャンセルされておらず【かつ締めくくりに入っていない】ときだけ
	// 再試行する。そうでなければ対応する分岐に譲る（締めくくり期間中は再試行しない。バックオフが
	// 他の worker の優雅な締めくくりの窓を圧迫しないように）。
	maxRetries, retryBackoff := e.modelErrorRetryPolicy()
	for attempt := 1; attempt <= maxRetries &&
		retryableWorkerModelError(reason, err) &&
		workCtx.Err() == nil && ectx.Err() == nil && !e.IsPaused(t.ID) && !e.isSettling(t.ID); attempt++ {
		log.Printf("[worker %s] task %s 意図 #%d が model_error で終了、%v 後に再試行 (%d/%d)",
			name, t.ID, intent.ID, retryBackoff, attempt, maxRetries)
		if sleepCtx(workCtx, retryBackoff) {
			break // バックオフ中にキャンセルされた（停止/一時停止）→ 下の分岐に任せる
		}
		e.BeginLLMCall(t.ID)
		if hasChatMessage {
			reason, wrote, err = worker.ExecuteWithMessage(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding, requestID, message)
		} else {
			reason, wrote, err = worker.Execute(workCtx, name, wTaskID, e.m.assets, t.Store, intent, hooks, emit, e.m.enrich, t.NotifyFinding)
		}
		e.EndLLMCall(t.ID)
	}
	// detachWork が workCtx をキャンセルする前に kill 状態を記録する。kill = タスクの
	// ctx は動いたまま、この work の ctx だけがキャンセルされた（planner の kill_work）。
	// pause の場合はタスクの ctx(ectx) がキャンセルされる。unregister の後に
	// workCtx.Err() を見ると常に true になる（unregister がキャンセルする）ので、
	// 完了したすべての work が誤って stopped 扱いになってしまう。
	workCause := context.Cause(workCtx)
	killed := workCtx.Err() != nil && ectx.Err() == nil
	action, completeWork := e.detachWork(intent.ID)
	// 呼び出し元はエージェントがキャンセルに従う前に待機をやめ、メモリ上の予約を解放する
	// ことがある。名前付きの context 原因が依然として正であり、止まった run を復旧可能な
	// 状態へ落ち着かせる。
	if action == "" {
		switch {
		case errors.Is(workCause, agent.AbortWorkPausedByUser):
			action = "pause"
		case errors.Is(workCause, agent.AbortWorkCancelledByUser):
			action = "cancel"
		}
	}
	var controlErr error
	defer func() { completeWork(controlErr) }()
	if action == "pause" {
		controlErr = transitionIntentState(t.Store, intent.ID, "running", "paused")
		if controlErr != nil {
			log.Printf("[worker %s] task %s 意図 #%d の一時停止状態を DB に保存できず: %v", name, t.ID, intent.ID, controlErr)
			return true
		}
		log.Printf("[worker %s] task %s 意図 #%d を一時停止", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	if action == "cancel" {
		// クリーンアップを API に渡す前に、止まった run を paused に駐車する。キャンセル後に
		// リクエストが切断されても、意図は復旧可能なままで、後の cancel がクリーンアップを
		// 完了できる。幽霊のような running 行を残さないため。
		controlErr = transitionIntentState(t.Store, intent.ID, "running", "paused")
		if controlErr != nil {
			log.Printf("[worker %s] task %s 意図 #%d のキャンセルバリアを DB に保存できず: %v", name, t.ID, intent.ID, controlErr)
			return true
		}
		log.Printf("[worker %s] task %s 意図 #%d を停止、キャンセルのクリーンアップ待ち", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	// pause が実行中の run を中断させた場合、意図を frontier に戻して再開時に
	// 再取得できるようにする。worker はゼロからやり直すのではなく、転記から
	// 前回の LLM 会話を再開する。
	if ectx.Err() != nil && taskExecutionPaused(context.Cause(ectx)) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "open"); err != nil {
			log.Printf("[worker %s] task %s 意図 #%d のタスク一時停止による巻き戻しに失敗: %v", name, t.ID, intent.ID, err)
		}
		return true
	}
	// タスクタイムアウト締めくくりの強制フォールバック cancel（pause でも kill でもない）が
	// この run をキャンセルした → exhausted（締めくくり済み）に分類し、誤って blocked に
	// しない。このとき worker は通常すでに settlement 段階で結果を書き戻している。
	if ectx.Err() != nil && e.isSettling(t.ID) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "exhausted"); err != nil {
			log.Printf("[worker %s] task %s 意図 #%d のタイムアウト締めくくり状態を DB に保存できず: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 意図 #%d はタスクタイムアウトの締めくくりで終了(exhausted)、書き戻し %s", name, t.ID, intent.ID, wrote)
		e.touch(t.ID)
		return true
	}
	// タスクが完了と判定済み（通常経路で done）→ 上の cancelExec がこの run をキャンセルした。
	// 意図の結果はもう意味がないので stopped（blocked ではない）にする。完了済みタスクの
	// 意図状態を汚さないため。
	if ectx.Err() != nil && isTerminalStatus(e.m.TaskStatus(t.ID)) {
		if err := transitionIntentState(t.Store, intent.ID, "running", "stopped"); err != nil {
			log.Printf("[worker %s] task %s 意図 #%d の終状態停止を DB に保存できず: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 意図 #%d はタスク完了によりキャンセル(stopped)", name, t.ID, intent.ID)
		e.touch(t.ID)
		return true
	}
	// planner によって kill された：stopped にする（結果を書き戻さず、自動再取得もしない）。
	if killed {
		if err := transitionIntentState(t.Store, intent.ID, "running", "stopped"); err != nil {
			log.Printf("[worker %s] task %s 意図 #%d の planner 停止を DB に保存できず: %v", name, t.ID, intent.ID, err)
		}
		log.Printf("[worker %s] task %s 意図 #%d を終了(stopped)", name, t.ID, intent.ID)
		e.touch(t.ID)
		t.Notify()
		return true
	}
	if err != nil {
		log.Printf("[worker %s] intent %d: %v", name, intent.ID, err)
	}
	// terminal の振り分け：ステップ数上限での打ち切り ≠ 完了。max_turns→exhausted
	// （planner はこれでこの方向を試したが本当にはやり切れていないと分かり、角度を変える
	// 必要がある。カバー済みとして永久にスキップしてはならない）。エラー→blocked、正常→done。
	state := "done"
	switch {
	case err != nil:
		state = "blocked"
	case reason == harness.ReasonMaxTurns:
		state = "exhausted"
		log.Printf("[worker %s] intent %d がステップ数上限に到達(exhausted)、今回の書き戻し %s", name, intent.ID, wrote)
	case reason == harness.ReasonTimeout:
		state = "exhausted"
		log.Printf("[worker %s] intent %d が実行タイムアウト(exhausted)、締めくくり後に書き戻し %s", name, intent.ID, wrote)
	}
	if state == "blocked" && isTaskLLMChainExhausted(err) {
		_ = t.Store.SetIntentBlockedReason(intent.ID, db.IntentBlockedLLMQuota)
	} else {
		if stateErr := transitionIntentState(t.Store, intent.ID, "running", state); stateErr != nil {
			log.Printf("[worker %s] task %s 意図 #%d の終状態 %s を DB に保存できず: %v", name, t.ID, intent.ID, state, stateErr)
		}
	}
	log.Printf("[worker %s] task %s 意図 #%d 終了: %s (書き戻し %s)", name, t.ID, intent.ID, state, wrote)
	e.touch(t.ID)
	t.NotifyDone(intent.ID) // results changed the graph -> wake the planner (with the just-finished intent id)
	return true
}

// runDetachedIntent は一時停止中の意図を worker プールの外、専用 goroutine で実行する
// ——人手メッセージの経路。意図を自分で paused→running へ遷移させ（'open' は経由しない）、
// 'open' しか取得しないプールと競合しないようにする。「1 意図につき最大 1 実行」の
// 不変条件は、CAS に勝つことが唯一の入口であり、pause の確定時に work[intentID] が
// クリアされていることから依然として成り立つ。frontier の枠を競合しないので、プールの
// 枠がすべて埋まっていてもユーザーのメッセージは worker を即座に再開させる（主エージェントの
// チャットハンドラが実行を直接開始するのと同じ形）。生成された goroutine は実行全体で
// タスク操作のアドミッションを 1 つ所有し、context の根を ctx に置く（サーバーのルートを
// 渡し、HTTP リクエストは決して渡さない。切断が run を宙吊りにせず、タスクの pause/delete/
// シャットダウンは依然として止められるようにする）。開始できなかった場合はエラーを返し、
// その場合意図はそのまま残る。
func (e *Engine) runDetachedIntent(ctx context.Context, t *Task, intentID int64, requestID, message, agentMessage string) error {
	if !e.beginTaskOperation(t.ID) {
		return fmt.Errorf("task is being deleted")
	}
	release := true
	defer func() {
		if release {
			e.decInflight(t.ID)
		}
	}()
	_, worker := e.snapshotFor(t)
	if worker == nil {
		return fmt.Errorf("worker がまだ準備できていない")
	}
	node, err := t.Store.GetNode(intentID)
	if err != nil {
		return err
	}
	if node == nil || node.Kind != db.KindIntent {
		return fmt.Errorf("intent not found")
	}
	changed, err := t.Store.CompareAndSetIntentState(intentID, "paused", "running")
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("%w: 意図はもはや paused 状態ではない", db.ErrIntentStateConflict)
	}
	node.State, node.Owner = "running", "chat"
	// 人間のターンを run 開始前に可視 activity として記録し、あらゆる worker ステップより
	// 前に並び、run が起きていないのに現れることがないようにする。UI の文言は簡潔に保つ。
	// ExecuteWithMessage がサーバー側で解決した参照スナップショットを意図の転記に LLM の
	// 入力として書き込む。
	uid := intentID
	e.emitActivity(t, db.Activity{NodeID: &uid, Worker: "user", Kind: "user", Summary: message, Detail: message})
	release = false // ownership of the admission passes to the goroutine
	go func() {
		defer e.decInflight(t.ID)
		e.runIntent(ctx, t, "chat", worker, node, requestID, agentMessage)
	}()
	return nil
}

func taskExecutionPaused(cause error) bool {
	var abort *agent.AbortCause
	if !errors.As(cause, &abort) {
		return false
	}
	switch abort.Code {
	case "paused_by_user", "paused_by_orchestrator", "paused_on_reload", "paused_race_guard",
		"queued_for_admission", "llm_unavailable_queued", "task_deleted":
		return true
	default:
		return false
	}
}

func sleepCtx(ctx context.Context, d time.Duration) (done bool) {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

func (e *Engine) claimNext(t *Task, name string) *db.Node {
	fr, _ := t.Store.Frontier(20)
	for _, in := range fr {
		if ok, _ := t.Store.ClaimIntent(in.ID, name); ok {
			return in
		}
	}
	return nil
}
