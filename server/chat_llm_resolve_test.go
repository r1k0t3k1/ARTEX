package server

import (
	"context"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// TestChatUnavailableReasonDistinguishesStates は、運用者向けメッセージが実際の
// 設定状態に対応していることを固定する：未設定の場合と、設定済みだが有効化されていない
// 場合は異なる文言になり、ユーザーが何をすべきか分かるようにする。
func TestChatUnavailableReasonDistinguishesStates(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)

	// クリーンな profile テーブルから開始する。共有 DB を使う他のテストが行を残している場合がある。
	existing, _ := m.pg.ListProfiles()
	for _, p := range existing {
		if p.IsDefault {
			// The active profile cannot be deleted directly; clear the flag first.
			_, _ = m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, p.ID)
		}
		_ = m.pg.DeleteProfile(p.ID)
	}

	if reason := s.chatUnavailableReason(); !strings.Contains(reason, "LLM が未設定です") {
		t.Fatalf("no-profile reason=%q, want LLM が未設定です", reason)
	}

	id, err := m.pg.SaveProfile(&db.LLMProfile{Name: "p1", Format: "anthropic", Model: "claude-x", APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, id)
		_ = m.pg.DeleteProfile(id)
	})

	// profile は存在するが有効化されていない。
	if reason := s.chatUnavailableReason(); !strings.Contains(reason, "アクティブな LLM 設定がありません") {
		t.Fatalf("inactive reason=%q, want アクティブな LLM 設定がありません", reason)
	}

	// 有効化後は、未設定/未アクティブを示す文言にならない。
	if err := m.pg.SetActiveProfile(id); err != nil {
		t.Fatal(err)
	}
	if reason := s.chatUnavailableReason(); strings.Contains(reason, "LLM が未設定です") || strings.Contains(reason, "アクティブな LLM 設定がありません") {
		t.Fatalf("active reason=%q should not report missing/inactive", reason)
	}
}

// TestResolveChatAgentHonoursConversationProfile は報告された不具合の回帰ガードである：
// 有効な profile を選択した会話は、グローバルに有効な profile がなくても chat agent を
// 解決できなければならず、送信時のプリチェックが「LLM 未設定」で拒否しなくなる。
func TestResolveChatAgentHonoursConversationProfile(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)

	// グローバルフォールバックを nil にして、非 nil の結果が会話自身の profile からしか
	// 得られないようにする。これはユーザーが遭遇した状況（利用可能なグローバル設定がない）
	// そのものであり、テスト環境から修正を切り離せる。
	s.cfgMu.Lock()
	s.chatAgent = nil
	s.cfgMu.Unlock()

	id, err := m.pg.SaveProfile(&db.LLMProfile{Name: "conv-pick", Format: "anthropic", Model: "claude-x", APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteProfile(id) })

	// この profile を pin した会話は、グローバルフォールバックが nil でも chat agent を
	// 解決する。修正前は送信プリチェックがこれをそのまま拒否していた。
	conv := &db.Conversation{AgentKey: "mainagent", LLMProfileID: &id}
	if s.resolveChatAgent(conv) == nil {
		t.Fatalf("resolveChatAgent(conversation with valid profile) = nil; global fallback wrongly gates the pick")
	}

	// 選択もグローバルフォールバックもない場合は、実行できるものが本当にない。
	if s.resolveChatAgent(&db.Conversation{AgentKey: "mainagent"}) != nil {
		t.Fatalf("resolveChatAgent with neither pick nor global config should be nil")
	}
}
