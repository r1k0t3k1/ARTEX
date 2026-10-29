package traffic

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// openLegacyIndex builds the index exactly as the pre-reclamation Open did: a
// plain-path DSN, pragmas via the pool, and auto_vacuum left at its default 0.
func openLegacyIndex(t *testing.T, dir string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "_index"), 0o755); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", filepath.Join(dir, "_index", "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := old.Exec(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.Exec(indexSchema); err != nil {
		t.Fatal(err)
	}
	return old
}

// TestUpgradeFromOldInstall guards the upgrade path. Open now names the database
// through a file: URI so per-connection pragmas can ride in the DSN, and a
// driver that did not treat that as a URI would quietly open a file literally
// named "file:/…" — an empty index, with every recorded exchange apparently
// gone. The assertions below are what prove that does not happen.
func TestUpgradeFromOldInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "_index", "index.sqlite")
	old := openLegacyIndex(t, dir)
	if _, err := old.Exec(ftsSchema); err != nil {
		t.Fatal(err)
	}
	// 過去のトラフィック 3 件。うち 1 件は legacy な path<>'' の行
	for i, row := range [][]any{
		{"1700000000-0001", "old.example.com", ""},
		{"1700000000-0002", "old.example.com", ""},
		{"1700000000-0003", "legacy.example.com", "legacy.example.com/GET/x"},
	} {
		if _, err := old.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,'GET','/x','http://x/x',200,'text/html',0,9,?)`, row[0], 1700000000+i, row[1], row[2]); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO exchange_bodies(id,req_head,req_body,resp_head,resp_body)
VALUES(?,'GET /x','','HTTP 200','旧データ本文')`, row[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO ex_fts(rowid,content) VALUES(?,?)`, i+1, "旧データ本文 secret-token"); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// ---- 新バージョンが引き継ぐ
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("新バージョンが旧 DB を開けない: %v", err)
	}
	defer tr.Close()

	// 1. 同じファイルであること。こっそり新しい空 DB を開いてはならない
	if st2, err := os.Stat(path); err != nil || st2.Size() == 0 {
		t.Fatalf("元の索引ファイルが異常: size=%v err=%v", st2, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "_index")); len(entries) > 3 {
		for _, e := range entries {
			t.Logf("_index 配下: %s", e.Name())
		}
		t.Fatal("_index 配下に想定外のファイルがある。DSN が別の DB を指している可能性")
	}
	t.Logf("旧 DB %d バイト、新バージョン引き継ぎ後も同一ファイル", stat.Size())

	// 2. 過去のデータがすべて見える
	n, err := tr.Count()
	if err != nil || n != 3 {
		t.Fatalf("Count=(%d,%v)、%s であるべき —— 過去のトラフィックが失われた", n, err, "(3,nil)")
	}
	// 3. 過去の全文索引が引き続き検索できる
	if tr.fts {
		rows, err := tr.query("old.example.com", "", "secret-token", 0, 10)
		if err != nil {
			t.Fatalf("過去データの全文検索に失敗: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("過去データの全文検索が %d 件ヒット、2 件であるべき", len(rows))
		}
	}
	// 4. 過去の本文が引き続き読める
	if _, resp, err := tr.Get("1700000000-0001"); err != nil {
		t.Fatalf("過去の本文の読み込みに失敗: %v", err)
	} else if resp == "" {
		t.Fatal("過去のレスポンスが空")
	}
	// 5. 旧 DB が増分回収有効と誤判定されない
	if tr.incrementalVacuum {
		t.Fatal("旧 DB が増分回収有効と誤判定された")
	}
	// 6. 削除が引き続き正常に動作し、回収処理が旧 DB 上で収束する
	deleted, err := tr.DeleteHostsExact([]string{"old.example.com"})
	if err != nil || deleted != 2 {
		t.Fatalf("DeleteHostsExact=(%d,%v)、%s であるべき", deleted, err, "(2,nil)")
	}
	tr.reaping.Wait()
	if n, err := tr.Count(); err != nil || n != 1 {
		t.Fatalf("削除後の Count=(%d,%v)、%s であるべき", n, err, "(1,nil)")
	}
	// 7. legacy な path<>'' の行が巻き込まれていない
	var legacyPath string
	if err := tr.DB().QueryRow(`SELECT path FROM exchanges`).Scan(&legacyPath); err != nil {
		t.Fatal(err)
	}
	if legacyPath == "" {
		t.Fatal("legacy 行の path が空にされた")
	}
}

// TestDowngradeToOldBinary covers a rollback: a database created with
// auto_vacuum=incremental must stay readable and writable by a build that knows
// nothing about it. auto_vacuum only changes where SQLite tracks free pages, so
// the old binary simply goes back to never returning them.
func TestDowngradeToOldBinary(t *testing.T) {
	dir := t.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if !tr.incrementalVacuum {
		t.Fatal("新規 DB では増分回収が有効であるべき")
	}
	bulkRecord(tr, "keep.example.com", 5, 100*1024)
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}

	old := openLegacyIndex(t, dir) // 旧バージョンのバイナリが引き継ぐ
	defer old.Close()
	var n int
	if err := old.QueryRow(`SELECT COUNT(*) FROM exchanges`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("旧バージョンで (%d,%v) を読み取り、%s であるべき", n, err, "(5,nil)")
	}
	if _, err := old.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES('x',1,'new.example.com','GET','/x','http://x/x',200,'',0,0,'')`); err != nil {
		t.Fatalf("旧バージョンでの書き込みに失敗: %v", err)
	}
	if _, err := old.Exec(`DELETE FROM exchanges WHERE host='keep.example.com'`); err != nil {
		t.Fatalf("旧バージョンでの削除に失敗: %v", err)
	}
}
