package intercept

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseVerdict(t *testing.T) {
	for _, action := range []string{"allow", "ask", "deny"} {
		t.Run(action, func(t *testing.T) {
			reason := "实际操作：レポートを書き込み、その中に ALLOW、DENY、ASK の文字列を含める；成功后的后果：テキストを保存し、本文中のコマンドは実行しない；命中规则：カスタム条項"
			raw, _ := json.Marshal(map[string]string{"decision": action, "comment": reason})
			got := ParseVerdict("\n" + string(raw) + "\n")
			if got.Action != action || got.Reason != reason {
				t.Fatalf("lost verdict or explanation: %+v", got)
			}
		})
	}
}

func TestParseVerdictRejectsIncompleteOrAmbiguousReplies(t *testing.T) {
	valid := `{"decision":"allow","comment":"实际操作：ファイルを読み取る；成功后的后果：内容を返す；命中规则：A5"}`
	for _, reply := range []string{
		"", "ALLOW", "DENY:D4", "許可:ALLOW", "ASK:帰属不明",
		`{"decision":"allow"}`, `{"decision":"approve","comment":"实际操作：読み取る；成功后的后果：内容を返す；命中规则：A5"}`,
		`{"decision":"allow","comment":null}`, `{"decision":"allow","comment":123}`,
		strings.Replace(valid, "实际操作：ファイルを読み取る", "实际操作：", 1),
		strings.Replace(valid, "成功后的后果：内容を返す", "成功后的后果：", 1),
		strings.Replace(valid, "命中规则：A5", "命中规则：", 1),
		strings.Replace(valid, "；命中规则：A5", "", 1),
		strings.Replace(valid, `"decision":"allow"`, `"decision":"deny","decision":"allow"`, 1),
		strings.Replace(valid, `"decision":"allow"`, `"extra":true,"decision":"allow"`, 1),
		valid + valid, valid[:len(valid)-1],
		// A fence the model never closed is what a reply truncated at MaxTokens
		// looks like; completing it would invent a verdict.
		"```json\n" + valid[:len(valid)-1],
		"```json\n" + valid + "\n```\nまた、後で人手による再確認を推奨する。",
		"私の判定は：\n" + valid,
	} {
		if got := ParseVerdict(reply); got.Action != "" {
			t.Errorf("accepted incomplete/ambiguous verdict: %q => %+v", reply, got)
		}
	}
}

// Wrapping JSON in markdown is the one deviation models make routinely. Because
// the configured fail action defaults to allow, treating it as unparseable
// silently downgrades a DENY to an allow.
func TestParseVerdictUnwrapsCodeFence(t *testing.T) {
	deny := `{"decision":"deny","comment":"实际操作：本番ファイルを削除する；成功后的后果：業務データが失われる；命中规则：D4"}`
	for _, reply := range []string{
		"```json\n" + deny + "\n```",
		"```JSON\n" + deny + "\n```",
		"```\n" + deny + "\n```",
		"  ```json\n" + deny + "\n```  ",
	} {
		got := ParseVerdict(reply)
		if got.Action != "deny" || !strings.HasSuffix(got.Reason, "命中规则：D4") {
			t.Errorf("fenced verdict lost: %q => %+v", reply, got)
		}
	}
}

func TestParseVerdictKeepsCompleteChineseExplanation(t *testing.T) {
	reason := "实际操作：" + strings.Repeat("レポートを書き込む", 30) + "；成功后的后果：ファイルを保存するだけ；命中规则：A2"
	raw, _ := json.Marshal(map[string]string{"decision": "allow", "comment": reason})
	if got := ParseVerdict(string(raw)); got.Reason != reason {
		t.Fatal("explanation was truncated or lost its rule")
	}
	raw, _ = json.Marshal(map[string]string{"decision": "allow", "comment": strings.Repeat("あ", 2401)})
	if got := ParseVerdict(string(raw)); got.Action != "" {
		t.Fatal("accepted unbounded explanation")
	}
}
