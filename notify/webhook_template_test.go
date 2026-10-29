package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 本ファイルは汎用 Webhook テンプレートの**能力の境界**を固定する。
//
// パッケージ全体で唯一「ユーザーが提供した文字列がコードとして評価される」場所な
// ので、何ができて何ができないかを明確にし、テストでこれらの性質を固定しておく
// 必要がある——さもないと将来誰かがついでにテンプレートのコンテキストへメソッドを
// 追加したり、FuncMap に readFile を足したりして、能力面が静かに広がるのに、
// diff だけ見れば無害な小さな関数に見えてしまう。

// TestTemplateContextHasNoMethods が最も重要な 1 つ。
//
// text/template はエクスポートされたメソッドを呼び出す（{{.Foo}} はフィールドの
// 取得もメソッドの呼び出しもできる）。つまりテンプレートのコンテキストが
// **エクスポートされたメソッドを持つ**型に少しでも触れられるなら、それらの
// メソッドをテンプレート作者に公開したのと同じ。本機能のコンテキストは意図的に
// 純粋なデータだけ（エクスポートされたフィールドのみ、メソッドはゼロ）。
//
// これが失敗した場合：誰かが webhookTemplateData / webhookItem にメソッドを
// 追加したということ。許可する前に、そのメソッドを使ってテンプレートが公開したく
// ないものを読み出せないかよく考えること。
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s が %d 個のメソッド（%s）を公開している。text/template はそれらを"+
				"呼び出せるので、これらのメソッドの能力をテンプレート作者に開放したことになる", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal はテンプレートへ公開する関数の集合を固定する。
//
// FuncMap に関数が 1 つ増えるごとに能力も 1 つ増える。現在は json / jsons のみで、
// 役割は値を JSON 断片へシリアライズすること——ファイルの読み取りも、リクエストの
// 送信も、コマンドの実行もできない。
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("テンプレート関数の集合が変わった：得られたのは %v、期待 %v。関数を追加する前に"+
			"それが能力面を広げないことを確認すること（ファイルの読み書き不可、ネットワーク要求不可、コマンド実行不可）", got, want)
	}
}

// TestTemplateCannotReachUnknownData はテンプレート内の範囲外アクセスをカバーする：
// 存在しないものへのアクセスは必ず失敗すべきで、何かをエコーしてはならない。また
// 失敗メッセージが内部データを持ち出してはならない。
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("存在しないフィールドへのアクセスはエラーを返すべき")
	}
	// エラーにテンプレートのコンテキストの実内容（脆弱性のタイトル／サマリー）が
	// 現れてはならない。
	for _, leak := range []string{"SQLインジェクション", "パラメータ id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("テンプレートのエラーがメッセージ内容 %q を漏らした: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently テンプレートの誤記は設定エラーで、再試行しても
// 回復しない。再試行可能と判定されると、壊れたテンプレートのせいで毎回の配信が
// 無駄に 3 回バックオフする。
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("テンプレートの構文エラーは保存時に止めるべき")
	}
	// 検証を迂回して直接配信しても、永久失敗と判定すべきで、繰り返し再試行しては
	// ならない。
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("壊れたテンプレートは永久失敗と判定すべき、得られたのは %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON は「テンプレートのレンダリング結果は正当な JSON で
// なければならない」という制約をカバーする。これがついでに「テンプレートで平文を
// 生成し、別のプロトコルを発火させる」といった使い方も塞いでいる。
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// 正当なテンプレートは通る。
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("正当なテンプレートは検証を通過すべき: %v", err)
	}
	// JSON でないものにレンダリングされた場合は拒否すべき（そのまま送信するのではなく）。
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("JSON でないレンダリング結果は永久失敗と判定すべき、得られたのは %v", err)
	}
	if !strings.Contains(err.Error(), "正当な JSON") {
		t.Errorf("エラーメッセージは JSON の問題だと示すべき、得られたのは %v", err)
	}
}
