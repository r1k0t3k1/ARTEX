package db

import (
	"regexp"
	"testing"
)

// 組み込みの「削除系エンドポイントパス」ルールは tool_input の JSON 文字列全体に一致するため、
// 各ケースも JSON の形で与え、Interceptor が実際に受け取る subject と揃えている。
func TestDeleteEndpointPathPattern(t *testing.T) {
	re := regexp.MustCompile(deleteEndpointPathPattern)

	hit := []string{
		`{"command":"curl -s 'http://t.com/api/user/delete?id=1'"}`,    // GET で削除系エンドポイントを叩く
		`{"command":"curl -X POST http://t.com/admin/delete -d id=1"}`, // POST で削除系エンドポイントを叩く
		`{"command":"curl 'http://t.com/api/deleteAll'"}`,
		`{"command":"curl 'http://t.com/api/delete_user?id=1'"}`,
		`{"command":"curl 'http://t.com/api/delete-user?id=1'"}`,
		`{"url":"http://t.com/api/remove?id=1"}`,
		`{"command":"curl http://t.com/files/unlink/3"}`,
		`{"command":"curl http://t.com/api/del?id=2"}`,
		`{"command":"curl -X POST http://t/v1/erase"}`,
		`{"command":"curl http://t/admin/destroyAll"}`, // v1 のパスルールは接尾辞を許さないので、ここで補う
	}
	for _, s := range hit {
		if !re.MatchString(s) {
			t.Errorf("ヒットすべきなのに通過した: %s", s)
		}
	}

	// 動詞の後には区切り文字が必要で、/delivery や /details のような参照専用パスを誤って止めない。
	miss := []string{
		`{"command":"curl 'http://t.com/api/delivery?id=1'"}`,
		`{"command":"curl 'http://t.com/order/details'"}`,
		`{"command":"curl 'http://t.com/api/delta/sync'"}`,
		`{"command":"curl 'http://t.com/user/delegate'"}`,
		`{"command":"curl 'http://delete.example.com/'"}`, // 削除動詞がパスではなくドメインに現れる
		`{"command":"curl 'http://t.com/remote/status'"}`,
		`{"command":"nmap -p80 10.0.0.1"}`,
	}
	for _, s := range miss {
		if re.MatchString(s) {
			t.Errorf("誤って止めた: %s", s)
		}
	}
}
