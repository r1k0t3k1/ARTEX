package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// GetSetting は「キーが存在しない」と「読み取りエラー」の戻り値の違いが error だけ：
// どちらの場合も value は空文字列になる。パスワード関連の handler が error を
// 「まだパスワード未設定」と解釈してしまうと、データベースが不安定な間に
// 初期化の入口を開け放つことになる——authInit は未認証リクエストを通して既存の
// 管理者パスワードを上書きさせ、authStatus はフロントエンドをそのまま /setup へ
// 送って同じことをさせてしまう。
//
// この 2 つのテストは handler の接続プールを閉じて読み取り失敗を起こし、
// どちらも fail closed になることを検証する。
func TestAuthStatusFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	// プールを閉じて、以降の GetSetting が sql.ErrNoRows ではなく error を返すようにする。
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	s.authStatus(w, httptest.NewRequest("GET", "/api/auth/status", nil))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (読み取り失敗を未初期化と扱うとユーザーが /setup へ送られパスワードを上書きしてしまう); body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Initialized *bool `json:"initialized"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err == nil && payload.Initialized != nil {
		t.Fatalf("読み取り失敗時に initialized を返してはならない。実際の値は %v", *payload.Initialized)
	}
}

func TestAuthInitFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"password":"correct horse battery"}`)
	s.authInit(w, httptest.NewRequest("POST", "/api/auth/init", body))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (読み取り失敗時に通すと未認証リクエストが既存パスワードを上書きしてしまう); body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "token") {
		t.Fatalf("読み取り失敗時にトークンを発行してはならない: %s", w.Body.String())
	}
}

func TestValidatePassword(t *testing.T) {
	for _, tc := range []struct {
		name, pw string
		wantErr  bool
	}{
		{"空", "", true},
		{"7 文字", "1234567", true},
		{"8 文字", "12345678", false},
		{"マルチバイト 10 文字はバイト数 30 でも文字数で判定する", "パスワードパスワード", false},
		{"漢字 3 文字は 9 バイトあっても 3 文字にしかならない", "秘密鍵", true},
		{"72 バイト", strings.Repeat("a", 72), false},
		{"73 バイトで bcrypt の上限を超える", strings.Repeat("a", 73), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validatePassword(tc.pw); (got != "") != tc.wantErr {
				t.Fatalf("validatePassword(%q)=%q, wantErr=%v", tc.pw, got, tc.wantErr)
			}
		})
	}
}
