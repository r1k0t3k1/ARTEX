package db

import (
	"fmt"
	"testing"
	"time"
)

// InsertSettingIfAbsent は auth.password_hash の最後の砦：呼び出し側の GetSetting 確認は
// DB エラーで無効になり得るし、並行リクエストに割り込まれることもある（bcrypt は数十ミリ秒かかる）。
// そのため「初回のみ設定可能」の保証は、上位の if 判定ではなく主キー制約に載せねばならない。
func TestInsertSettingIfAbsentDoesNotOverwrite(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()

	// テスト専用の key を使い、開発 DB の実在する auth.password_hash には決して触れない。
	key := fmt.Sprintf("test.insert_if_absent.%d", time.Now().UnixNano())
	defer func() { _, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, key) }()

	inserted, err := d.InsertSettingIfAbsent(key, "first")
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("初回書き込みは inserted=true を返すべき")
	}

	inserted, err = d.InsertSettingIfAbsent(key, "second")
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("キーが既存なら inserted=false を返すべき")
	}

	got, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		t.Fatalf("GetSetting: ok=%v err=%v", ok, err)
	}
	if got != "first" {
		t.Fatalf("値が %q で上書きされた。%q のままであるべき", got, "first")
	}
}
