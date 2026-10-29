package db

import (
	"strconv"
	"strings"
	"testing"
)

// cleanupTreeFixtures は 1 つのテストが作った資産と検出を削除する。必ず defer で登録する
// （t.Cleanup ではなく）: t.Cleanup はテスト関数の return 後に走るが、その時点では defer d.Close() が
// すでに接続を閉じているため、後始末が黙って失敗し、共有開発 DB に汚れたデータが残ってしまう。
func cleanupTreeFixtures(d *DB, taskID int64, rootDomains ...string) {
	d.Exec(`DELETE FROM assets WHERE root_domain = ANY($1::text[])`, rootDomains) //nolint:errcheck
	d.DeleteFindingsByTask(taskID)                                                //nolint:errcheck
}

// seedTreeAsset は資産行を 1 件挿入する。
func seedTreeAsset(t *testing.T, d *DB, kind string, cols map[string]any) int64 {
	t.Helper()
	names := []string{"type"}
	values := []any{kind}
	placeholders := []string{"$1"}
	for k, v := range cols {
		values = append(values, v)
		names = append(names, k)
		placeholders = append(placeholders, "$"+strconv.Itoa(len(values)))
	}
	q := "INSERT INTO assets(" + strings.Join(names, ",") + ") VALUES (" +
		strings.Join(placeholders, ",") + ") RETURNING id"
	var id int64
	if err := d.QueryRow(q, values...).Scan(&id); err != nil {
		t.Fatalf("seed %s asset: %v", kind, err)
	}
	return id
}

func nodeByKey(tree *FindingAssetTree, key string) *FindingAssetNode {
	for i := range tree.Nodes {
		if tree.Nodes[i].Key == key {
			return &tree.Nodes[i]
		}
	}
	return nil
}

// TestBuildFindingAssetTree は「資産別」ツリーの全体像を扱う：葉だけを指す finding から
// root→subdomain→service→endpoint の連鎖を再構築し、祖先はサブツリーを集計し、
// finding を持たない資産はツリーに入らず、資産行が消えた finding は
// 未関連バケットに入ることを検証する。
func TestBuildFindingAssetTree(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	tk, err := d.CreateTask("資産ツリーテスト", "目標", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)

	const root = "tree-test.example"
	const sub = "api.tree-test.example"
	defer cleanupTreeFixtures(d, tk.ID, root)
	rootID := seedTreeAsset(t, d, "root_domain", map[string]any{"domain": root, "root_domain": root})
	subID := seedTreeAsset(t, d, "subdomain", map[string]any{"domain": sub, "root_domain": root})
	svcID := seedTreeAsset(t, d, "service", map[string]any{
		"domain": sub, "root_domain": root, "url": "https://" + sub, "port": 443, "service_type": "http",
	})
	epID := seedTreeAsset(t, d, "endpoint", map[string]any{
		"domain": sub, "root_domain": root, "url": "https://" + sub + "/admin", "port": 443, "method": "GET",
	})
	// 同じドメイン下の別サービスで、finding を 1 つも付けない —— ツリーに出てはいけない。
	seedTreeAsset(t, d, "service", map[string]any{
		"domain": sub, "root_domain": root, "url": "http://" + sub + ":8080", "port": 8080, "service_type": "http",
	})

	// finding は最も深い endpoint にだけ付け、祖先の連鎖はツリー構築側で補わせる。
	if _, err := d.AddFinding(tk.ID, 0, "XSS", "反射型 XSS", "high", "s", "e", "w", []int64{epID}); err != nil {
		t.Fatal(err)
	}
	// サービスに直接付けた 1 件。Self と Total の違いを検証するために使う。
	if _, err := d.AddFinding(tk.ID, 0, "Info", "情報漏えい", "low", "s", "e", "w", []int64{svcID}); err != nil {
		t.Fatal(err)
	}
	// 資産行が存在しない（削除済み資産）→ 未関連バケット。
	if _, err := d.AddFinding(tk.ID, 0, "Misc", "孤立", "medium", "s", "e", "w", []int64{999000111}); err != nil {
		t.Fatal(err)
	}

	tree, err := d.BuildFindingAssetTree(FindingFilter{TaskID: strconv.FormatInt(tk.ID, 10)})
	if err != nil {
		t.Fatal(err)
	}
	if tree.FindingTotal != 3 {
		t.Fatalf("finding_total: 期待 3、実際 %d", tree.FindingTotal)
	}

	rootNode := nodeByKey(tree, assetKey(rootID))
	subNode := nodeByKey(tree, assetKey(subID))
	svcNode := nodeByKey(tree, assetKey(svcID))
	epNode := nodeByKey(tree, assetKey(epID))
	for name, n := range map[string]*FindingAssetNode{
		"root": rootNode, "subdomain": subNode, "service": svcNode, "endpoint": epNode,
	} {
		if n == nil {
			t.Fatalf("%s ノードがツリーに存在しない", name)
		}
	}

	// 親子連鎖：endpoint → service → subdomain → root_domain。
	if epNode.Parent != svcNode.Key {
		t.Errorf("endpoint の親: 期待 %s、実際 %s", svcNode.Key, epNode.Parent)
	}
	if svcNode.Parent != subNode.Key {
		t.Errorf("service の親: 期待 %s、実際 %s", subNode.Key, svcNode.Parent)
	}
	if subNode.Parent != rootNode.Key {
		t.Errorf("subdomain の親: 期待 %s、実際 %s", rootNode.Key, subNode.Parent)
	}
	if rootNode.Parent != "" {
		t.Errorf("root の親: 期待は最上位、実際 %s", rootNode.Parent)
	}

	// 集計：ルートドメインは 2 件（endpoint の high + service の low）、service 自身は 1 件、サブツリーは 2 件。
	if rootNode.Total != 2 || rootNode.High != 1 || rootNode.Low != 1 {
		t.Errorf("root の集計: 期待 2/high1/low1、実際 %d/high%d/low%d", rootNode.Total, rootNode.High, rootNode.Low)
	}
	if rootNode.Self != 0 {
		t.Errorf("root の self: 期待 0（単なる祖先）、実際 %d", rootNode.Self)
	}
	if svcNode.Total != 2 || svcNode.Self != 1 {
		t.Errorf("service の total/self: 期待 2/1、実際 %d/%d", svcNode.Total, svcNode.Self)
	}
	if epNode.Total != 1 || epNode.Self != 1 {
		t.Errorf("endpoint の total/self: 期待 1/1、実際 %d/%d", epNode.Total, epNode.Self)
	}

	// finding を持たない兄弟サービスはツリーに入らない。
	for _, n := range tree.Nodes {
		if n.Label == "http://"+sub+":8080" {
			t.Errorf("finding を持たない資産は表示されないべき: %+v", n)
		}
	}

	// 未関連バケットが、削除済み資産を指す finding を受け取る。
	none := nodeByKey(tree, FindingUnassignedAsset)
	if none == nil || none.Total != 1 || none.Medium != 1 {
		t.Fatalf("未関連バケット: 期待 1 medium、実際 %+v", none)
	}
}

// TestFindingAssetScopeFilter は、ノードを 1 つ選ぶと finding 一覧がそのノードの
// サブツリー全体に絞られること、および未関連センチネルも機能することを検証する。
func TestFindingAssetScopeFilter(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	tk, err := d.CreateTask("資産フィルタテスト", "目標", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)

	const root = "scope-test.example"
	const sub = "api.scope-test.example"
	const other = "other-scope-test.example"
	defer cleanupTreeFixtures(d, tk.ID, root, other)
	rootID := seedTreeAsset(t, d, "root_domain", map[string]any{"domain": root, "root_domain": root})
	subID := seedTreeAsset(t, d, "subdomain", map[string]any{"domain": sub, "root_domain": root})
	otherID := seedTreeAsset(t, d, "root_domain", map[string]any{"domain": other, "root_domain": other})

	if _, err := d.AddFinding(tk.ID, 0, "A", "サブドメイン上のもの", "high", "s", "e", "w", []int64{subID}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddFinding(tk.ID, 0, "B", "別のルートドメイン上のもの", "high", "s", "e", "w", []int64{otherID}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddFinding(tk.ID, 0, "C", "資産のないもの", "high", "s", "e", "w", nil); err != nil {
		t.Fatal(err)
	}
	// 削除済み資産を指す finding は、asset_ids が空のものと同じく「未関連」に属する——ツリーのバケットが受け取り、
	// 一覧の絞り込みでも取得できねばならない。両者の基準が食い違うと、バケット上の数字が開いた後の件数より大きくなる。
	if _, err := d.AddFinding(tk.ID, 0, "D", "資産が削除済み", "high", "s", "e", "w", []int64{999000333}); err != nil {
		t.Fatal(err)
	}

	base := FindingFilter{TaskID: strconv.FormatInt(tk.ID, 10)}
	cases := []struct {
		name  string
		scope string
		want  int
	}{
		{"サブツリー全体", assetKey(rootID), 1},   // ルートドメイン下にはサブドメインの 1 件だけ
		{"葉ノード", assetKey(subID), 1},       // サブドメイン自身
		{"別のツリー", assetKey(otherID), 1},    // 互いに混ざらない
		{"未関連", FindingUnassignedAsset, 2}, // asset_ids が空 + 削除済み資産を指すもの
		{"存在しないノード", "a:999000222", 0},     // 現在の絞り込みに該当ノードなし → 空結果（絞り込みなしではない）
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := base
			f.AssetScope = tc.scope
			items, total, err := d.ListFindingsPage(f, 1, 50)
			if err != nil {
				t.Fatal(err)
			}
			if total != tc.want || len(items) != tc.want {
				t.Fatalf("scope %s: 期待 %d 件、実際 total=%d items=%d", tc.scope, tc.want, total, len(items))
			}
		})
	}

	// scope なしなら 4 件すべてが対象。
	if _, total, err := d.ListFindingsPage(base, 1, 50); err != nil || total != 4 {
		t.Fatalf("scope なし: 期待 4、実際 %d (%v)", total, err)
	}

	// ツリー上の未関連バケットの件数は、開いた後に取得できる件数と一致せねばならない —— まさに両者の基準が
	// 分かれたときに壊れる検証である。
	tree, err := d.BuildFindingAssetTree(base)
	if err != nil {
		t.Fatal(err)
	}
	none := nodeByKey(tree, FindingUnassignedAsset)
	if none == nil || none.Total != 2 {
		t.Fatalf("未関連バケットの件数: 期待 2、実際 %+v", none)
	}
}
