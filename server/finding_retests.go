package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	actool "github.com/Autumn-27/norma/tool"
)

func (s *Server) listActiveFindingRetests(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	items, err := pg.ListActiveFindingRetests(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"retests": items})
}

func (s *Server) listFindingRetests(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	f, err := pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	items, err := pg.ListFindingRetests(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"retests": items})
}

func (s *Server) startFindingRetest(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	var req struct {
		Notes string `json:"notes"`
	}
	if !decodeConversationRequest(w, r, &req) {
		return
	}
	req.Notes = strings.TrimSpace(req.Notes)
	if utf8.RuneCountInString(req.Notes) > 4000 {
		writeErr(w, 400, "再検証の補足説明は最大 4000 文字です")
		return
	}
	f, err := pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	a, err := pg.GetAgentByKey(db.FindingRetestAgentKey)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if a == nil || !a.Enabled {
		writeErr(w, 409, "脆弱性再検証 Agent が存在しないか有効化されていません。Agent 管理で retester を設定してください")
		return
	}
	for _, key := range []string{"get_finding_retest_context", "record_finding_retest_result"} {
		t, err := pg.GetTool(key)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if t == nil || !t.Enabled || !slices.Contains(t.Agents, a.Key) {
			writeErr(w, 409, "再検証 Agent に対してツールを有効化してバインドしてください："+key)
			return
		}
	}
	if s.resolveChatAgent(&db.Conversation{AgentKey: a.Key}) == nil {
		writeErr(w, 503, s.chatUnavailableReason())
		return
	}
	if s.ctx.Err() != nil {
		writeErr(w, 503, "サービスは停止処理中です")
		return
	}
	retest, conv, created, err := pg.CreateFindingRetest(r.Context(), id, req.Notes)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "finding not found")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if created {
		busyKey := s.convBusyKey(conv.ID)
		s.chatMu.Lock()
		s.chatBusy[busyKey] = true
		s.chatMu.Unlock()
		s.runConversation(conv, retest.InitialMessage(), busyKey)
	}
	code := http.StatusOK
	if created {
		code = http.StatusAccepted
	}
	writeJSON(w, code, map[string]any{"retest": retest, "created": created})
}

// Both tools derive the finding from server-owned conversation context. Tool
// arguments cannot redirect a result into a different finding or conversation.
func (s *Server) findingRetestTools() []actool.CoreTool {
	return []actool.CoreTool{
		roTool("get_finding_retest_context", "現在の再検証セッションに関連付けられた脆弱性エビデンスのスナップショット、再検証状態、補足説明、現在のタスク制約を読み取る。引数なし。このセッションのみ読み取り可能。",
			objSchema(map[string]any{}), func(ctx context.Context, _ json.RawMessage) (actool.Result, error) {
				r, err := s.m.pg.FindingRetestForConversation(ctx, intercept.ConvIDFromContext(ctx))
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if r == nil {
					return actool.Errorf("このセッションには再検証レコードが関連付けられていません。脆弱性の詳細から再検証を開始してください"), nil
				}
				var constraints []db.Constraint
				f, err := s.m.pg.GetFinding(r.FindingID)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if f != nil && f.TaskID != nil {
					if task, ok := s.m.Task(strconv.FormatInt(*f.TaskID, 10)); ok {
						constraints, err = task.Store.ListConstraints()
						if err != nil {
							return actool.Errorf(err.Error()), nil
						}
					}
				}
				return jsonResult(map[string]any{"retest": r, "current_constraints": constraints})
			}),
		wrTool("record_finding_retest_result", "現在の再検証セッションに唯一の結論を保存する。元の脆弱性エビデンスとレポートは変更されない。セッションが正常終了し、結論が fixed の場合は、システムが脆弱性の状態を自動的に修正済みへ変更する。それ以外の結論では元の状態を維持する。今回の実際の確認によるエビデンスを必ず提供し、確認できない場合はブロック理由を明記すること。",
			objSchema(map[string]any{
				"verdict":  map[string]any{"type": "string", "enum": []string{"reproduced", "fixed", "inconclusive"}},
				"summary":  strParam("今回の再検証結論の要約"),
				"evidence": strParam("Markdown：今回の実際の手順、観察、比較、結論の根拠。確認できない場合は確認済みの内容とブロック理由を列挙する"),
			}, "verdict", "summary", "evidence"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
				var a struct {
					Verdict  string `json:"verdict"`
					Summary  string `json:"summary"`
					Evidence string `json:"evidence"`
				}
				if err := json.Unmarshal(in, &a); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if err := s.m.pg.RecordFindingRetestResult(ctx, intercept.ConvIDFromContext(ctx), a.Verdict, a.Summary, a.Evidence); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				return jsonResult(map[string]any{"saved": true, "verdict": a.Verdict})
			}),
	}
}

// Seed the editable agent atomically, without an automatic discovery trigger.
// Once seeded, user edits/deletion survive restarts; a pre-existing key is kept.
func (s *Server) seedFindingRetester() error {
	for _, t := range s.findingRetestTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings, _ := json.Marshal([]string{db.FindingRetestAgentKey})
		if err := s.m.pg.SeedTool(t.Name(), t.Description(), schema, bindings); err != nil {
			return err
		}
	}
	const flag = "finding_retester_seed_v1"
	tx, err := s.m.pg.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Lock this migration, including concurrent server initialization.
	if _, err = tx.Exec(`SELECT pg_advisory_xact_lock(7337741010)`); err != nil {
		return err
	}
	var done string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key=$1`, flag).Scan(&done)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if done == "true" {
		return nil
	}
	var id int64
	err = tx.QueryRow(`INSERT INTO agents(key,name,description,role,builtin,enabled)
	VALUES ($1,'脆弱性再検証','脆弱性の詳細から手動で開始し、元のエビデンスを読み取って独立した再検証結論を保存する。','assistant',false,true)
	ON CONFLICT (key) DO NOTHING RETURNING id`, db.FindingRetestAgentKey).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if id > 0 {
		var pid int64
		if err = tx.QueryRow(`INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by)
		VALUES ($1,1,$2,'組み込み既定','system') RETURNING id`, id, agent.RetesterDefaultPrompt).Scan(&pid); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, pid, id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES ($1,'true') ON CONFLICT(key) DO UPDATE SET value='true'`, flag); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) finishRetest(id int64, status, reason string) {
	if err := s.m.pg.FinishFindingRetest(id, status, reason); err != nil {
		// Surface failure to the conversation runner rather than inventing a result.
		log.Printf("[retest %d] finish: %v", id, err)
	}
}
