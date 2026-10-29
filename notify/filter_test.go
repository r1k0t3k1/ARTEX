package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// 不正な JSON、空入力、型の合わないフィールド——すべてゼロ値の Filter、
	// つまり「フィルタしない」へ退化しなければならない。この不変条件は
	// 「取りこぼすより多く配信する」の落とし所：ここをエラーや中途半端な解析に
	// 変えると、ユーザーが 1 文字設定を間違えるだけで高リスク通知がすべて静かに
	// 失われる。
	cases := []struct {
		name string
		raw  string
	}{
		{"空入力", ""},
		{"不正な JSON", `{not json`},
		{"切り詰めた JSON", `{"min_severity":`},
		{"型の不一致", `{"min_severity": 123, "task_ids": "abc"}`},
		{"トップレベルが配列", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("不正な設定はゼロ値の Filter へ退化すべき、得られたのは %+v", f)
			}
			// ゼロ値の Filter はあらゆるイベントにヒットしなければならない。
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("ゼロ値の Filter はすべてのイベントにヒットすべき")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// 未知のレベルの序数は 0 なので、非空のしきい値にはすべて阻まれるべき
		// （疑わしきは配信しない）。
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: 期待 %v 得られた %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQLインジェクション",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"空の範囲=制限なし", Filter{}, true},
		{"タスクがヒット", Filter{TaskIDs: []int64{7}}, true},
		{"タスクがヒットしない", Filter{TaskIDs: []int64{8}}, false},
		{"タスク複数選択にヒットが含まれる", Filter{TaskIDs: []int64{8, 7}}, true},
		{"資産に共通部分がある", Filter{AssetIDs: []int64{20, 99}}, true},
		{"資産に共通部分がない", Filter{AssetIDs: []int64{99}}, false},
		{"タスクと資産の両方がヒット", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"タスクはヒットだが資産はヒットしない", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("期待 %v 得られた %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"include が空=すべて受信", Filter{}, "任意のタイプ", true},
		{"include がヒット", Filter{VulnClassInclude: []string{"SQL"}}, "SQLインジェクション", true},
		{"include がヒットしない", Filter{VulnClassInclude: []string{"コマンド実行"}}, "SQLインジェクション", false},
		{"include は複数語のいずれかがヒット", Filter{VulnClassInclude: []string{"コマンド実行", "SQL"}}, "SQLインジェクション", true},
		{"大文字小文字を区別しない", Filter{VulnClassInclude: []string{"sql"}}, "SQLインジェクション", true},
		{"exclude がヒットすれば除外", Filter{VulnClassExclude: []string{"情報漏洩"}}, "情報漏洩", false},
		{"exclude がヒットしなければ通過", Filter{VulnClassExclude: []string{"情報漏洩"}}, "SQLインジェクション", true},
		// 除外は包含より優先：同時にヒットした場合は対象外とする。
		{"除外は包含より優先", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"インジェクション"},
		}, "SQLインジェクション", false},
		// 空白だけのキーワードは無視すべき。さもないと「空白を含むあらゆる文字列に
		// 一致する」へ退化する。
		{"空白キーワードは無視", Filter{VulnClassInclude: []string{"", "  "}}, "SQLインジェクション", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("期待 %v 得られた %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// 既定はオフ：ほとんどの利用者が「脆弱性を配信する」で期待するのは新規検出で
	// あって、各ステータスの遷移を逐一追いかけることではない。
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("ステータス変更イベントは未有効化ならスキップすべき")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("on_status_change を有効化すればステータス変更イベントにヒットすべき")
	}
	// 検出イベントは on_status_change の影響を受けない。
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("検出イベントは on_status_change に依存すべきではない")
	}
}
