package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Repo はリリース元。設定項目にせずハードコードする：更新元を設定可能にすることは、
// 設定を変更できる者すべてにリモートコード実行の経路を与えることに等しく、
// ペネトレーションテストプラットフォームでは開けてはならない穴である。
const Repo = "Autumn-27/artex"

// latestURL は GitHub の「最新正式版」API。prerelease と draft は自動的にスキップされる。
const latestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// allowedHosts はアップグレード経路がアクセスできるドメインを限定する。以下の
// checkRedirect と組み合わせ、リダイレクトのどの段階でもリスト外ホストへ
// 飛ばされれば即失敗する——DNS 汚染 / 中間者攻撃によるバイナリすり替えを防ぐ
// 第一の関門であり、第二の関門は SHA256SUMS の照合である。
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // release アセットが実際に配置されるオブジェクトストレージ
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release は GitHub Release のうち本アプリが扱うフィールド。
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset は Release に添付された 1 つのファイル。
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// NewClient は GitHub ドメインのみを認める HTTP クライアントを構築する。proxy が空なら
// 直接接続する。
//
// デフォルトの Transport は意図的に再利用しない：アップグレード経路は必ず TLS を
// 強制し証明書を検証する必要があり、他所で設定された InsecureSkipVerify などの
// 影響を受けてはならない。
func NewClient(proxy string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // パッケージ全体をダウンロードするため、リクエスト単位のタイムアウトで打ち切られないようにする
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("リダイレクト回数が多すぎる")
			}
			return checkURL(req.URL)
		},
	}
}

// checkURL は https とドメインホワイトリストを強制する。
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("HTTPS 以外のアドレスを拒否: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("GitHub 以外のドメインを拒否: %s", u.Hostname())
	}
	return nil
}

// FetchLatest は最新の正式版を取得する。
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub へのアクセスに失敗（システム設定でグローバルプロキシを設定可能）: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		// 未認証の GitHub API は IP あたり 1 時間 60 回で、出口 IP を共有していると
		// すぐに上限へ達する。
		return nil, fmt.Errorf("GitHub API のレート制限（1 時間 60 回）。しばらくしてから再試行してください")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("リポジトリ %s はまだ正式版を 1 つもリリースしていない", Repo)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub が %d を返した", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("Release の解析に失敗: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("Release に tag がない")
	}
	return &rel, nil
}

// AssetName は現行プラットフォームに対応するリリースパッケージ名を返す。build.sh の
// package_binary と一致させる：artex-<バージョン>-<os>-<arch>.zip
// （バージョン番号には v プレフィックスを付けない）。
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset は Release 内を名前で検索してアセットを探す。
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
