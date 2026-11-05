package server

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
)

// jaLocalizeDefaults は、中国語版（0.3.15）が DB へ初回 seed した既定値と、それに対応する
// 日本語版の既定値の対応表。各 seed は first-insert only（SeedTool / SeedPromptIfEmpty /
// 設定フラグでガードされたプリセット）なので、コードを日本語化しても既存 DB の行は中国語の
// まま残り、モデルはその中国語のプロンプト・ツール説明を読んで検出結果やレポートを中国語で
// 書いてしまう。本ファイルは新旧 2 つのバイナリでそれぞれ空 DB を初期化して採取したもの。
//
//go:embed ja_localize_defaults.json
var jaLocalizeDefaults []byte

type jaPair[T any] struct {
	Old T `json:"old"`
	New T `json:"new"`
}

type jaToolDefault struct {
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

type jaRuleDefault struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

type jaAgentDefault struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type jaDefaults struct {
	Tools               map[string]jaPair[jaToolDefault]  `json:"tools"`
	InterceptRules      map[string]jaPair[jaRuleDefault]  `json:"intercept_rules"` // key = 組み込みルールの id
	AssetInterceptRules map[string]jaPair[string]         `json:"asset_intercept_rules"`
	Agents              map[string]jaPair[jaAgentDefault] `json:"agents"`
	Prompts             map[string]jaPair[string]         `json:"prompts"`
	Triggers            map[string]jaPair[string]         `json:"triggers"`
}

// localizeSeededDefaults は、既存 DB に残る中国語の既定値を日本語の既定値へ置き換える。
// 置き換えるのは中国語版の既定値と【逐字一致する】行・フィールドだけで、ユーザーが編集した
// ものには触れない（他の移行と同じ方針）。プロンプトは上書きせず新バージョンとして追加するので、
// 履歴から元に戻せる。一度だけ実行し、全件が成功したときだけフラグを立てる。
func (s *Server) localizeSeededDefaults() {
	const flag = "ja_localized_defaults_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	n, err := s.applyJALocalization()
	if err != nil {
		log.Printf("[i18n] 既定値の日本語化に失敗しました: %v", err)
		return
	}
	if err := s.m.pg.SetSetting(flag, "true"); err != nil {
		log.Printf("[i18n] フラグの保存に失敗しました: %v", err)
		return
	}
	if n > 0 {
		log.Printf("[i18n] 中国語のまま残っていた既定値 %d 件を日本語の既定値へ更新しました", n)
	}
}

func (s *Server) applyJALocalization() (int, error) {
	var d jaDefaults
	if err := json.Unmarshal(jaLocalizeDefaults, &d); err != nil {
		return 0, fmt.Errorf("対応表の解析: %w", err)
	}
	tx, err := s.m.pg.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	total := 0
	exec := func(q string, args ...any) error {
		res, err := tx.Exec(q, args...)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		total += int(n)
		return nil
	}

	for key, p := range d.Tools {
		if err := exec(`UPDATE tools SET description=$3, updated_at=now() WHERE key=$1 AND system AND description=$2`,
			key, p.Old.Description, p.New.Description); err != nil {
			return 0, fmt.Errorf("tool %s: %w", key, err)
		}
		if err := exec(`UPDATE tools SET schema=$3::jsonb, updated_at=now() WHERE key=$1 AND system AND schema=$2::jsonb`,
			key, string(p.Old.Schema), string(p.New.Schema)); err != nil {
			return 0, fmt.Errorf("tool %s schema: %w", key, err)
		}
	}
	for id, p := range d.InterceptRules {
		if err := exec(`UPDATE intercept_rules SET name=$3 WHERE id=$1::bigint AND name=$2`, id, p.Old.Name, p.New.Name); err != nil {
			return 0, fmt.Errorf("intercept rule %s: %w", id, err)
		}
		if err := exec(`UPDATE intercept_rules SET message=$3 WHERE id=$1::bigint AND message=$2`, id, p.Old.Message, p.New.Message); err != nil {
			return 0, fmt.Errorf("intercept rule %s message: %w", id, err)
		}
	}
	for pattern, p := range d.AssetInterceptRules {
		if err := exec(`UPDATE asset_intercept_rules SET note=$3 WHERE pattern=$1 AND builtin AND note=$2`, pattern, p.Old, p.New); err != nil {
			return 0, fmt.Errorf("asset intercept rule %s: %w", pattern, err)
		}
	}
	for key, p := range d.Agents {
		if err := exec(`UPDATE agents SET name=$3 WHERE key=$1 AND name=$2`, key, p.Old.Name, p.New.Name); err != nil {
			return 0, fmt.Errorf("agent %s: %w", key, err)
		}
		if err := exec(`UPDATE agents SET description=$3 WHERE key=$1 AND description=$2`, key, p.Old.Description, p.New.Description); err != nil {
			return 0, fmt.Errorf("agent %s description: %w", key, err)
		}
	}
	for key, p := range d.Triggers {
		if err := exec(`UPDATE agent_triggers SET tool_call_message=$3 WHERE agent_key=$1 AND tool_call_message=$2`, key, p.Old, p.New); err != nil {
			return 0, fmt.Errorf("trigger %s: %w", key, err)
		}
	}
	for key, p := range d.Prompts {
		// 現在のバージョンが中国語の既定値のままのときだけ、日本語の既定値を新バージョンとして追加する。
		var agentID int64
		err := tx.QueryRow(`SELECT a.id FROM agents a JOIN agent_prompts p ON p.id=a.current_prompt_id
WHERE a.key=$1 AND p.template_text=$2`, key, p.Old).Scan(&agentID)
		if err != nil {
			continue // 該当なし（編集済み・agent なし）
		}
		var pid int64
		if err := tx.QueryRow(`INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by)
SELECT $1, COALESCE(max(version),0)+1, $2, '組み込み既定', 'system' FROM agent_prompts WHERE agent_id=$1
RETURNING id`, agentID, p.New).Scan(&pid); err != nil {
			return 0, fmt.Errorf("prompt %s: %w", key, err)
		}
		if err := exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, pid, agentID); err != nil {
			return 0, fmt.Errorf("prompt %s: %w", key, err)
		}
	}
	return total, tx.Commit()
}
