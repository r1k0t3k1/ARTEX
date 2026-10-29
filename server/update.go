package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// ページからのワンクリック更新の HTTP 面。実際のダウンロード/検証/差し替え処理は
// すべて selfupdate パッケージにあり、ここは認証境界、排他制御、進捗ブロードキャスト、
// そして「そろそろ終了すべき」を main に伝えることだけを担う。
//
// 再起動はこのプロセスでは行わない。新バージョンをステージしたらプロセスは
// selfupdate.ExitRestart で終了し、監視スクリプト（start.sh / start.bat、
// Docker では ENTRYPOINT）が再起動する。

// restartCh はアップグレードの準備完了またはロールバック完了でクローズされ、
// main が受け取ると ExitRestart で終了する。
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// RestartRequested は「終了して監視プロセスに再起動してほしい」というタイミングで
// クローズされる channel を返す。
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState は今回の起動時における selfupdate.Bootstrap の結論（アップグレード成功 /
// ロールバック直後 / ステージ済みファイルの破棄）で、main が設定する。前回の
// アップグレードの顛末を /api/update/check からフロントに正しく伝えるために使う。
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState は main が起動時に一度だけ呼ぶ。
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// releaseCache は GitHub の最新バージョン問い合わせ結果をキャッシュする。
//
// トップバーの「新バージョンあり」通知はページ読み込みのたびに問い合わせを行うが、
// 未認証の GitHub API は IP あたり毎時 60 回まで——キャッシュしなければタブを
// いくつか開いたりページを数回更新するだけでクォータを食い尽くし、いざ更新したい
// ときに問い合わせられなくなる。ユーザーが明示的に「更新を確認」を押したときは
// force でキャッシュを迂回できる。
type releaseCache struct {
	mu  sync.Mutex
	rel *selfupdate.Release
	err error
	at  time.Time
	// fetch は取得関数で、テスト用の差し込み口としてのみ存在する。nil なら実際の GitHub 問い合わせを行う。
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// 失敗結果も少しの間キャッシュする。さもないと GitHub に到達できないとき、
	// ページ読み込みのたびにタイムアウトを待ち続けることになる。ただし TTL は
	// 短くし、ネットワーク復旧後すぐ自然に回復できるようにする。
	releaseErrTTL = 2 * time.Minute
	// 問い合わせ用タイムアウト。NewClient の 30 分はパッケージ全体のダウンロード用で、
	// バージョン確認にそんなに待つわけにはいかない。
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// get は最新 Release を返す。キャッシュにヒットすればネットワークアクセスしない。
//
// 取得中はずっとロックを保持する。並行リクエストはそれぞれ GitHub に問い合わせる
// のではなく、同じ問い合わせ結果を待つ（ページ読み込み直後に複数タブが同時に
// 問い合わせるのが、レート制限を最も招きやすい瞬間だから）。
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// リクエストのキャンセル（ユーザーがタブを閉じた）は GitHub の問題ではないので
	// キャッシュに書かない。さもないと次の訪問者が意味不明な「キャンセル済み」エラーを受け取る。
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at = rel, err, time.Now()
	return rel, err
}

// updateProgress はフロントに送る 1 件の進捗。
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // ダウンロード段階でのみ意味を持つ。それ以外は -1
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub は 1 回のアップグレードの進捗を保持し、SSE 購読者にブロードキャストする。
//
// running は同時に排他制御も兼ねる。アップグレード中に再度 POST /api/update/apply
// すると即座に 409 を返し、2 つの goroutine が同時に同じ artex.new へ書き込むのを防ぐ。
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin はアップグレード権限を奪取する。すでに進行中なら false を返す。
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: "準備中…", Version: version}
	h.fanout(h.cur)
	return true
}

// finish は 1 回のアップグレードを終える。err が nil ならステージ成功で、再起動待ち。
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: "更新に失敗しました", Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: "新バージョンの準備ができました。再起動しています…", Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout は h.mu を保持したまま呼び出すこと。購読者 channel にはバッファがあり、
// 溢れたら捨てる——進捗は捨ててよい一時情報であり、詰まった SSE 接続が
// アップグレード自体をブロックしてはならない。
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck は GitHub 上の最新正式版を問い合わせ、現在のバージョンと比較する。
//
// フロントも api.github.com に直接接続できる（GitHub の CORS は *）が、**この API を正とする**。
// ダウンロードはバックエンドが行うので、バックエンドが GitHub に到達できて初めて
// 更新が成立する。ブラウザからは繋がるのにサーバーからは繋がらない状況はよくあり
// （サーバーがイントラネット内、あるいはプロキシがブラウザにしか設定されていない等）、
// その場合更新を押しても必ず失敗するので、確認の段階で正しくエラーを返したほうがよい。
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}

	// トップバーの通知はキャッシュを使う（既定）。ユーザーが「更新を確認」を押したときは
	// force=1 で強制的にオリジンへ問い合わせる。
	force := r.URL.Query().Get("force") != ""
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// 開発ビルド（dev / git describe にサフィックス付き）には比較可能なバージョン番号がない。
		// 許可するとローカルでデバッグ中のバイナリを正式版で上書きしてしまうため、更新を許可しない。
		out["reason"] = fmt.Sprintf("現在のバージョン %q は正式リリース版ではないため、ワンクリック更新は無効です", current)
	}
	writeJSON(w, 200, out)
}

// updateApply は新バージョンをダウンロードしてステージし、完了後にプロセスを終了して
// 監視スクリプトに再起動を委ねる。
//
// すぐに 202 を返し、実際の処理はバックグラウンドの goroutine で実行する。パッケージ全体の
// ダウンロードには数分かかることもあり、リクエストに紐づけるとリバースプロキシのタイムアウトで
// 切られてしまう。進捗は /api/update/stream で配信する。
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion

	// キャッシュ経由で取得する。これにより、インストールされるのはユーザーが UI 上で見て
	// 確認したバージョンそのものであることが保証される。
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf("現在のバージョン %q は正式リリース版ではないため、ワンクリック更新は無効です", current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf("すでに最新バージョン %s です", current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, "更新がすでに進行中です")
		return
	}

	go func() {
		// 意図的にリクエストの ctx ではなく s.ctx を使う。HTTP レスポンスが返ると
		// リクエストは終了し、それに紐づけたダウンロードは即座にキャンセルされてしまう。
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] 更新に失敗しました：%v", err)
			return
		}
		log.Printf("[update] %s → %s をステージしました。差し替えのため間もなく終了します", current, rel.TagName)
		// 最後の進捗をフロントに届ける時間を少し取り、それから終了をトリガーする。
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback は前のバージョンへ手動で戻す（差し替え前にバックアップした artex.old）。
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, "更新が進行中のためロールバックできません")
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] 手動で前のバージョンへロールバックしました。切り替えのため間もなく終了します")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// updateStream は更新進捗を SSE で配信する。
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// まず現在の状態を 1 件送り、ページ更新後すぐ進行中のアップグレードが見えるようにする。
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
