package db

import "testing"

// mustIntent / mustNode / mustLink は削除カスケードのテスト用の簡潔なビルダー。
func mustIntent(t *testing.T, es *ExplorationStore, summary string) int64 {
	t.Helper()
	id, err := es.AddIntent(map[string]any{"summary": summary}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustNode(t *testing.T, es *ExplorationStore, kind, summary string) int64 {
	t.Helper()
	id, err := es.AddNode(kind, map[string]any{"summary": summary}, 1, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustLink(t *testing.T, es *ExplorationStore, from int64, rel string, to int64) {
	t.Helper()
	if err := es.Link(from, rel, to); err != nil {
		t.Fatal(err)
	}
}

func gone(t *testing.T, es *ExplorationStore, id int64) bool {
	t.Helper()
	n, err := es.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	return n == nil
}

// TestSoftDeleteIntent は仮削除で deleted + delete_reason を設定し、ノードを残すことを検証する。
func TestSoftDeleteIntent(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("soft delete", "仮削除")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	intent := mustIntent(t, es, "削除対象の意図")
	if err := es.SetIntentState(intent, "paused"); err != nil {
		t.Fatal(err)
	}
	summary, err := es.SoftDeleteIntent(intent, "方向性の判断ミス")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "削除対象の意図" {
		t.Fatalf("summary=%q、期待 削除対象の意図", summary)
	}
	n, err := es.GetNode(intent)
	if err != nil || n == nil {
		t.Fatalf("仮削除で意図が消えた: n=%+v err=%v", n, err)
	}
	if n.State != StateIntentDeleted || n.DeleteReason != "方向性の判断ミス" {
		t.Fatalf("state=%q delete_reason=%q、期待 deleted/方向性の判断ミス", n.State, n.DeleteReason)
	}
	// 未着手（open）の意図も仮削除を許可する。
	openIntent := mustIntent(t, es, "未着手の意図")
	if _, err := es.SoftDeleteIntent(openIntent, "方向性が不要になった"); err != nil {
		t.Fatalf("open の意図の仮削除: %v", err)
	}
	if n, err := es.GetNode(openIntent); err != nil || n == nil || n.State != StateIntentDeleted {
		t.Fatalf("open の意図が仮削除されていない: n=%+v err=%v", n, err)
	}

	// 削除済み（deleted）など他の状態は再度仮削除できない。
	if _, err := es.SoftDeleteIntent(intent, "再削除"); err == nil {
		t.Fatal("削除済みの意図の仮削除が予期せず成功した")
	}
}

// TestHardDeleteCascadesExclusiveDescendants は、本削除が連鎖に沿って排他的な子孫を葉までカスケード削除することを検証する。
func TestHardDeleteCascadesExclusiveDescendants(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("hard cascade", "カスケード削除")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	// intent1 --yields--> fact1 --derived_from--> intent2 --yields--> fact2（葉）
	intent1 := mustIntent(t, es, "ルート意図")
	fact1 := mustNode(t, es, KindFact, "事実1")
	mustLink(t, es, intent1, RelYields, fact1)
	intent2 := mustIntent(t, es, "派生した意図")
	mustLink(t, es, fact1, RelDerivedFrom, intent2)
	fact2 := mustNode(t, es, KindFact, "事実2")
	mustLink(t, es, intent2, RelYields, fact2)

	cleanup, err := es.CancelIntent(intent1)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup.Intents != 2 || cleanup.Facts != 2 {
		t.Fatalf("cleanup=%+v、期待 2 意図 / 2 事実", cleanup)
	}
	for _, id := range []int64{intent1, fact1, intent2, fact2} {
		if !gone(t, es, id) {
			t.Fatalf("ノード %d がカスケード削除を生き延びた", id)
		}
	}
}

// TestHardDeletePreservesSharedAndGoal は、本削除が共有の子孫（他にも親がある）とゴールを残すことを検証する。
func TestHardDeletePreservesSharedAndGoal(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("hard preserve", "共有/ゴールの保持")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	es := d.Exploration(expID)

	goal, err := es.AddGoal(map[string]any{"text": "管理画面を奪取する"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	// intent1 が finding を独占（goal を proves）、intent1 と intentX が fact1 を共有（fact1 から intent2 が派生）。
	intent1 := mustIntent(t, es, "削除対象の意図")
	intentX := mustIntent(t, es, "バイパスの意図")
	finding := mustNode(t, es, KindFinding, "脆弱性")
	mustLink(t, es, intent1, RelYields, finding)
	mustLink(t, es, finding, RelProves, goal)
	shared := mustNode(t, es, KindFact, "共有の事実")
	mustLink(t, es, intent1, RelYields, shared)
	mustLink(t, es, intentX, RelYields, shared)
	intent2 := mustIntent(t, es, "共有の事実から派生")
	mustLink(t, es, shared, RelDerivedFrom, intent2)

	cleanup, err := es.CancelIntent(intent1)
	if err != nil {
		t.Fatal(err)
	}
	// intent1 とそれが独占する finding だけを削除。shared（intentX という親がある）とその下流の intent2、goal はすべて残る。
	if cleanup.Intents != 1 || cleanup.Findings != 1 || cleanup.Facts != 0 {
		t.Fatalf("cleanup=%+v、期待 1 意図 / 1 finding / 0 事実", cleanup)
	}
	if !gone(t, es, intent1) || !gone(t, es, finding) {
		t.Fatal("intent1/finding は削除されるべき")
	}
	for _, id := range []int64{goal, intentX, shared, intent2} {
		if gone(t, es, id) {
			t.Fatalf("ノード %d が誤ってカスケード削除された", id)
		}
	}
}
