package server

import (
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/agent"
)

// 対応表の new 側はコード既定値のスナップショットなので、コード側の既定値を変えたのに
// 対応表を更新し忘れると、旧 DB だけ古い日本語文言へ移行されてしまう。主要な既定値が
// コードと一致していることをここで固定する。
func TestJALocalizeDefaultsMatchCodeDefaults(t *testing.T) {
	var d jaDefaults
	if err := json.Unmarshal(jaLocalizeDefaults, &d); err != nil {
		t.Fatalf("対応表を解析できない: %v", err)
	}

	prompts := agent.BuiltinPromptSeeds()
	prompts["reporter"] = agent.ReporterDefaultPrompt
	prompts["retester"] = agent.RetesterDefaultPrompt
	for key, want := range prompts {
		p, ok := d.Prompts[key]
		if !ok {
			t.Errorf("prompt %s が対応表にない", key)
			continue
		}
		if p.New != want {
			t.Errorf("prompt %s の new がコード既定値と一致しない", key)
		}
		if p.Old == p.New {
			t.Errorf("prompt %s の old と new が同一", key)
		}
	}

	for _, s := range agent.BuiltinToolSeeds() {
		p, ok := d.Tools[s.Key]
		if !ok {
			t.Errorf("tool %s が対応表にない", s.Key)
			continue
		}
		if p.New.Description != s.Desc {
			t.Errorf("tool %s の new description がコード既定値と一致しない", s.Key)
		}
	}

	if got := d.Triggers["reporter"].New; got != reporterToolCallMessage {
		t.Errorf("reporter トリガーの new がコード既定値と一致しない: %q", got)
	}
	for key, p := range d.Agents {
		if p.Old.Name == p.New.Name {
			t.Errorf("agent %s の名前が日本語化されていない", key)
		}
	}
}
