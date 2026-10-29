package db

import (
	"fmt"
	"testing"
	"time"
)

// TestParseScopeLine covers classification + guardrails without a DB.
func TestParseScopeLine(t *testing.T) {
	cases := []struct {
		in      string
		kind    string
		wantErr bool
	}{
		{"example.com", "domain", false},
		{"https://sub.example.com/path", "domain", false},
		{"1.2.3.4", "ip", false},
		{"10.0.0.0/8", "", true}, // over-broad IPv4 (< /16)
		{"198.51.100.0/24", "cidr", false},
		{"co.uk", "", true}, // bare public suffix
		{"not a host", "", true},
		{"1.2.3.1-1.2.3.9", "", true}, // ranges must be CIDR
	}
	for _, c := range cases {
		r, err := ParseScopeLine(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseScopeLine(%q) want error, got %+v", c.in, r)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseScopeLine(%q) unexpected error: %v", c.in, err)
			continue
		}
		if r.Kind != c.kind {
			t.Errorf("ParseScopeLine(%q) kind=%q want %q", c.in, r.Kind, c.kind)
		}
	}
}

func TestParseAutoScopeLine(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		kind       string
		normalized string
		wantErr    bool
	}{
		{name: "domain", input: "example.com", kind: "domain", normalized: "example.com"},
		{name: "url", input: "https://sub.example.com/path", kind: "domain", normalized: "sub.example.com"},
		{name: "url query", input: "https://example.com/path?source=x", kind: "domain", normalized: "example.com"},
		{name: "url credentials", input: "https://user:pass@example.com/path", kind: "domain", normalized: "example.com"},
		{name: "url ip", input: "http://203.0.113.10/path", kind: "ip", normalized: "203.0.113.10/32"},
		{name: "ipv4", input: "203.0.113.10", kind: "ip", normalized: "203.0.113.10/32"},
		{name: "ipv6", input: "2001:db8::10", kind: "ip", normalized: "2001:db8::10/128"},
		{name: "cidr", input: "198.51.100.0/24", kind: "cidr", normalized: "198.51.100.0/24"},
		{name: "icp latin", input: "京 ICP备 123号", kind: "icp", normalized: "京icp备123号"},
		{name: "icp chinese", input: "沪网备案 9988", kind: "icp", normalized: "沪网备案9988"},
		{name: "icp domain", input: "icp.example.com", kind: "domain", normalized: "icp.example.com"},
		{name: "icp url query", input: "https://example.com/path?icp=1", kind: "domain", normalized: "example.com"},
		// ICP 登録番号にドットを含まない：ドメインやバージョン番号を混ぜた説明的な文字列はキーワード扱いにする。
		// さもないと永遠に一致しない死んだ ICP ルールとして保存されてしまう。
		{name: "icp with domain text", input: "备案 www.example.com", kind: "keyword", normalized: "备案 www.example.com"},
		{name: "icp with version text", input: "某公司 ICP v1.0", kind: "keyword", normalized: "某公司 icp v1.0"},
		{name: "icp fullwidth dot", input: "备案 例．com", kind: "keyword", normalized: "备案 例．com"},
		{name: "keyword", input: "  ACME   Security  ", kind: "keyword", normalized: "acme security"},
		{name: "colon keyword", input: "ACME: Cloud: Security", kind: "keyword", normalized: "acme: cloud: security"},
		{name: "empty", input: "  ", wantErr: true},
		{name: "invalid cidr", input: "10.0.0.0/not-a-prefix", wantErr: true},
		{name: "invalid ipv4", input: "999.0.0.1", wantErr: true},
		{name: "invalid ipv6", input: "2001:db8::zz", wantErr: true},
		{name: "invalid ipv6 cidr", input: "2001:db8::zz/64", wantErr: true},
		{name: "overbroad cidr", input: "10.0.0.0/8", wantErr: true},
		{name: "bare suffix", input: "co.uk", wantErr: true},
		{name: "empty domain label", input: "foo..example.com", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule, err := ParseAutoScopeLine(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseAutoScopeLine(%q) = %+v, want error", tc.input, rule)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAutoScopeLine(%q): %v", tc.input, err)
			}
			if rule.Kind != tc.kind {
				t.Fatalf("kind=%q want %q", rule.Kind, tc.kind)
			}
			got := rule.Value
			if rule.Kind == "domain" {
				got = rule.Domain
			} else if rule.Kind == "ip" || rule.Kind == "cidr" {
				got = rule.Net
			}
			if got != tc.normalized {
				t.Fatalf("normalized=%q want %q", got, tc.normalized)
			}
		})
	}
}

func TestExplicitCompanyAttributionSurvivesScopeRebuild(t *testing.T) {
	d, as, cs := testSetup(t)
	defer d.Close()

	stamp := time.Now().UnixNano()
	explicitCompany, _, err := cs.UpsertCompany(fmt.Sprintf("Explicit Attribution %d", stamp), "")
	if err != nil {
		t.Fatal(err)
	}
	autoCompany, _, err := cs.UpsertCompany(fmt.Sprintf("Automatic Attribution %d", stamp), "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupCompany(d, explicitCompany)
	defer cleanupCompany(d, autoCompany)

	domain := fmt.Sprintf("explicit-%d.invalid", stamp)
	icp := fmt.Sprintf("ICP-%d", stamp)
	network := fmt.Sprintf("2001:db8:%x::/64", uint64(stamp)&0xffff)
	ip := fmt.Sprintf("2001:db8:%x::10", uint64(stamp)&0xffff)

	// A pre-existing row with company_id and no provenance value represents old
	// installations. The schema default conservatively treats it as explicit.
	var assetID int64
	if err := d.QueryRow(`INSERT INTO assets(type,domain,root_domain,company_id)
		VALUES ('root_domain',$1,$1,$2) RETURNING id`, domain, explicitCompany).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, assetID)
	appID, err := as.UpsertApp(UpsertAppReq{
		Name: fmt.Sprintf("explicit-app-%d", stamp), ICP: icp, CompanyID: &explicitCompany,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, appID)
	autoAssetID, err := as.UpsertIP(UpsertIPReq{IP: ip})
	if err != nil {
		t.Fatal(err)
	}
	defer deleteAsset(d, autoAssetID)

	rules := []ScopeInput{
		{Kind: "domain", Value: domain},
		{Kind: "icp", Value: icp},
		{Kind: "cidr", Value: network},
	}
	if added, _, invalid, errs := cs.AddScopeInputs(autoCompany, rules, "test"); added != len(rules) || invalid != 0 {
		t.Fatalf("AddScopeInputs: added=%d invalid=%d errors=%v", added, invalid, errs)
	}

	assertCompany := func(id, want int64, source string) {
		t.Helper()
		var got *int64
		var gotSource string
		if err := d.QueryRow(`SELECT company_id,company_source FROM assets WHERE id=$1`, id).Scan(&got, &gotSource); err != nil {
			t.Fatal(err)
		}
		if got == nil || *got != want || gotSource != source {
			t.Fatalf("asset %d company=%v source=%q, want %d/%q", id, got, gotSource, want, source)
		}
	}
	assertCompany(assetID, explicitCompany, "explicit")
	assertCompany(appID, explicitCompany, "explicit")
	assertCompany(autoAssetID, autoCompany, "scope")

	// Replacing every matching rule detaches only the automatically-owned row.
	if _, invalid, errs := cs.UpdateScopeInputs(autoCompany, []ScopeInput{
		{Kind: "domain", Value: fmt.Sprintf("replacement-%d.invalid", stamp)},
	}, "test"); invalid != 0 || len(errs) != 0 {
		t.Fatalf("UpdateScopeInputs: invalid=%d errors=%v", invalid, errs)
	}
	assertCompany(assetID, explicitCompany, "explicit")
	assertCompany(appID, explicitCompany, "explicit")
	var autoCompanyID *int64
	var autoSource string
	if err := d.QueryRow(`SELECT company_id,company_source FROM assets WHERE id=$1`, autoAssetID).Scan(&autoCompanyID, &autoSource); err != nil {
		t.Fatal(err)
	}
	if autoCompanyID != nil || autoSource != "scope" {
		t.Fatalf("automatic asset was not detached: company=%v source=%q", autoCompanyID, autoSource)
	}

	if err := cs.RecomputeAttribution(); err != nil {
		t.Fatal(err)
	}
	assertCompany(assetID, explicitCompany, "explicit")
	assertCompany(appID, explicitCompany, "explicit")

	// Deleting the explicitly selected company detaches through the FK, then the
	// transactional rebuild may adopt the assets into a still-valid scope.
	if _, invalid, errs := cs.UpdateScopeInputs(autoCompany, []ScopeInput{
		{Kind: "domain", Value: domain},
		{Kind: "icp", Value: icp},
	}, "test"); invalid != 0 || len(errs) != 0 {
		t.Fatalf("restore fallback scope: invalid=%d errors=%v", invalid, errs)
	}
	if err := cs.DeleteCompany(explicitCompany); err != nil {
		t.Fatal(err)
	}
	assertCompany(assetID, autoCompany, "scope")
	assertCompany(appID, autoCompany, "scope")
}

func TestParseStructuredCompanyScope(t *testing.T) {
	icp, err := ParseScopeInput(ScopeInput{Kind: "ICP", Value: " 京ICP 备 123号-1\t"})
	if err != nil {
		t.Fatalf("parse ICP: %v", err)
	}
	if icp.Kind != "icp" || icp.Value != "京icp备123号-1" {
		t.Fatalf("unexpected normalized ICP: %+v", icp)
	}
	keyword, err := ParseScopeInput(ScopeInput{Kind: "keyword", Value: "  ACME   Security  "})
	if err != nil {
		t.Fatalf("parse keyword: %v", err)
	}
	if keyword.Value != "acme security" {
		t.Fatalf("unexpected normalized keyword: %+v", keyword)
	}
	if _, err := ParseScopeInput(ScopeInput{Kind: "ip", Value: "example.com"}); err == nil {
		t.Fatal("typed IP accepted a domain")
	}
}

func TestCompanyICPAttribution(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	// 接続のクローズは必ず t.Cleanup で行い、しかも**後始末より先に登録**する：t.Cleanup は後入れ先出しなので、
	// 先にクローズを登録 → クローズが最後に実行され、下のデータ後始末が DB に接続できる。
	// 以前ここは `defer d.Close()` だった：defer は関数の return 時に先に走り、t.Cleanup はその後に
	// 実行されるため、後始末の文がすべて**閉じた接続**上で走り、しかもエラーが `_, _ =` で捨てられて、
	// 資産と企業が DB に永久に残っていた。残留自体はすぐにはエラーにならないが、このテストは
	// `MAX(companies.id)+1` を偽の TaskID として資産に付ける（下の suffix 参照）ため、
	// この数字が他のテストのタスク id と衝突すると、そのテストの「ちょうど N 件の資産」という検証が
	// 不可解に失敗する——原因究明のコストが極めて高い。
	t.Cleanup(func() { d.Close() })

	var suffix int64
	if err := d.QueryRow(`SELECT COALESCE(MAX(id),0)+1 FROM companies`).Scan(&suffix); err != nil {
		t.Fatal(err)
	}
	cs := d.Companies()
	as := d.Assets()
	companyID, _, err := cs.UpsertCompany(fmt.Sprintf("ICP Scope Co %d", suffix), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// エラーを飲み込まない：後始末の失敗は後続のテストを汚染するので、今回の実行で必ず表面化させる。
		if _, err := d.Exec(`DELETE FROM assets WHERE task_ids @> ARRAY[$1]::bigint[]`, suffix); err != nil {
			t.Errorf("テスト資産の後始末に失敗: %v", err)
		}
		if _, err := d.Exec(`DELETE FROM companies WHERE id=$1`, companyID); err != nil {
			t.Errorf("テスト企業の後始末に失敗: %v", err)
		}
	})

	// キーワードは Agent を導けるが、その名前で資産を主張してはならない。
	added, _, invalid, errs := cs.AddScopeInputs(companyID, []ScopeInput{
		{Kind: "icp", Value: "京 ICP备 998877号"},
		{Kind: "keyword", Value: "ICP Scope"},
	}, "unit test")
	if added != 2 || invalid != 0 || len(errs) != 0 {
		t.Fatalf("add structured scope: added=%d invalid=%d errors=%v", added, invalid, errs)
	}

	rootID, err := as.UpsertRootDomain(UpsertRootDomainReq{
		Domain: fmt.Sprintf("icp-scope-%d.example", suffix), ICP: "京icp备998877号", TaskID: suffix,
	})
	if err != nil {
		t.Fatal(err)
	}
	appID, err := as.UpsertApp(UpsertAppReq{
		Name: fmt.Sprintf("ICP Scope Keyword Only %d", suffix), TaskID: suffix,
	})
	if err != nil {
		t.Fatal(err)
	}
	icpAppID, err := as.UpsertApp(UpsertAppReq{
		Name: fmt.Sprintf("ICP Matched App %d", suffix), ICP: " 京 ICP备 998877号 ", TaskID: suffix,
	})
	if err != nil {
		t.Fatal(err)
	}

	assertCompany := func(assetID int64, want *int64) {
		t.Helper()
		var got *int64
		if err := d.QueryRow(`SELECT company_id FROM assets WHERE id=$1`, assetID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if want == nil && got != nil {
			t.Fatalf("asset %d attributed by keyword: %d", assetID, *got)
		}
		if want != nil && (got == nil || *got != *want) {
			t.Fatalf("asset %d company=%v want %d", assetID, got, *want)
		}
	}
	assertCompany(rootID, &companyID)
	assertCompany(appID, nil)
	assertCompany(icpAppID, &companyID)
}

// TestCompanyScopeAttribution は開発用 PG に対して一連の流れを実走する：企業を作成し（一意な名前）、
// スコープを追加し、挿入時の自動紐付け、既存資産のバックフィル、CIDR + ドメイン接尾辞の一致、
// そしてスコープ外の資産が紐付かないことを検証する。
func TestCompanyScopeAttribution(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	as := d.Assets()
	cs := d.Companies()

	var startMax int64
	if err := d.QueryRow(`SELECT COALESCE(MAX(id),0) FROM assets`).Scan(&startMax); err != nil {
		d.Close()
		t.Fatalf("startMax: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM assets WHERE id > $1`, startMax)
		d.Close()
	})

	uniq := startMax + 1
	root := fmt.Sprintf("scopetest%d.com", uniq)
	sub := "api." + root
	ipIn := "198.51.100.9"
	ipOut := "203.0.113.9"
	outDomain := fmt.Sprintf("other%d.net", uniq)

	cid, _, err := cs.UpsertCompany(fmt.Sprintf("ScopeCo %d", uniq), "")
	if err != nil {
		t.Fatalf("UpsertCompany: %v", err)
	}

	// 既存資産（スコープ追加より前に挿入）——バックフィルされねばならない。
	preID, err := as.UpsertSubdomain(UpsertSubdomainReq{Domain: sub})
	if err != nil {
		t.Fatalf("pre upsert: %v", err)
	}
	var preCompanyID *int64
	d.QueryRow(`SELECT company_id FROM assets WHERE id = $1`, preID).Scan(&preCompanyID)
	if preCompanyID != nil {
		t.Fatalf("pre-scope asset should be unattributed, got %v", *preCompanyID)
	}

	cs.AddScope(cid, []string{root, "198.51.100.0/24"}, "unit test")

	mustCid := func(id int64, want int64, label string) {
		var cID *int64
		d.QueryRow(`SELECT company_id FROM assets WHERE id = $1`, id).Scan(&cID)
		if cID == nil {
			t.Fatalf("%s company_id = nil, want %d", label, want)
		}
		if *cID != want {
			t.Fatalf("%s company_id = %d, want %d", label, *cID, want)
		}
	}
	mustNil := func(id int64, label string) {
		var cID *int64
		d.QueryRow(`SELECT company_id FROM assets WHERE id = $1`, id).Scan(&cID)
		if cID != nil {
			t.Fatalf("%s should be unattributed, got %d", label, *cID)
		}
	}

	// バックフィルが既存サブドメインを紐付けた（ドメイン接尾辞の一致）。
	mustCid(preID, cid, "pre-existing subdomain (backfill)")

	// 挿入時の紐付け：CIDR 内の IP と、もう 1 つのサブドメイン。
	ipInID, err := as.UpsertIP(UpsertIPReq{IP: ipIn})
	if err != nil {
		t.Fatalf("UpsertIP in: %v", err)
	}
	mustCid(ipInID, cid, "in-CIDR ip (insert-time)")

	sub2ID, err := as.UpsertSubdomain(UpsertSubdomainReq{Domain: "www." + root})
	if err != nil {
		t.Fatalf("UpsertSubdomain: %v", err)
	}
	mustCid(sub2ID, cid, "new subdomain (insert-time)")

	// スコープ外は紐付かないまま。
	outID, err := as.UpsertRootDomain(UpsertRootDomainReq{Domain: outDomain})
	if err != nil {
		t.Fatalf("UpsertRootDomain out: %v", err)
	}
	mustNil(outID, "out-of-scope domain")

	ipOutID, err := as.UpsertIP(UpsertIPReq{IP: ipOut})
	if err != nil {
		t.Fatalf("UpsertIP out: %v", err)
	}
	mustNil(ipOutID, "out-of-CIDR ip")
}
