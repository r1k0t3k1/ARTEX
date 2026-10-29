package db

import (
	"encoding/json"
	"time"
)

// LLM リトライポリシー：五層リトライの「回数 + 間隔」をグローバル設定する。docs/LLM重试设计.md を参照。
// settings テーブルの 1 つの JSON 値として保存する——これはマシン全体で 1 つだけの実行パラメータであり、
// 専用テーブルを用意する価値はない。読み取りは組み込みデフォルトでフォールバックするため、
// キーが存在しない場合（新規 DB / 未設定）の挙動は定数をハードコードしていた頃と完全に同じ。

const settingLLMRetryPolicy = "llm_retry_policy"

// RetryRule is one layer's knob pair. The zero value means "unset":
//
//	Attempts   0 = 組み込みのデフォルト回数; -1 = その層のリトライを無効化; >0 = その値を使用
//	IntervalMS 0 = その層本来の間隔戦略（通常は指数バックオフ）; >0 = 固定ミリ秒間隔に変更
//
// -1 は「明示的にオフ」であり「0 回」ではない。0 は既に「未設定」に使われているため。
type RetryRule struct {
	Attempts   int `json:"attempts"`
	IntervalMS int `json:"interval_ms"`
}

// Interval returns the configured fixed interval, or 0 when unset (caller keeps
// its own default ladder).
func (r RetryRule) Interval() time.Duration {
	if r.IntervalMS <= 0 {
		return 0
	}
	return time.Duration(r.IntervalMS) * time.Millisecond
}

// Or returns the rule with each unset field filled in from fallback. Used to
// layer a profile override on top of the global policy field by field, so a
// profile that only pins the interval still inherits the global count.
func (r RetryRule) Or(fallback RetryRule) RetryRule {
	if r.Attempts == 0 {
		r.Attempts = fallback.Attempts
	}
	if r.IntervalMS == 0 {
		r.IntervalMS = fallback.IntervalMS
	}
	return r
}

// retry knob bounds. A count above the cap turns a blip into a token bonfire;
// an interval above an hour outlives any transient failure worth waiting out.
const (
	maxRetryAttempts   = 20
	maxRetryIntervalMS = 3600_000 // 1h
)

// Clamped returns the rule with out-of-range values pulled back into the sane
// band (attempts within [-1, 20], interval within [0, 1h]).
func (r RetryRule) Clamped() RetryRule {
	if r.Attempts < -1 {
		r.Attempts = -1
	}
	if r.Attempts > maxRetryAttempts {
		r.Attempts = maxRetryAttempts
	}
	if r.IntervalMS < 0 {
		r.IntervalMS = 0
	}
	if r.IntervalMS > maxRetryIntervalMS {
		r.IntervalMS = maxRetryIntervalMS
	}
	return r
}

// Clamped bounds a profile's override the same way the global policy is bounded,
// so a hand-crafted API payload can't land a value the CHECK constraint rejects.
func (o RetryOverride) Clamped() RetryOverride {
	o.Connect, o.Empty, o.Stream = o.Connect.Clamped(), o.Empty.Clamped(), o.Stream.Clamped()
	return o
}

// LLMRetryPolicy holds the five-layer retry configuration. Connect/Empty/Stream are the
// per-request layers (a profile may override them, see LLMProfile.Retry);
// Breaker and Intent are process-wide by nature and live only here.
type LLMRetryPolicy struct {
	// Connect：SDK の接続リトライ（接続リセット/タイムアウト/429/5xx、ストリーム開始前）。デフォルト 3 回、指数バックオフ。
	Connect RetryRule `json:"connect"`
	// Empty：SDK の空応答リトライ（完了したが content block が無い、openai 形式のみ）。デフォルト 2 回、指数バックオフ。
	Empty RetryRule `json:"empty"`
	// Stream：同一 provider の安全ウィンドウリトライ（出力未配信時の断流リプレイ）。デフォルト 2 回、0.5s から指数（上限 4s）。
	Stream RetryRule `json:"stream"`
	// Breaker：ポーリングのサーキットブレーカ。Attempts=連続何回の一時的失敗でブレーカを開くか（デフォルト 3、-1=一時的失敗では開かない、
	// ハード失敗（残高不足/キー失効）は依然として即座にブレーカを開く）；IntervalMS=固定クールダウン時間（0=デフォルト 1/5/30min の段階）。
	Breaker RetryRule `json:"breaker"`
	// Intent：worker が model_error で終わった後の意図全体の再実行。デフォルト 2 回、固定 3s。
	Intent RetryRule `json:"intent"`
}

// Clamped returns the policy with every rule clamped.
func (p LLMRetryPolicy) Clamped() LLMRetryPolicy {
	p.Connect, p.Empty, p.Stream = p.Connect.Clamped(), p.Empty.Clamped(), p.Stream.Clamped()
	p.Breaker, p.Intent = p.Breaker.Clamped(), p.Intent.Clamped()
	return p
}

// LLMRetryPolicy reads the global retry policy. A missing or unparseable value
// yields the zero policy — i.e. every layer on its built-in default.
func (d *DB) LLMRetryPolicy() LLMRetryPolicy {
	var p LLMRetryPolicy
	if d == nil {
		return p
	}
	raw, ok, err := d.GetSetting(settingLLMRetryPolicy)
	if err != nil || !ok || raw == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return LLMRetryPolicy{}
	}
	return p.Clamped()
}

// SetLLMRetryPolicy persists the global retry policy (values are clamped first).
func (d *DB) SetLLMRetryPolicy(p LLMRetryPolicy) error {
	raw, err := json.Marshal(p.Clamped())
	if err != nil {
		return err
	}
	return d.SetSetting(settingLLMRetryPolicy, string(raw))
}
