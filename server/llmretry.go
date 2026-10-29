package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// リトライポリシーのサーバー側解決。docs/LLM重试设计.md を参照。5 つの層のうち：
//   - 接続 / 空レスポンス / 同一 provider の安全ウィンドウは「エンドポイントに従う」もので、
//     各 LLM 設定がグローバル既定を上書きできる（profile の項目が空ならグローバルを継承し、
//     グローバルも未設定なら組み込み既定を使う）。
//   - サーキットブレーカー / 意図の再実行はプロセス単位で、グローバルに 1 つだけ。
//
// グローバルポリシーは DB の settings 1 行を 1 回読むだけで、呼び出し箇所はすべて低頻度の
// 経路（provider 構築、work の後処理、設定保存）なので、さらにキャッシュ層を足す価値はない。
// サーキットブレーカーのパラメータは例外で、失敗経路で毎回読むため
// applyRetryPolicy が Registry に押し込んで保持させる。

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// 回数はここでは「0=既定 / 負=無効」という元の意味のまま維持する。SDK の MaxRetries /
		// EmptyResponseRetries と完全に同型なので、解析は SDK 自身に任せる。
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// サーキットブレーカー（ポーリング冷却）の既定値は llmpool の組み込み値と一致させている。
// ここでは「ユーザーが値を設定した」場合にだけ上書きする。
// 意図の再実行の既定値は engine.go の modelErrorRetries / modelErrorRetryBackoff を参照。

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy は意図レベルの再実行ノブ（層 ⑤）を解決する。model_error の
// work を何回再実行するか、実行間にどれだけバックオフするか。
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit は 1 つの work が注入できる空ターン継続の回数を解決する
// （steerHooks.Stop を参照）。これは層 ② のノブ「空レスポンスのリトライ回数」を
// 意図的に再利用している。両者は同じことの 2 つの手段である。SDK の層は「コンテンツ
// ブロックが 1 つもない」を扱い、手段は同じリクエストをそのまま再送すること。ここでは
// 「思考だけがあり本文もツールもない」を扱い、手段は指示を 1 つ追加して既存の思考を
// 持ったまま続行させること（コンテキストの形で決まる空転にはそのまま再送しても意味がない）。
// 空の判定基準が異なるのは、SDK が「イベントを yield したかどうか」を基準にする一方、
// 思考の差分自体がイベントになるため。ただしユーザーが「空レスポンスを何回リトライするか」
// を設定するときの意図は「モデルが実質的な出力をしなければもう一度」であり、
// 2 つの層が 1 つの回数を共有してこそそのメンタルモデルに合う。
//
// 特定 profile の上書きではなくグローバルポリシーを読む。1 回の run の途中で
// フェイルオーバーにより profile が変わり得るが、これは意図全体の総量リミッターであり、
// エンドポイントの切り替えに追随して変わってはならない。
// 意味は SDK の emptyRetries() と同型：0 = 既定の defaultEmptyTurnNudges、
// -1（負）= 空転時の継続を無効、>0 = その値を使用。
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
