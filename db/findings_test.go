package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDeleteFinding は、脆弱性の削除が findings の行と、その由来である探索ノード
// （kind='finding'）の両方を削除することを検証する。
func TestDeleteFinding(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	tk, err := d.CreateTask("脆弱性削除テスト", "目標", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)

	// タスクの探索グラフに finding ノードを 1 つ作り、その上に findings の行を 1 行作る
	es := d.Exploration(tk.ExplorationID)
	nodeID, err := es.AddNode(KindFinding, map[string]any{"summary": "x", "severity": "high"}, 5, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	fid, err := d.AddFinding(tk.ID, nodeID, "XSS", "反射型 XSS", "high", "summary", "poc", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}

	n, err := d.DeleteFinding(fid)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("DeleteFinding の行数: 期待 1、実際 %d", n)
	}
	if f, _ := d.GetFinding(fid); f != nil {
		t.Fatalf("finding の行は消えているべき: %+v", f)
	}
	var cnt int
	d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE id=$1`, nodeID).Scan(&cnt)
	if cnt != 0 {
		t.Fatalf("由来の finding ノードは削除されるべきが、まだ %d 件ある", cnt)
	}

	// 存在しない finding の削除は no-op（0 行）であり、エラーではない
	if n, err := d.DeleteFinding(fid); err != nil || n != 0 {
		t.Fatalf("再削除: 期待 (0,nil)、実際 (%d,%v)", n, err)
	}
}

// TestFindingsPageAndStats は ListFindingsPage（フィルタ/ソート/ページング）と
// FindingStats を実際の開発用 PG に対して実走する。行には一意な vulnclass を付けて
// 既存データから検証を隔離し、最後に後始末する。
func TestFindingsPageAndStats(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	const vc = "__test_vc_pagination__"
	// 以前の中断した実行の残骸を掃除し、終了時にも後始末する
	cleanup := func() { _, _ = d.Exec(`DELETE FROM findings WHERE vulnclass=$1`, vc) }
	cleanup()
	defer cleanup()

	// マーカー vulnclass の下に 6 件の finding を投入：critical 1、high 3、low 2、pending 2。
	// critical の行には名前を付け、ラウンドトリップを検証する。
	seed := []struct {
		sev, status, name string
	}{
		{"critical", "pending", "重大な脆弱性のタイトル"},
		{"high", "resolved", ""},
		{"high", "resolved", ""},
		{"high", "pending", ""},
		{"low", "resolved", ""},
		{"low", "resolved", ""},
	}
	var ids []int64
	for i, s := range seed {
		id, err := d.AddFinding(0, 0, vc, s.name, s.sev, "summary", "poc", "tester", nil)
		if err != nil {
			t.Fatalf("AddFinding[%d]: %v", i, err)
		}
		if _, err := d.SetFindingStatus(id, s.status); err != nil {
			t.Fatalf("SetFindingStatus[%d]: %v", i, err)
		}
		ids = append(ids, id)
	}

	// 自前の vulnclass で絞り込み → 投入した 6 行だけが対象、1 ページ 2 件でページング。
	p1, total, err := d.ListFindingsPage(FindingFilter{VulnClass: vc, Sort: "severity"}, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 6 {
		t.Fatalf("total: 期待 6、実際 %d", total)
	}
	if len(p1) != 2 {
		t.Fatalf("page1 の件数: 期待 2、実際 %d", len(p1))
	}
	// severity ソート → critical が先頭（名前もラウンドトリップ）、次に high。
	if p1[0].Severity != "critical" {
		t.Fatalf("severity ソート: critical が先頭のはずが %q", p1[0].Severity)
	}
	if p1[0].Name != "重大な脆弱性のタイトル" {
		t.Fatalf("名前のラウンドトリップ: 期待 重大な脆弱性のタイトル、実際 %q", p1[0].Name)
	}
	if p1[1].Severity != "high" {
		t.Fatalf("severity ソート: 2 番目は high のはずが %q", p1[1].Severity)
	}

	// 複合フィルタ：vulnclass + status=pending → 2 行。
	pend, total, err := d.ListFindingsPage(FindingFilter{VulnClass: vc, Status: FindingPending}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(pend) != 2 {
		t.Fatalf("pending フィルタ: 期待 2/2、実際 %d/%d", total, len(pend))
	}

	// 複合フィルタ：vulnclass + severity=high → 3 行。
	_, total, err = d.ListFindingsPage(FindingFilter{VulnClass: vc, Severity: "high"}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("high フィルタ: 期待 3、実際 %d", total)
	}

	// 統計はテーブル全体が対象なので、自分の寄与が反映されていること（>=）と、
	// マーカー vulnclass が存在することを検証する。
	st, err := d.FindingStats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Critical < 1 {
		t.Fatalf("統計の critical が過少: %+v", st)
	}
	if st.Total < 6 || st.High < 3 || st.Low < 2 || st.Pending < 2 {
		t.Fatalf("統計が過少: %+v", st)
	}
	if !slices.Contains(st.VulnClasses, vc) {
		t.Fatalf("統計の vulnclasses に %q がない", vc)
	}

	// GetFinding：1 行取得で id/name/severity がラウンドトリップする。
	one, err := d.GetFinding(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if one == nil || one.ID != ids[0] || one.Severity != "critical" || one.Name != "重大な脆弱性のタイトル" {
		t.Fatalf("GetFinding の不一致: %+v", one)
	}
	if one.Report != "" {
		t.Fatalf("新しい finding の report は空のはずが %q", one.Report)
	}
	// report 列が GetFinding を通してラウンドトリップする。
	if _, err := d.Exec(`UPDATE findings SET report=$1 WHERE id=$2`, "# レポート\n本文", ids[0]); err != nil {
		t.Fatal(err)
	}
	if one, _ = d.GetFinding(ids[0]); one.Report != "# レポート\n本文" {
		t.Fatalf("report が読み戻せない: %q", one.Report)
	}
	for _, test := range []struct {
		name  string
		query string
		want  int
	}{
		{name: "name", query: "重大な脆弱性のタイトル", want: 1},
		{name: "summary", query: "summary", want: 6},
		{name: "evidence", query: "poc", want: 6},
		{name: "report", query: "本文", want: 1},
		{name: "case insensitive vulnclass", query: strings.ToUpper(vc), want: 6},
	} {
		t.Run("query_"+test.name, func(t *testing.T) {
			matches, searchTotal, searchErr := d.ListFindingsPage(
				FindingFilter{VulnClass: vc, Query: test.query}, 1, 20,
			)
			if searchErr != nil || searchTotal != test.want || len(matches) != test.want {
				t.Fatalf("query %q: len=%d total=%d err=%v、期待 %d", test.query, len(matches), searchTotal, searchErr, test.want)
			}
		})
	}
	if _, err := d.Exec(`UPDATE findings SET name=$1 WHERE id=$2`, "literal %_ marker", ids[1]); err != nil {
		t.Fatal(err)
	}
	matches, searchTotal, err := d.ListFindingsPage(FindingFilter{VulnClass: vc, Query: "%_"}, 1, 20)
	if err != nil || searchTotal != 1 || len(matches) != 1 || matches[0].ID != ids[1] {
		t.Fatalf("query のワイルドカードはリテラル扱いのはず: matches=%+v total=%d err=%v", matches, searchTotal, err)
	}
	if miss, err := d.GetFinding(-1); err != nil || miss != nil {
		t.Fatalf("GetFinding(-1): 期待 nil,nil、実際 %+v,%v", miss, err)
	}

	// SetFindingSeverity：単独の行更新。未知の id では 0 行。
	if n, err := d.SetFindingSeverity(ids[0], "high"); err != nil || n != 1 {
		t.Fatalf("SetFindingSeverity: 期待 1,nil、実際 %d,%v", n, err)
	}
	one, _ = d.GetFinding(ids[0])
	if one.Severity != "high" {
		t.Fatalf("severity が更新されていない: %q", one.Severity)
	}
	if n, err := d.SetFindingSeverity(-1, "low"); err != nil || n != 0 {
		t.Fatalf("SetFindingSeverity(-1): 期待 0,nil、実際 %d,%v", n, err)
	}
}

func TestListFindingsPageUsesStableIDTieBreaker(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	vc := fmt.Sprintf("__test_finding_stable_page_%d__", time.Now().UnixNano())
	defer d.Exec(`DELETE FROM findings WHERE vulnclass=$1`, vc)
	ids := make([]int64, 0, 7)
	for i := 0; i < 7; i++ {
		id, addErr := d.AddFinding(0, 0, vc, "", SeverityHigh, fmt.Sprintf("finding %d", i), "", "test", nil)
		if addErr != nil {
			t.Fatalf("AddFinding[%d]: %v", i, addErr)
		}
		ids = append(ids, id)
	}
	sharedCreatedAt := time.Date(2026, time.August, 21, 8, 30, 0, 0, time.UTC)
	if _, err := d.Exec(`UPDATE findings SET created_at=$1 WHERE vulnclass=$2`, sharedCreatedAt, vc); err != nil {
		t.Fatal(err)
	}

	want := make([]int64, len(ids))
	for i := range ids {
		want[i] = ids[len(ids)-1-i]
	}
	for _, sort := range []string{"time", "severity"} {
		t.Run(sort, func(t *testing.T) {
			var got []int64
			for page := 1; page <= 3; page++ {
				items, total, pageErr := d.ListFindingsPage(FindingFilter{VulnClass: vc, Sort: sort}, page, 3)
				if pageErr != nil {
					t.Fatal(pageErr)
				}
				if total != len(ids) {
					t.Fatalf("page %d total=%d、期待 %d", page, total, len(ids))
				}
				for _, item := range items {
					got = append(got, item.ID)
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("同一タイムスタンプでのページングが不安定: 実際=%v 期待=%v", got, want)
			}
		})
	}
}

func TestFindingGroupsAndUnassignedPaging(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	vc := fmt.Sprintf("__test_finding_groups_%d__", time.Now().UnixNano())
	cleanupFindings := func() { _, _ = d.Exec(`DELETE FROM findings WHERE vulnclass=$1`, vc) }
	defer cleanupFindings()

	taskA, err := d.CreateTask("group A", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(taskA.ID)
	taskB, err := d.CreateTask("group B", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(taskB.ID)

	seed := []struct {
		taskID   int64
		severity string
		status   string
	}{
		{taskA.ID, SeverityCritical, FindingPending},
		{taskA.ID, SeverityHigh, FindingResolved},
		{taskB.ID, SeverityLow, FindingPending},
		{0, SeverityMedium, FindingPending},
	}
	for i, item := range seed {
		id, addErr := d.AddFinding(item.taskID, 0, vc, "", item.severity, fmt.Sprintf("summary %d", i), "", "test", nil)
		if addErr != nil {
			t.Fatalf("AddFinding[%d]: %v", i, addErr)
		}
		if _, setErr := d.SetFindingStatus(id, item.status); setErr != nil {
			t.Fatalf("SetFindingStatus[%d]: %v", i, setErr)
		}
	}
	matchedGroups, matchedGroupTotal, matchedFindingTotal, err := d.ListFindingGroups(
		FindingFilter{VulnClass: vc, Query: "summary 0"}, 1, 10,
	)
	if err != nil || matchedGroupTotal != 1 || matchedFindingTotal != 1 || len(matchedGroups) != 1 ||
		matchedGroups[0].TaskID == nil || *matchedGroups[0].TaskID != taskA.ID {
		t.Fatalf("query で絞ったグループ: %+v groups=%d findings=%d err=%v",
			matchedGroups, matchedGroupTotal, matchedFindingTotal, err)
	}
	// 削除済みタスクから保持された finding は、タスクなしで作られた finding と同じ
	// バケットに入る。
	if err := d.DeleteTask(taskB.ID); err != nil {
		t.Fatal(err)
	}

	page, groupTotal, findingTotal, err := d.ListFindingGroups(FindingFilter{VulnClass: vc, Sort: "severity"}, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || groupTotal != 2 || findingTotal != 4 {
		t.Fatalf("ページの集計: items=%d groups=%d findings=%d", len(page), groupTotal, findingTotal)
	}
	groups, _, _, err := d.ListFindingGroups(FindingFilter{VulnClass: vc}, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	var live, unassigned *FindingGroup
	for i := range groups {
		if groups[i].TaskID == nil {
			unassigned = &groups[i]
		} else if *groups[i].TaskID == taskA.ID {
			live = &groups[i]
		}
	}
	if live == nil || live.Count != 2 || live.Critical != 1 || live.High != 1 || live.TaskDescription != "group A" {
		t.Fatalf("稼働中グループの不一致: %+v", live)
	}
	if unassigned == nil || unassigned.Count != 2 || unassigned.Medium != 1 || unassigned.Low != 1 {
		t.Fatalf("未関連グループの不一致: %+v", unassigned)
	}
	groupForTask := func(items []FindingGroup, taskID int64) *FindingGroup {
		for i := range items {
			if items[i].TaskID != nil && *items[i].TaskID == taskID {
				return &items[i]
			}
		}
		return nil
	}
	if err := d.SetPaused(taskA.ID, true); err != nil {
		t.Fatal(err)
	}
	groups, _, _, err = d.ListFindingGroups(FindingFilter{VulnClass: vc}, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if group := groupForTask(groups, taskA.ID); group == nil || group.TaskStatus != "paused" {
		t.Fatalf("一時停止タスクのグループ状態が不一致: %+v", groups)
	}
	if err := d.SetPaused(taskA.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := d.Enqueue(taskA.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	groups, _, _, err = d.ListFindingGroups(FindingFilter{VulnClass: vc}, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if group := groupForTask(groups, taskA.ID); group == nil || group.TaskStatus != "queued" {
		t.Fatalf("キュー済みタスクのグループ状態が不一致: %+v", groups)
	}
	if err := d.SetStatus(taskA.ID, "done"); err != nil {
		t.Fatal(err)
	}
	groups, _, _, err = d.ListFindingGroups(FindingFilter{VulnClass: vc}, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if group := groupForTask(groups, taskA.ID); group == nil || group.TaskStatus != "done" {
		t.Fatalf("終端タスクの状態が queued フラグより優先されるべき: %+v", groups)
	}

	orphans, total, err := d.ListFindingsPage(FindingFilter{VulnClass: vc, TaskID: FindingUnassignedTask}, 1, 10)
	if err != nil || total != 2 || len(orphans) != 2 {
		t.Fatalf("未関連ページ: len=%d total=%d err=%v", len(orphans), total, err)
	}
	maxPage := int(^uint(0) >> 1)
	farFindings, farTotal, err := d.ListFindingsPage(FindingFilter{VulnClass: vc}, maxPage, 200)
	if err != nil || farTotal != len(seed) || len(farFindings) != 0 {
		t.Fatalf("範囲外の finding ページ: len=%d total=%d err=%v", len(farFindings), farTotal, err)
	}
	farGroups, farGroupTotal, farFindingTotal, err := d.ListFindingGroups(FindingFilter{VulnClass: vc}, maxPage, 100)
	if err != nil || farGroupTotal != 2 || farFindingTotal != len(seed) || len(farGroups) != 0 {
		t.Fatalf("範囲外のグループページ: len=%d groups=%d findings=%d err=%v",
			len(farGroups), farGroupTotal, farFindingTotal, err)
	}
	filtered, filteredGroups, filteredFindings, err := d.ListFindingGroups(
		FindingFilter{VulnClass: vc, Status: FindingResolved}, 1, 10,
	)
	if err != nil || filteredGroups != 1 || filteredFindings != 1 || len(filtered) != 1 || filtered[0].High != 1 {
		t.Fatalf("絞り込み後のグループ: %+v groups=%d findings=%d err=%v", filtered, filteredGroups, filteredFindings, err)
	}
}

func TestAddFindingFollowUpIntent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	task, err := d.CreateTask("finding follow-up", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	assetID, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{
		Domain: fmt.Sprintf("follow-up-%d.example.test", time.Now().UnixNano()),
		TaskID: task.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM assets WHERE id=$1`, assetID)

	store := d.Exploration(task.ExplorationID)
	findingNodeID, err := store.AddNode(KindFinding, map[string]any{"summary": "source"}, 5, "confirmed", "worker", []int64{assetID})
	if err != nil {
		t.Fatal(err)
	}
	findingID, err := d.AddFinding(task.ID, findingNodeID, "test", "source", SeverityHigh, "source", "", "worker", []int64{assetID})
	if err != nil {
		t.Fatal(err)
	}

	auditInput := Activity{Worker: "system", Kind: "text", Summary: "手動提出した脆弱性の深掘り意図", Detail: "悪用可能性を検証し証拠チェーンを形成する"}
	intentID, audit, err := store.AddFindingFollowUpIntent(findingID, findingNodeID, "悪用可能性を検証し証拠チェーンを形成する", auditInput)
	if err != nil {
		t.Fatal(err)
	}
	secondID, _, err := store.AddFindingFollowUpIntent(findingID, findingNodeID, "別の経路から深掘りする", Activity{
		Worker: "system", Kind: "text", Summary: "手動提出した脆弱性の深掘り意図", Detail: "別の経路から深掘りする",
	})
	if err != nil {
		t.Fatal(err)
	}
	if secondID == intentID {
		t.Fatal("同じフォローアップを繰り返し提出しても別々の意図が作られるべき")
	}
	expectedAuditSummary := fmt.Sprintf("%s #%d", auditInput.Summary, intentID)
	if audit.ID <= 0 || audit.NodeID == nil || *audit.NodeID != intentID || audit.CreatedAt.IsZero() || audit.Summary != expectedAuditSummary {
		t.Fatalf("永続化された監査ログが不一致: %+v", audit)
	}
	node, err := store.GetNode(intentID)
	if err != nil || node == nil {
		t.Fatalf("GetNode: node=%+v err=%v", node, err)
	}
	if node.Kind != KindIntent || node.Priority != 10 || node.State != "open" || node.Origin != "human" {
		t.Fatalf("フォローアップ意図のメタデータ: %+v", node)
	}
	var anchorCount, edgeCount int
	if err := d.QueryRow(`SELECT COUNT(*) FROM exploration_anchors WHERE node_id=$1 AND asset_id=$2`, intentID, assetID).Scan(&anchorCount); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT COUNT(*) FROM exploration_edges
		WHERE exploration_id=$1 AND src_id=$2 AND rel=$3 AND dst_id=$4`,
		task.ExplorationID, findingNodeID, RelDerivedFrom, intentID).Scan(&edgeCount); err != nil {
		t.Fatal(err)
	}
	if anchorCount != 1 || edgeCount != 1 {
		t.Fatalf("フォローアップの系譜: anchors=%d edges=%d", anchorCount, edgeCount)
	}
	var persistedAuditCount int
	if err := d.QueryRow(`SELECT COUNT(*) FROM activity
		WHERE id=$1 AND exploration_id=$2 AND node_id=$3 AND worker='system' AND summary=$4`,
		audit.ID, task.ExplorationID, intentID, expectedAuditSummary).Scan(&persistedAuditCount); err != nil {
		t.Fatal(err)
	}
	if persistedAuditCount != 1 {
		t.Fatalf("原子的な監査ログ件数=%d、期待 1", persistedAuditCount)
	}

	var nodesBefore, activitiesBefore int
	if err := d.QueryRow(`SELECT COUNT(*) FROM exploration_nodes WHERE exploration_id=$1`, task.ExplorationID).Scan(&nodesBefore); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT COUNT(*) FROM activity WHERE exploration_id=$1`, task.ExplorationID).Scan(&activitiesBefore); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddFindingFollowUpIntent(findingID, findingNodeID, "must roll back", Activity{
		Worker: "system", Kind: "text", Summary: "invalid audit", Metadata: json.RawMessage(`{`),
	}); err == nil {
		t.Fatal("不正な activity メタデータはトランザクションを失敗させるべき")
	}
	var nodesAfter, activitiesAfter int
	if err := d.QueryRow(`SELECT COUNT(*) FROM exploration_nodes WHERE exploration_id=$1`, task.ExplorationID).Scan(&nodesAfter); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT COUNT(*) FROM activity WHERE exploration_id=$1`, task.ExplorationID).Scan(&activitiesAfter); err != nil {
		t.Fatal(err)
	}
	if nodesAfter != nodesBefore || activitiesAfter != activitiesBefore {
		t.Fatalf("activity の失敗でグラフがロールバックされていない: nodes %d->%d activities %d->%d",
			nodesBefore, nodesAfter, activitiesBefore, activitiesAfter)
	}

	if _, err := d.Exec(`DELETE FROM exploration_nodes WHERE id=$1`, findingNodeID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddFindingFollowUpIntent(findingID, findingNodeID, "should fail", auditInput); !errors.Is(err, ErrFindingOriginUnavailable) {
		t.Fatalf("由来ノードが消失: ErrFindingOriginUnavailable を期待したが %v", err)
	}
}
