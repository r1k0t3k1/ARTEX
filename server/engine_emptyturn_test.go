package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// 空転ターン（思考のみで、本文もツールもない）の識別と継続実行については steerHooks.Stop を参照。

func assistantThinking(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: text, Signature: "sig"},
	}}
}

func TestIsThinkingOnlyTurn(t *testing.T) {
	toolUse := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "まずポートをスキャンする"},
		{Type: llm.BlockToolUse, ID: "t1", Name: "run_nuclei"},
	}}
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"思考のみ", []llm.Message{llm.UserText("開始"), assistantThinking("考える")}, true},
		{"思考+ツール", []llm.Message{llm.UserText("開始"), toolUse}, false},
		{"思考+本文", []llm.Message{assistantThinking("考える"), {
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("結論")},
		}}, false},
		{"本文が空白文字だけ", []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("  \n ")},
		}}, true},
		{"完全に空の assistant ターン", []llm.Message{{Role: llm.RoleAssistant}}, true},
		// ツール結果は user ロールなので、判定は必ずその手前の assistant まで遡るべきで、
		// 直近のメッセージで誤判定してはならない。
		{"最後がツール結果", []llm.Message{toolUse, {
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "t1"}},
		}}, false},
		{"assistant メッセージなし", []llm.Message{llm.UserText("開始")}, false},
		{"履歴が空", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isThinkingOnlyTurn(c.msgs); got != c.want {
				t.Fatalf("isThinkingOnlyTurn = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeHooks はプログラム可能な inner HookRunner で、steerHooks が inner の決定を
// 尊重するかを検証するために使う。
type fakeHooks struct {
	prevent  bool
	blocking []string
	msg      string
}

func (f fakeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (f fakeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f fakeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return f.prevent, f.blocking, f.msg
}

func TestSteerHooksStopNudgesEmptyTurn(t *testing.T) {
	empty := []llm.Message{assistantThinking("まずサブドメインを列挙すべきだ")}

	t.Run("空転ターンに継続指示を注入", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1"}
		prevent, blocking, _ := h.Stop(context.Background(), empty)
		if prevent {
			t.Fatal("空転ターンでハードストップしてはならない")
		}
		if len(blocking) != 1 || blocking[0] != emptyTurnNudge {
			t.Fatalf("blocking = %v, want [emptyTurnNudge]", blocking)
		}
	})

	t.Run("本文かツールがあれば介入しない", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		normal := []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock("スキャン完了、開放ポートは見つからなかった")},
		}}
		if _, blocking, _ := h.Stop(context.Background(), normal); blocking != nil {
			t.Fatalf("正常な終了を空転と誤判定した: %v", blocking)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("介入しない場合はカウントしないはず, got %d", n)
		}
	})

	t.Run("上限に達したらそのまま終了させる", func(t *testing.T) {
		const limit = 5 // ユーザーが「空応答リトライ回数」を 5 に設定
		h := steerHooks{nudges: &atomic.Int64{}, limit: limit}
		for i := 1; i <= limit; i++ {
			if _, blocking, _ := h.Stop(context.Background(), empty); len(blocking) != 1 {
				t.Fatalf("%d 回目はまだ枠内のはず, blocking = %v", i, blocking)
			}
		}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("上限超過後もまだ注入している: %v", blocking)
		}
	})

	// 「空応答リトライ回数」が -1 = この層を無効化、emptyTurnNudgeLimit は 0 に解決される。
	t.Run("設定で無効化されているときは介入しない", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: 0}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("無効化済みなのに注入している: %v", blocking)
		}
	})

	t.Run("inner がハードストップを決めたら重ねない", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{prevent: true, msg: "guard が終了を拒否"}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		prevent, blocking, msg := h.Stop(context.Background(), empty)
		if !prevent || msg != "guard が終了を拒否" || blocking != nil {
			t.Fatalf("inner のハードストップが書き換えられた: prevent=%v blocking=%v msg=%q", prevent, blocking, msg)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("inner に譲るときはクォータを消費しないはず, got %d", n)
		}
	})

	t.Run("inner がすでに継続を求めているときは重ねない", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{blocking: []string{"guard の継続理由"}}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		_, blocking, _ := h.Stop(context.Background(), empty)
		if len(blocking) != 1 || blocking[0] != "guard の継続理由" {
			t.Fatalf("inner の継続メッセージが書き換えられた: %v", blocking)
		}
	})

	t.Run("カウンタ未設定でも挙動は変わらない", func(t *testing.T) {
		h := steerHooks{limit: defaultEmptyTurnNudges} // 将来の別の呼び出し点が nudges を渡し忘れた場合など
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("カウンタがないときは注入しないはず: %v", blocking)
		}
	})
}
