package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "モデルが今回の実行を正常に終了したが、テキストによるまとめを残さなかった。事実と資産は今回のツール呼び出し記録に基づく",
	harness.ReasonMaxTurns:          "ステップ数上限(MaxTurns)に到達：SDK は締めくくりを実行して事実と資産を書き戻した。意図は exhausted としてマークされ、失敗として扱わずプランナーが方向転換して続行できる",
	harness.ReasonTimeout:           "単一実行の実時間予算(MaxDuration)に到達：時間切れ時に実行中のツールを中断してその場で締めくくりに入り、識別済みの事実と資産を書き戻す。意図は exhausted としてマークされる",
	harness.ReasonModelError:        "モデルまたは API 呼び出しの失敗（ネットワーク、認証、レート制限、プロバイダの 5xx など）。リトライを使い切ると意図は blocked としてマークされる——伝送層の障害によりこの意図はほとんど実際には探索できていない。実行過程（get_worker_trace）を確認してから再派遣か手法変更を決める",
	harness.ReasonBlockingLimit:     "コンテキスト長がハード上限に達し、リクエストが送信前に遮断された。意図の粒度を狭くするかツールの返却を圧縮すべき",
	harness.ReasonPromptTooLong:     "プロンプトが長すぎ、コンテキスト圧縮のリトライも使い切ったため続行できない",
	harness.ReasonImageError:        "現在のモデルは今回のマルチモーダル内容に対応していない。ビジョン対応モデルに切り替えるか、ツールが画像を返さないようにする",
	harness.ReasonStopHookPrevented: "Stop フックが今回の実行の終了を妨げ、その後続行できなかった。タスクの Guard ルールが厳しすぎないか確認する",
	harness.ReasonHookStopped:       "ツールまたはフックが能動的に実行を停止した（範囲外のターゲットや禁止コマンドなど）。最後の tool_result の遮断説明を確認する",
	harness.ReasonAbortedStreaming:  "モデル出力のストリーミング生成段階で実行がキャンセルされた",
	harness.ReasonAbortedTools:      "ツール実行段階で実行がキャンセルされた",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "キャンセル理由を取得できなかった"
		}
		stage := "実行中"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "モデル出力段階"
		case harness.ReasonAbortedTools:
			stage = "ツール実行段階"
		}
		sum = "（実行が中断された：" + short + "；" + stage + "で停止" + progressSuffix(term, tr) + "、未完了）"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "（実行予算の上限(" + string(reason) + ")に達し、締めくくりとして事実を書き戻した" + progressSuffix(term, tr) + "；今回はテキストのまとめなし）"
	} else {
		hint := terminalReasonHint(reason)
		sum = "（テキストのまとめなし、終了状態 " + terminalReasonLabel(reason) + "：" + firstLine(hint, 80) + "）"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **終了状態**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **中断理由** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **中断理由**: 取得できず；キャンセル側が context.WithCancelCause で具名理由を付加していない可能性がある\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **下位層のエラー**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **キャンセル前に生成済みの部分出力**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **実行済み**: %d ターンのモデル呼び出し\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **今回の実行時間**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **累計 token**: 入力 %d / 出力 %d / キャッシュ読込 %d / キャッシュ書込 %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **ツール呼び出し**: 今回の実行はツール呼び出しを一度も発行せずに終了した\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **中断時に実行中だったツール**: `%s`（実行済み %s、**結果未返却**）\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **中断前の最後のツール**: `%s`（正常に返却済み）\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "実行中の context はキャンセルされたが、下位層で Terminal イベントが発生しなかった"
	}
	return "未知の終了状態；harness に TerminalReason が追加された可能性がある。reasonHint を追加すること"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d ターン", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "、実行済み " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
