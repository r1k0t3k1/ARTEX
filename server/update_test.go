package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache は GitHub クォータを守る層である。未認証の API は 60 回/時/IP しか許されず、
// トップバーの「新バージョンあり」表示はページ読み込みのたびに問い合わせる。キャッシュが
// 失効すると、ユーザーがタブをいくつも開くだけでクォータを使い切り、いざ更新したいときに
// 問い合わせられなくなる。

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 回の問い合わせでオリジン取得は 1 回だけのはず、実際は %d 回", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// ユーザーが「更新を確認」を押したら必ず最新の結果を返すこと。そうでないと、
	// リリース直後のバージョンはキャッシュが切れるまで見えない。
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force はキャッシュを迂回すべき、オリジン取得 2 回を期待、実際は %d 回", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 保存時刻をちょうど失効したところまで戻し、TTL 切れを再現する。
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL 失効後は再取得すべき、2 回を期待、実際は %d 回", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github に到達できない")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーが返るはず")
	}
	// 失敗結果も少しの間キャッシュする。さもないと GitHub に到達できない間、
	// ページ読み込みのたびに無駄にタイムアウトを待つことになる。
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーが返るはず")
	}
	if calls != 1 {
		t.Errorf("エラーは短時間キャッシュされるべき、オリジン取得 1 回を期待、実際は %d 回", calls)
	}

	// ただしエラーの TTL は成功時より明らかに短くなければならない。ネットワーク復旧後すぐ自己修復できるように。
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("エラー TTL(%v) は成功 TTL(%v) より短くなければならない", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーが返るはず")
	}
	if calls != 2 {
		t.Errorf("エラー TTL 失効後は再試行すべき、2 回を期待、実際は %d 回", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// 訪問者がタブを閉じるとリクエストはキャンセルされる。それは GitHub に問題がある
	// わけではないので、絶対に「キャンセル済み」をキャッシュに書いてはならない。
	// そうすると次の 30 分間、すべての訪問者が意味不明なエラーを受け取ることになる。
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // キャッシュを失効させ、再取得を強制する

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("呼び出し元がキャンセルしたときはエラーをそのまま返すべき")
	}

	// 重要な不変条件: キャンセルされた呼び出しは何も痕跡を残さない。キャッシュに
	// 「キャンセル済み」というエラーはなく、前回の良い結果がそのまま残っている。
	if c.err != nil {
		t.Fatalf("キャンセルエラーをキャッシュに書いてはならない、得られた値 %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("キャッシュは前回の良い結果を保持すべき、得られた値 %+v", c.rel)
	}

	// あのキャンセルは新しいデータをもたらさなかったので、次の訪問者は再取得すべきであり、
	// しかも正常に結果を取得できること。前回のキャンセルに巻き込まれてはならない。
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("キャンセル後の正常なリクエストはエラーを返すべきでない: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("正常な結果が得られるはず、得られた値 %+v", rel)
	}
}
