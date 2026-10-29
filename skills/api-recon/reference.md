# api-recon — リファレンスマニュアル

Grep レシピ、`config.json` テンプレート、トラブルシューティング。すべての grep は `js/` ディレクトリに対して実行する。bundle が 1 行の場合は先に `js-beautify` または `sed 's/}/}\n/g'` を使うとよいが、通常はコンテキストウィンドウ付きの raw grep で十分。

## スクリプトの説明

`scripts/` 内のすべてのファイルは**参考テンプレート**であり、実行前に必ず対象サイトに合わせて調整する。よくある変更ポイント：

| スクリプト | よく調整が必要な項目 |
|---|---|
| `harvest_static.py` | endpoint 正規表現、webpack/Vite manifest の解析、マイクロフロントエンドの publicPath、リトライ/並列度 |
| `runtime_harvest.js` | neutralize のフィールド名と成功値、stub のマッチルールと body 構造、routes の供給元、WS 記録、`waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`、L1 stubs、`neutralize.fields`、`apiPattern`、L3 の有効化可否、`recordDetail`、`observe.*`、`neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` の破壊的リンク、cookie、depth/max、同一ドメインフィルタ |
| `extract_route_map.py` | `routeMap` / `routeLink` 正規表現、KEY の命名パターン |
| `build_perm_tree.py` | `userRouteAuth` の解析、`ROOTS`/`PREFIX_PARENT` の階層ヒューリスティック、stub の外側フィールド名 |
| `config.json` | 上記すべてのサイト固有パラメータの統一入口 |

調整後のファイルはタスクの作業ディレクトリ（例 `recon/`）に置き、レポートに参考スクリプトからの具体的な変更点を記載することを推奨する。

---

## A. 逆解析の 3 つのゲート

### A1. 描画ゲート — 「ログイン済みをどう判断するか？」

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

チェーン `isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))` を探し、**ストレージキー**、**コンテナ**（Cookie か localStorage か）、**エンコード**を特定する：

| エンコード | config での偽造方法 |
|---|---|
| 平文文字列 / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | 署名なし/`alg:none` の JWT、または bundle 内の鍵で署名 |
| 暗号化（SM2/AES/RSA） | ハードコードされた鍵を探す。描画ゲートにデコード可能な blob だけで足りるなら forge 可能。そうでなければ静的解析でフォールバック |

→ `cookies` / `localStorage` に書き込む。

### A2. インターセプタゲート — 「何が /login への遷移をトリガーするか？」

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(未登录|请重新登录|登录已过期|unauthorized|登录失效|授权|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

**フィールド名**、**成功値**（通常 `0` または `200`）、**遷移をトリガーする失敗値**を特定する。junk セッションで検証する：

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ `neutralize.fields` + `neutralize.success` に書き込む。

### A3. コンテンツゲート — 「メニュー/権限はどこから来るか？」

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**2 層のデータ**（よくある企業向け管理画面）：

| API | 典型的な payload | 消費側 |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | ルートガード、ボタン単位の ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | サイドバーメニューの描画 |
| bundle 内の `userRouteAuth` | `{ CODE: { url, name? } }` | code → フロントエンド path |
| bundle 内の `routeMap` | `{ KEY: { name, link } }` | 別名の解決（webpack `o.DASHBOARD`） |

消費側のコードを読んで確認する：`getResultTree(tree, permissions)` がどうフィルタするか、`v-if` / `hasAuth(code)` がどのフィールドをチェックするか。

**手動 forge**（小規模サイト）：permissive な payload を構築 → `stubs`。

**完全な権限ツリーの復元**（大規模サイトでサイドバー/サブモジュールがまだ空白）：**I 節**を参照。

---

## B. config.json テンプレート

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

フィールドの説明：
- `runtimeMode`：`depth`（Puppeteer）、`coverage`（browser MCP）、`both`
- `cookies[].value` の接頭辞：`b64json:` → base64(JSON)。`json:` → 生の JSON。接頭辞なし → リテラル
- `forward: true` は実際のリクエストを転送してコードフィールドを書き換える。`false` は完全オフラインの stub
- `mockTier`：coverage モードで preload が有効化する階層。例 `L1+L2`、`L1+L2+L3`
- `routes` は `routes.txt` から取得。メニューを forge すると harness が `<a href>` を自動で追加する
- `captureResponses` / `recordWs` は depth モードでのみ有効
- `waitUntil`：大規模 SPA では `domcontentloaded` を使い、`networkidle2` でのハングを避ける
- `routeTimeout`：単一ルートの `page.goto` タイムアウト（ミリ秒）
- `proxy`：Puppeteer の `--proxy-server`。`HTTP_PROXY` / `HTTPS_PROXY` でも設定可能

### B1. 2 つの stub テンプレート（role_permissions + permissions/all）

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

外側のフィールド名（`response_code` / `code` / `data`）は A2 のインターセプタゲートと一致していること。`permissions` は tree 内のすべての leaf code をカバーしていること。

---

## C. coverage モード：preload 設定

`scripts/preload.js` 先頭の `CONFIG` オブジェクトを編集するか、CDP で注入する前に置き換える：

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* config.json の stubs と同じ */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

検証：`window.__API_RECON_PRELOAD__ === true` かつ pathname が安定していること。

記録結果のエクスポート：

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime Hook の機能

preload（coverage）と runtime_harvest（depth）に組み込まれたブラウザ Hook 機能とそのカバレッジ：

| Hook 機能 | API 発見への価値 | カバレッジ |
|---|---|---|
| Hook fetch / XHR.open | リクエスト URL/メソッドを記録 | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | Authorization などのヘッダを発見 | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie の読み取り | セッションキー名を確認 | ⚠️ 任意 `observe.storageReads/cookieReads` |
| Vue のルート取得 | frontendRoutes を補完 | ✅ `__API_RECON_ROUTES__`（読み込み済みルート） |
| Vue ルートガードの中和 / ログイン遷移の遮断 | モジュールを広げて API をトリガー | ✅ `neutralizeVueRouter` + ネイティブ遷移の中和 |
| React のルート取得 | ルートを補完 | ⚠️ 静的 + クリック。専用 Hook なし |
| ページ遷移の遮断（ログイン path） | ページに留まって分析 | ⚠️ ログイン path のみ遮断し、業務ナビゲーションを妨げない |
| 暗号化ライブラリの Hook（CryptoJS/SM など） | 暗号化パラメータ → 平文の API body | ❌ 暗号化関数の入力を手動で Hook する必要あり。結論は config に記載 |
| アンチデバッグの bypass | ないと runtime で API を記録できない | ❌ 手動対応が必要。静的解析は引き続き利用可能 |

---

## E. Endpoint 抽出の正規表現（静的が少なすぎる場合）

`harvest_static.py` の `extract_endpoints` を緩めるか、手動で：

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. トラブルシューティング

| 現象 | 原因 → 対処 |
|---|---|
| 静的 API が少ない | endpoint の方言が一致しない → 正規表現を緩める（D 節） |
| chunk 数 ≪ manifest | CSS のみ、または未デプロイの chunk。404 はリトライ済み |
| runtime でもログインページが表示される | 描画ゲートの誤り → A1 を再確認：キー名、コンテナ、エンコード、domain |
| シェルには到達するがモジュールが空白 | コンテンツゲート → メニューを forge（A3）。`routes` の path が誤っている可能性 |
| 各ルートで bootstrap/locale のみ | 権限コードが不完全 → I 節の権限ツリー復元。`role_permissions` + `permissions/all` の2 つの stub を確認 |
| サイドバーに項目はあるが子ページが空白 | tree に intermediate ノードがない、または code が `userRouteAuth` と一致しない |
| すべての API でログインへ飛ぶ | インターセプタゲート → `neutralize` を確認。ネストしたフィールドは walk ロジックの拡張が必要 |
| WS フレームが 0 | ユーザー操作後にしか subscribe しない。`perRouteMs` を延長 |
| レスポンスボディが空 | `forward: true` のときのみ実際のレスポンスがある |
| Chromium がない | chromium をインストールするか、`config.chromium` / `CHROMIUM` を設定 |
| Mock が多いのにログインへ戻る | Hook が遅すぎる、または `location.href` の setter がない → document-start + preload |
| 一覧がすべて空 | L3 の空配列は正常。タブ/設定/詳細をクリックし続ける |
| Redux の action をルートと誤認 | get/set/change/clear/toggle/upload を含む内部 path を除外 |
| Vue が依然としてログインへ飛ぶ | preload が document-start でない → 注入タイミングを変更。または `neutralizeVueRouter: false` のときはガードを手動で消す |
| レスポンスに URL があるが log に入らない | `extractUrlsFromResponse` を有効化。または `__API_RECON_DETAIL__` から手動抽出 |
| Authorization ヘッダ名が不明 | `observe.xhrHeaders` を有効化するか、DevTools でリクエストヘッダを確認 |
| runtime が極端に遅い / タイムアウト | `waitUntil: domcontentloaded` に変更。`routeTimeout` を下げる。`networkidle2` を使わない |
| プロキシ接続に失敗 | `proxy` / 環境変数を確認。Puppeteer と curl のプロキシポートを一致させる |

---

## G. hardened なターゲット

サーバー側でセッションを段階的に検証する場合（forge 不可の署名付き cookie、サーバー側レンダリングで stub 不可のメニューなど）、runtime はシェルで止まる。想定される挙動：

- **静的解析だけで endpoint の列挙は可能** — モジュールの path はコード内にある
- 許可があれば、**実際のセッション**で同じ harness を実行する：`forward: true`、neutralize 不要で、実際の methods/params/responses を取得できる

---

## H. 単一タスクのチェックリスト

1. 許可範囲を確認する
2. `scripts/harvest_static.py` を**読み** → 対象に合わせて調整 → 実行 → `api_static.txt`、`routes.txt` を確認
3. **Phase 1b**：path アンカーの拡張ウィンドウ + バインディング層 → `param_candidates.json`（J 節）
4. A1/A2/A3 を逆解析 → サイト固有の `config.json` を書く
5. `runtime_harvest.js` / `preload.js` を**読み、調整**してから実行する
6. `runtimeMode=depth`：`npm install` → 調整後の harvest スクリプトを実行
7. `runtimeMode=coverage/both`：document-start で調整後の preload を注入 → browser MCP で動的列挙 + **パラメータトリガーマトリクス**
8. モジュールが描画されない → **I 節の権限ツリー復元** → stub を patch → 再実行
9. パラメータの複数サンプル diff + エラーからの逆推 → `params_merged.json`
10. マージ → `site_map.json` + `api_merged.txt`。カバレッジ、ギャップ、スクリプトの変更点を正直に明記する

---

## I. 権限ツリーの復元（Phase 4 の深化）

`menus: [{ path, show: true }]` という単純な forge が効かず、サブモジュールがまだ mount されない場合に使う。

### I1. auth モジュールの特定

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

記録する：**権限 API の path**、**レスポンスのフィールド名**、**消費する chunk のファイル名**。

### I2. routeMap の抽出

```bash
python3 scripts/extract_route_map.py recon/js recon/
# 生成物 recon/route_map.json
```

`[!] no routeMap pattern found` が出た場合：`extract_route_map.py` 内の正規表現を緩めるか、手動で grep する：

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. 権限ツリーの構築 + stub

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

スクリプトのロジック：
1. `userRouteAuth={MONITOR:{url:...},...}` を解析（webpack の別名 `He=o.DASHBOARD` を含む）
2. `route_map.json` で alias → 実際の path を解決
3. code の接頭辞から parent を推測（`MONITOR_ALERT` → `MONITOR`）
4. `permissions_tree.json`、`permissions_all_stub.json`、`role_permissions_stub.json` を出力
5. `--config` 指定時は `config.json` の `stubs` と拡張 `routes` に自動書き込み

**対象に合わせた調整**（スクリプト先頭）：
- `DEFAULT_ROOTS`：トップレベルモジュールの code リスト
- `DEFAULT_PREFIX_PARENT`：`PREFIX_` → parent のマッピング
- `DEFAULT_EXTRA_PARENT`：接頭辞関係にない orphan ノード

### I4. stub の一貫性の検証

```bash
# permissions の数は ≈ userRouteAuth のエントリ数であるべき
wc -l recon/perm_codes_all.txt
# routes は route_map のすべての link をカバーすべき
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. runtime の再実行と比較

```bash
node recon/runtime_harvest.js recon/config.json
# forge 前後で runtime_api.json の件数を比較。/attack、/asset などにモジュール API が現れるか確認
```

| forge 前 | forge 後（成功） |
|---|---|
| 各ルートで同じ 3〜5 件の bootstrap | ルートごとに異なる module API がトリガーされる |
| `/api/locale/language` のみ | `/api/web/...` のようなモジュール endpoint が現れる |
| `routes.txt` が数件のルート | `routes` が route_map 由来の 80〜110+ になる |

### I6. それでも失敗する場合

- **coverage モード**：サイドバー + タブをクリックする。権限 gating は操作後に初めてリクエストする可能性がある
- **stub フィールド**：実際の API（curl + 実セッション）と stub のネスト構造を比較する
- **追加のガード**：`hasPermission|checkRole|func.` などのボタン単位チェックを grep し、`role_permissions.permissions` を拡張する
- **静的解析でフォールバック**：モジュール API の path は `api_static.txt` に残っており、runtime は METHOD/body を補うだけ。パラメータは `param_candidates.json` + 記録済みサンプルとして保持する

---

## J. パラメータ逆解析（Phase 1b / 5b / 5c）

**方法論であり、汎用スクリプトではない。** path の探索は正規表現。パラメータの探索はアンカー拡張ウィンドウ + UI バインディングチェーン + 複数サンプル diff + エラーからの逆推。

### J1. アンカー拡張ウィンドウ — path から組み立てオブジェクトを探す

```bash
# Phase 1 で判明した path をアンカーとする
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. ラッパー層と転送形態

```bash
# axios / 統一 request
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# パスパラメータ
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. 検証ゲート — 必須 / 形式 / 列挙

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. バインディング層 — フォーム → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

runtime での補完：DevTools → Network → リクエスト → **イニシエータ**（call stack）で `fetch`/`send` から上へ組み立て関数を追う。

### J5. 暗号化パラメータ

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**暗号文からフィールドを推測しない** — 暗号化関数の**引数**を Hook し、暗号化前の平文 payload を記録する。結論は `config.json` / `param_candidates.json` に書く。

### J6. パラメータトリガーマトリクス（Phase 3 で必須）

各モジュールで操作ごとに 1 回ずつ記録し、リクエストの body/query を diff する：

| 操作 | 注目点 |
|---|---|
| 一覧の初回画面 | ページングの既定値 |
| 検索 | keyword、filters |
| 高度なフィルタ | optional フィールド |
| 新規作成/編集 | 完全な entity |
| 一括/エクスポート | `ids[]`、`exportType` |
| 並び替え/ページ送り | `sortField`、`order` |

産出 `param_samples.json`：`[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. 信頼度のルール

| 信頼度 | 条件 |
|---|---|
| **高** | 静的な callsite + runtime で 2 サンプル以上が一致 |
| **中** | 静的なみ、または runtime が 1 回のみ |
| **低** | レスポンス/エラーからの逆推で、二重検証なし |
| **トリガー待ち** | 静的に既知のフィールドだが、UI/権限が到達していない |

### J8. シナリオ別の手順

| シナリオ | 順序 |
|---|---|
| REST の一覧ページ | J1 で組み立てオブジェクト → J6 で 4 回 diff → J3 で rules |
| 新規作成/編集フォーム | J3 で Form name → J4 で submit チェーン → runtime で送信し、わざと空にして 400 を見る |
| GraphQL | J2 で variables の宣言 → runtime で各 operation の variables を記録 |
| 暗号化 body | J5 で引数を Hook → 暗号化前のフィールドが実際の params |

### J9. api-recon の段階との対応

| api-recon | パラメータ recon |
|---|---|
| Phase 1 静的 | J1 アンカー拡張ウィンドウ |
| Phase 2 A2 インターセプタ | グローバル注入フィールド（tenantId、sign） |
| Phase 3 runtime | J6 トリガーマトリクス + `param_samples.json` |
| Phase 4 権限ツリー | モジュールごとにフォームが異なる → 権限が足りて初めて全フィールドをトリガーできる |
| Phase 5 マージ | `params_merged.json` + 信頼度。単一サンプルで必須を確定しない |

### J10. トラブルシューティング

| 現象 | 対処 |
|---|---|
| 静的にフィールド名があるが runtime に一度も現れない | 「トリガー待ち」と明記。権限ツリーを補う / 高度なフィルタをクリックする / 連動 select の各 option を選ぶ |
| 同じ path で body の形状が異なる | 正常 — `action` ごとに分けて記録し、schema を無理にマージしない |
| stub のレスポンスは偽物だが params を見たい | **outbound リクエスト**の body/headers を見る。stub のレスポンスから逆推しない |
| 400 で nested field が報告される | 外側のラッピング `data`/`bizData`/`variables` に注意 |
| GraphQL で operation 名しか見えない | `variables` の JSON を展開。静的で `$var: Type` を探す |

---
