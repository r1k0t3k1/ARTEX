package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// 総覧「ゴール管理」の手動 CRUD インターフェース。agent 側の set_goals ツールと
// 同じ goal ノード群を書き込むが、入口は人間が UI 上で直接追加・変更・削除する。
// 追加/変更後に「タスク復活」ロジックを再利用し（admitTask resume：終了状態→running、
// 一時停止解除、必要ならキュー投入）、削除では復活させない（プロダクト判断）。
// 各変更 handler は beginTaskOperation/decInflight を通し、タスク削除との競合を避ける
// （意図の CRUD と同様）。

// listGoals はこのタスクの全ゴールを（text/vulnclass/state を分解して）返し、
// ゴール管理カードの描画に使う。
func (s *Server) listGoals(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	goals, err := t.Store.ListByKind(db.KindGoal, 10000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"goals": goalDTOs(goals)})
}

// addGoal は手動でゴールを 1 件追加する。DB に保存し（タスクルートの spawns 下に連結）→
// 「ゴールを追加した」トリガーを記録してプランナーを起こす → タスクを復活させ、
// プランナーが新ゴールに基づいて達成可否を再判定できるようにする。
func (s *Server) addGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "タスクを削除中のため、ゴールを追加できません")
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "ゴールの内容を入力してください")
		return
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(body.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	id, err := t.Store.AddGoal(payload, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if of, _ := t.Store.OriginFactID(); of > 0 && id > 0 {
		_ = t.Store.Link(of, db.RelSpawns, id) // goal descends from the task root (origin fact)
	}
	t.NotifyGoal([]string{text}) // 「人がゴールを追加した」トリガーを記録し、プランナーを起こす
	s.reviveTask(t)              // 完了/一時停止中のタスクを実行状態に戻して続行させる
	node, _ := t.Store.GetNode(id)
	if node == nil {
		writeErr(w, 500, "ゴール書き込み後の読み取りに失敗しました")
		return
	}
	writeJSON(w, 200, goalDTO(node))
}

// editGoal は手動でゴールのテキスト（および vulnclass）を変更する。DB を更新 →
// 「ユーザーがゴールを old から new に変更した」トリガーを記録してプランナーを起こす →
// タスクを復活させ、プランナーが新ゴールに合わせて方向性を調整できるようにする。
func (s *Server) editGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "タスクを削除中のため、ゴールを変更できません")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "ゴールの内容を入力してください")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "ゴールが存在しません")
		return
	}
	oldText := goalDTO(node).Text
	if err := t.Store.UpdateGoalPayload(gid, text, strings.TrimSpace(body.VulnClass)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalEdited(oldText, text) // 「人がゴールを old から new に変更した」トリガーを記録し、プランナーを起こす
	s.reviveTask(t)                   // 追加時と同様：タスクを復活させ新ゴールで再判定する
	updated, _ := t.Store.GetNode(gid)
	if updated == nil {
		writeErr(w, 500, "ゴール更新後の読み取りに失敗しました")
		return
	}
	writeJSON(w, 200, goalDTO(updated))
}

// deleteGoal は手動でゴールを 1 件削除する（ハード削除、辺/アンカーを連鎖削除）。
// DB から削除 → 「ユーザーがゴール X を削除した」トリガーを記録してプランナーを起こし、
// 残りのゴールを再判定させる。プロダクト判断により、削除ではタスクを復活【させない】。
func (s *Server) deleteGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "タスクを削除中のため、ゴールを削除できません")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "ゴールが存在しません")
		return
	}
	text := goalDTO(node).Text
	if err := t.Store.DeleteGoal(gid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalDeleted(text) // 「人がそのゴールを削除した」トリガーを記録し、プランナーを起こす（タスクは復活させない）
	writeJSON(w, 200, map[string]bool{"ok": true})
}
