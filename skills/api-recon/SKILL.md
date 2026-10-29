---
name: api-recon
description: Web サイトの API エンドポイントを収集するときにこの skill を呼び出す。
---

# API Recon（フロントエンド API 偵察）

**許可済み**であることを前提に、次のものをできるだけ完全に発見する：**バックエンド API**（パス、メソッド、パラメータ、レスポンスボディ）、**フロントエンドルート**、**UI 機能のトリガーポイント**（タブ、ダイアログ、テーブル操作など）。

---

## 境界と禁止事項（Agent 必読 · 違反は境界逸脱）

本 skill は**API / パラメータ面の偵察のみ**を行い、脆弱性探索やペネトレーションのエクスプロイト段階ではない。

### タスクの境界

| 範囲 | 許可 | 禁止 |
|---|---|---|
| **対象** | path、method、パラメータ、ルート、UI トリガーポイントの列挙 | SQLi/XSS/権限逸脱/ブルートフォース/fuzz による脆弱性、パケット改ざん攻撃、破壊的操作 |
| **認証** | Hook + stub/mock で**クライアント側**のログインゲートを回避 | ユーザーにアカウント/パスワードを要求したり推測したりすること。実際のログインフォームへの送信 |
| **ランタイム** | 認証情報なしでインターフェースを hook し、mock レスポンスで SPA をログイン後のシェルへ進める | 実際のバックエンドセッションがないと続行できないフロー |

### 認証情報なしの動的解析（Phase 3 の既定）

1. `preload.js` / `runtime_harvest.js` でログイン、権限、メニューなどの bootstrap インターフェースを**インターセプトして stub** する。
2. 業務クエリのインターフェースには**構造が正しく、業務コードが成功で、データは空でもよい** mock body を返す。
3. バックエンドがない、または 401 の環境でもフロントエンドがログイン後のページを描画できるようにし、より多くの XHR/fetch/WebSocket をトリガーする。
4. **空のデータ、空のテーブル、プレースホルダ UI はいずれも想定どおり**——これを理由に実際のログインや脆弱性テストへ移らないこと。

**一言でいえば**：mock でフロントエンドのルートとコンポーネントのマウントを広げ、**outbound リクエストだけを記録する**。バックエンドが何を返すかは重要ではなく、重要なのはフロントエンドが**さらにどのインターフェースを送るか**である。

### フローの厳禁事項

| 禁止 | 代替手段 |
|---|---|
| Phase 1 完了前に grep/curl/Read でメイン entry `index-*.js` から API path を抽出する | `OUTDIR/harvest_static.py` を実行する |
| `extract_apis.py` などを手書きして harvest の代わりにする | `OUTDIR/harvest_static.py` を修正して再実行する |
| 同じ grep/コマンドが 2 回以上失敗しても繰り返す | 戦略を変える：tool_logs を読む、harvest を修正する、reference を調べる |
| ゲート A/B を飛ばして `scripts/` の原版を直接実行する | OUTDIR にコピーし、対象に合わせて修正する |
| 実際のユーザー名/パスワード、OTP、OAuth などの認証 | stub/mock（上記参照） |
| 「実データを取る」を理由に stub を飛ばし、権限逸脱/インジェクションテストを行う | outbound の記録のみ。recon の境界内 |
| 削除、機密データのエクスポート、一括書き込みなどの不可逆操作 | coverage のクリックも同様 |
| runtime + 動的列挙を完了せずに、全ページとインターフェースを取得したと称する | 「完了定義」を参照するか局限を明記する |
| パラメータトリガーマトリクス + diff を完了せずに、全パラメータを把握したと称する | Phase 3b マトリクス + Phase 5 diff |
| 単一の runtime サンプルから必須/任意を推測する | 複数サンプルの diff、または検証ルール/エラーからの逆推 |

---

## 2 層モデル + 実行モード

| 層 | 産出 | 上限 |
|---|---|---|
| **静的**（JS bundle） | 全エンドポイントパス、ルート草案、パケット組み立て箇所のフィールド候補 | HTTP メソッドなし。パラメータは Phase 1b が必要。実行時に連結される URL は漏れる |
| **ランタイム**（生きたセッション） | メソッド + body + レスポンス + 動的 URL + WS/SSE。複数サンプルの diff でパラメータを補完 | ページが実際に描画されて初めてリクエストが出る。単一サンプルでは必須/任意を確定できない |

| 実行モード | エンジン | 適用 |
|---|---|---|
| **depth** | `runtime_harvest.js`（Puppeteer） | API 一覧、METHOD/params/レスポンスボディ、WS/SSE、再現可能な一括実行 |
| **coverage** | browser + `preload.js` | タブ/ダイアログ/テーブルをクリックし、機能ポイントのカバレッジをより深く |
| **both** | 先に depth、次に coverage | 最も完全、所要時間も最長 |

**パラメータの方法論**（汎用スクリプトなし）：path は harvest/正規表現。パラメータは **アンカー拡張ウィンドウ + UI バインディングチェーン + 複数サンプル diff + エラーからの逆推**（grep レシピは [reference.md](reference.md) の J 節）。

---

## 完了定義

以下をすべて満たして初めて recon 完了を宣言できる：

- [ ] **静的**：Phase 1 の harvest が `api_static.txt`、`routes.txt`、`js/` を産出
- [ ] **ランタイム**：少なくとも depth か coverage のいずれか。coverage/both は **Hook 有効 + 動的列挙ループ** が必要
- [ ] **シェル到達**：業務 path へのアクセス時に `/login` でないこと（hash ルートに注意）
- [ ] **パラメータ**：coverage/both はパラメータトリガーマトリクス + `param_samples.json` を完了。Phase 5 で `params_merged.json` をマージ
- [ ] **深さ**（モジュールページが空白の場合）：Phase 4 で権限ツリーを復元して再実行し、**module レベルの API**（locale/bootstrap のみでない）が現れるまで続ける
- [ ] **納品**：Phase 5 の産出が揃っている（Phase 5 の産出表を参照）。`insert_assets` でサービスとエンドポイントの資産を書き込む

---

## スクリプトとゲート

`scripts/` は参考テンプレートにすぎず、原版を直接実行して最終結果とすることを**禁止**する。

**ルール**：まず読む → 対象に合わせて修正 → `OUTDIR`（例 `recon/`）に書き込む → `CHANGES.md` に記録する。合わない場合は方法論に従って書き直し、構造だけを借りる。

| ゲート | いつ | 参考スクリプト → OUTDIR コピー | よくある必須修正 |
|---|---|---|---|
| **A（静的）** | Phase 0 後、**最初に** harvest/spider を実行する前 | `harvest_static.py` / `spider_mpa.py` | **多くのサイトでは既定の regex でそのまま実行可能**。manifest/方言が合わない場合のみ endpoint 正規表現、webpack/Vite の `publicPath`、MPA の exclude/cookie を修正 |
| **B（ランタイム）** | Phase 2 後、depth/coverage を実行する前 | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage キー、neutralize の成功値、stubs、login 正規表現、api プレフィックス、hash/history |

**SPA の強制順序**（入れ替え不可。Phase 番号は「まず探索してからスクリプト」より優先）：

| ステップ | 必須 | 禁止 |
|---|---|---|
| Phase 0 完了後 | 次の Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read でメイン entry `index-*.js` を読む（通常 >500KB） |
| ゲート A | スクリプトをコピー → 必要に応じて小修正 → **即座に実行** | 先に手作業で API を抽出してから harvest するか決める |
| Phase 1 完了前 | `wc -l` で産出を検証。404 なら harvest を修正してリトライ | extract スクリプトの手書き。未ダウンロードの URL への繰り返し grep |
| Phase 1b 以降 | grep は `OUTDIR/js/*.js` のみ | メイン bundle を harvest の代わりに使う |

- ✅ `harvest_static.py` をコピー →（任意で）regex を修正 → **即座に実行**
- ❌ メイン bundle を curl → 複数回 grep → 一時的な extract を書く → 最後にようやく harvest
- **MPA**：Phase 0 後の次の Bash = `python3 OUTDIR/spider_mpa.py ...`

---

## ツールと出力の制約

| 制約 | 説明 |
|---|---|
| 大きなファイル | >100KB の `index-*.js` を Read/grep でコンテキストに入れることを**禁止**。OUTDIR のスクリプトでバッチ処理する |
| grep 出力 | 必ず `\| head -20` または `-m 5` を付ける。対話には path のサマリだけを残し、bundle の断片を貼らない |
| 検証 | `wc -l`、`ls \| wc -l` を使う。ディレクトリ全体を Read しない |
| regex の初探 | 任意、≤1 回、≤50KB の小さな chunk または HTML のみ。正式な静的解析は harvest を基準とする |
| reference | レシピ/テンプレート/トラブルシューティングは [reference.md](reference.md)、全文を inline で重複させない |

---

## 実行ロードマップ

```
Phase 0 分類 + OUTDIR
  → ゲート A → Phase 1 harvest（★ 即座に実行 ★）
  → Phase 1b パラメータ逆解析
  → Phase 2 認証の 3 つのゲート → config.json
  → ゲート B → Phase 3 ランタイム + パラメータマトリクス
  → Phase 4 権限ツリー（必要時）→ Phase 3 を再実行
  → Phase 5 マージレポート + insert_assets で発見した全サービス、エンドポイント API 資産を一括挿入。いずれにせよ挿入時に発見済み資産を漏らしてはならない
```

順にチェックする。**前の項目が未完了なら次の Phase に進んではならない**。

1. [ ] **Phase 0**：SPA/MPA の初探。`OUTDIR` を作成 → [Phase 0](#phase-0--分類)
2. [ ] **ゲート A + Phase 1**：スクリプトをコピー → **即座に** harvest → `wc -l` で検証 → [Phase 1](#phase-1--静的)
3. [ ] **Phase 1b**：アンカー拡張ウィンドウ + バインディング層 → `param_candidates.json` → [Phase 1b](#phase-1b--パラメータ逆解析)
4. [ ] **Phase 2**：認証の 3 つのゲート → `config.json` → [Phase 2](#phase-2--認証の-3-つのゲート)
5. [ ] **ゲート B**：runtime スクリプトを調整 → [Phase 3](#phase-3--ランタイム)
6. [ ] **Phase 3**：depth / coverage / both。シェル到達を確認。パラメータトリガーマトリクス → `param_samples.json`
7. [ ] **Phase 4**（必要な場合）：権限ツリー → stub を patch → Phase 3 を再実行 → [Phase 4](#phase-4--権限ツリーの復元)
8. [ ] **Phase 5**：産出のマージ + レポート + `insert_assets` → [Phase 5](#phase-5--マージとレポート)

---

## Phase 0 — 分類

エントリ HTML を取得し、**`OUTDIR` を作成する**（skill 内の `scripts/` は変更しない）：

- **SPA**：空のシェル + `<div id=app>` + chunk → Phase 1〜5
- **MPA**：SSR + `<form>`、endpoint bundle なし → ゲート A の後：

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

`forms.txt`、`links.txt`、`api_inline.txt` を産出。SPA で forms ≈ 0 なら → Phase 1 へ。

---

## Phase 1 — 静的

[スクリプトとゲート](#スクリプトとゲート) · [ツールと出力の制約](#ツールと出力の制約)に従う。

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest：HTML script の解析 → webpack/Vite manifest → すべての lazy chunk をダウンロード → `js/`、`api_static.txt`、`routes.txt`、`chunkmap.txt` を産出。

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk 数 vs manifest：404 なら harvest を修正してリトライし、手動 curl で 1 つずつ chunk を取得しない
- `api_static.txt` が少なすぎる → OUTDIR 内の endpoint 正規表現を緩めて再実行（reference 参照）

### Phase 1b — パラメータ逆解析

path は Phase 1 から得る。パラメータのフィールドは別途 recon が必要。grep のルールは[ツールと出力の制約](#ツールと出力の制約)を参照。

**完了基準**：重要なインターフェースについて答えられること——フィールド名、転送位置、型の推測、必須か否か、サンプル値、信頼度。

#### 1b.0 — 転送形態

| 形態 | パラメータの位置 | 静的で優先して見るもの |
|---|---|---|
| REST JSON | body + query | path アンカーの近くの `(params\|data\|body)\s*:\s*\{` |
| GraphQL | `variables` | gql テンプレート、`$page: Int` |
| 従来型 form | urlencoded | `<form>`、`FormData` |
| ファイルアップロード | multipart | `FormData.append` |
| パスパラメータ | `/user/:id` | ルートテーブル + `useParams` / `$route.params` |
| 暗号化/署名 | `sign`/`data` に包む | 暗号化関数の入力を Hook（reference D 節） |

産出：各インターフェースに `transport: query|json|form|graphql|encrypted` を付記。

#### 1b.1 — アンカー拡張ウィンドウ

既知の path をアンカーとして、ウィンドウを広げてパケット組み立てオブジェクトを探す：

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| ラッパー層 | パラメータの手がかり |
|---|---|
| axios インスタンス | `data` / `params` |
| 統一 request | インターセプタがグローバルフィールドを注入 |
| OpenAPI クライアント | 生成された method シグネチャ |
| React Query / SWR | hook の第 2 引数 |
| Vue composable | composable の引数 |

型の痕跡：`yup`/`zod`/rules、`Form.Item name=`、埋め込み Swagger。

→ `param_candidates.json`：`{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — バインディング層

```
Form field → onFinish/handleSubmit → transform → API payload
```

| バインディング元 | 手法 |
|---|---|
| フォーム submit | submit → transform → API を追う |
| テーブル検索 | `getFieldsValue()` → `params` |
| ルート | `:id` / `?tab=` |
| インターセプタ | グローバルの `tenantId`、ページング、sign |
| 列挙 select | `options` → API の列挙値 |

DevTools のコールスタックで `fetch`/`XHR.send` から上へパケット組み立て関数を追う。

#### 1b.3 — パケット組み立ての 3 問（≠ Phase 2 の認証 3 ゲート）

| 問い | 答えるべきこと |
|---|---|
| **組み立て** | payload はどこで build されるか、transform の痕跡 |
| **検証** | required、pattern、enum |
| **転送** | path / query / body / multipart / ヘッダ |

インターセプタゲート（Phase 2）ではついでにグローバル注入フィールド（Authorization、`X-Tenant-Id`、sign）を読む。

#### 1b.4 — Phase 3 との接続

候補フィールドは静的/バインディング層から得る。**必須/任意/条件依存**は Phase 3 のパラメータマトリクス + diff + Phase 5 のエラーからの逆推が必要。

---

## Phase 2 — 認証の 3 つのゲート

`OUTDIR/js/` を grep し（`head` 付き）、`config.json` に書き込む（レシピは reference）：

| ゲート | 問い | キーワード |
|---|---|---|
| **描画ゲート** | ログイン済みをどう判定するか？ | `isLogin`、`getToken`、Cookie/localStorage |
| **インターセプタゲート** | 何が `/login` へのジャンプをトリガーするか？ | `response_code`、`errno`、axios interceptor |
| **コンテンツゲート** | メニュー/権限はどこから来るか？ | `menu`、`permission`、`role`、`acl`、`routes` |

localStorage のキー名を認証情報と見なすことを禁止——chunk/リクエストチェーンから確認する必要がある。

**出口 = ゲート B**：結論を `config.json` に落とし、`OUTDIR/runtime_harvest.js` / `preload.js` を修正する。

### Phase 2b — API 観察（任意）

OUTDIR 内の `preload.js` でセッションキー名、Authorization、ネストした API URL を確認する：

| 設定 | 産出 |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | headers の観察 |
| `extractUrlsFromResponse: true` | レスポンス内の子 API |
| `observe.storageReads/cookieReads: true` | config へ反映 |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

coverage は毎ラウンドでエクスポート：`__API_RECON_LOG__`、`__API_RECON_DETAIL__`、`__API_RECON_ROUTES__`、`__API_RECON_OBSERVE__`。

---

## Phase 3 — ランタイム

ゲート B を通過済みであること。[境界と禁止事項](#境界と禁止事項agent-必読--違反は境界逸脱) · 認証情報なしの mock 戦略に従う。

`config.json` で `"runtimeMode": "depth" | "coverage" | "both"` を設定（テンプレートは reference）。

### Hook と stub（depth + coverage 共通）

| 層 | 範囲 | 目的 |
|---|---|---|
| L1 正確 | auth/権限/bootstrap の stub | 初回画面の認証を通過 |
| L2 負方向修正 | すべての JSON レスポンス | 未ログインコード → 成功 |
| L3 フォールバック | L1 にヒットしない `/api` など | 空の成功 body で UI を広げる |

- **depth**：fake auth + `forward` で業務コードを変更 + `stubs`。`routes`（hash/history）を走査。`runtime_api.json` を産出
- **coverage**：**document-start** で `preload.js` を注入（CDP の `addScriptToEvaluateOnNewDocument` または Userscript）

検証：`window.__API_RECON_PRELOAD__` が存在する。業務 path が `/login` に戻らない。

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — coverage 動的列挙（必須）

1. メインナビ/サイドバー — 各項目をクリックし、ネットワークを 1〜3 秒待つ
2. タブ — `role=tab`、`.ant-tabs-tab`
3. テーブル — 先頭行の表示/編集/詳細
4. ツールバー — エクスポート、フィルタ、新規作成（**不可逆な削除は避ける**）
5. 各モジュールに入るたび — API/ルートをマージ
6. SPA — `routes.txt` で未カバーの path に対して制御された `pushState`（MPA は禁止）

**パラメータトリガーマトリクス**（必須）：各モジュールで操作タイプごとに 1 回ずつ記録し、**複数サンプルを diff** する：

| 操作 | 通常増えるパラメータ |
|---|---|
| 一覧の初回画面 | ページング + 既定フィルタ |
| 検索のクリック | keyword、filter |
| 高度なフィルタ | より多くの optional |
| 新規作成/編集 | 完全な entity |
| 一括/エクスポート/並び替え | `ids[]`、`exportType`、`sortField` |

**stub 下でも outbound body/headers は実物**——リクエストを基準とする。記録 → `scan_raw.json`、`param_samples.json`、`api_detail.json`。

- **Vue**：`neutralizeVueRouter: true` + document-start preload
- **React**：`routes.txt` + サイドバーティック + `pushState`
- **both**：先に 3a depth、次に 3b coverage

---

## Phase 4 — 権限ツリーの復元

**トリガー**：モジュールページが空白 / 各ルートで bootstrap（locale など）のみ → コンテンツゲート未通過。

| 現象 | 意味 |
|---|---|
| シェル到達に成功 | 描画ゲート + インターセプタゲートは通過済み |
| サイドバーに項目がない/クリックしても空白 | stub の shape または権限コードが不完全 |
| 各ルートの API が同じで極端に少ない | `v-if permission` が通っていない |
| `routes.txt` が bundle よりはるかに少ない | auth モジュールから補完が必要 |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

典型的なチェーン：`role_permissions`（flat codes）+ `permissions/all`（tree）→ `getResultTree` → `userRouteAuth[CODE].url`。

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

中間産出：`route_map.json`、`userRouteAuth.json`、`permissions_tree.json`、`*_stub.json`、`perm_codes_all.txt`。

stub のチェック：外側の `response_code` がインターセプタゲートと一致。flat codes が tree と揃っている。`routes` が `route_map` の全 link をカバーしている。

`config.json` を更新した後、**Phase 3 を再実行**する。大規模 SPA では `waitUntil`、`routeTimeout`、`perRouteMs` を調整可能（reference の A3/I 節参照）。

---

## Phase 5 — マージとレポート

### 産出表

| ファイル | 段階 | 内容 |
|---|---|---|
| `js/`、`api_static.txt`、`routes.txt`、`chunkmap.txt` | 1 | 静的 bundle と path |
| `param_candidates.json` | 1b | 静的パラメータフィールドの候補 |
| `config.json` | 2 | 3 つのゲート + runtime 設定 |
| `runtime_api.json` | 3a | depth の詳細記録（WS/SSE を含む） |
| `param_samples.json`、`scan_raw.json`、`api_detail.json` | 3b | 複数サンプル、クリックログ、detail |
| `route_map.json` など | 4 | 権限ツリーの中間ファイル（実行した場合） |
| `params_merged.json` | 5 | マージしたパラメータフィールド + 信頼度 |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | ルート、API、params、機能ポイント、局限 |
| **insert_assets** | 5 | すべてのサービス、エンドポイント資産を資産ライブラリへ書き込む |

### 5b — パラメータのマージ

`param_samples.json` から diff する。**汎用のマージスクリプトはない**。信頼度のルールは reference J7（高/中/低/トリガー待ち）。

### 5c — エラーからの逆推

許可範囲内で不完全なリクエストを送り 400 を読む（**パラメータ recon であり、脆弱性テストではない**）：`field 'x' is required`、列挙エラーなど。`data` ラッピング、`variables`、暗号化前の `bizData` に注意。

レポートには必ず記載する：runtimeMode、静的/ランタイムの API 数、パラメータの信頼度、未カバーのモジュール、参考スクリプトに対する `CHANGES.md` の要約。

`site_map.json` の推奨構造：

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

その他のフィールドと grep レシピは [reference.md](reference.md) を参照。

---

## 共通事項

- **フレームワーク非依存**：webpack/Vite/Angular の lazy load 方法は同じ
- **転送**：REST/JSON、GraphQL、WebSocket、SSE。gRPC-web は範囲外
- **SSR**：クライアント側 fetch は記録可能。RSC/Server Actions は完全には列挙できない
- **盲点**：JSVMP、WASM、HMAC/mTLS の強い検証 → 静的 + 局限の明記
- **パラメータの盲点**：条件連動、hidden params、WASM による組み立て → 「トリガー待ち」/「到達不可」
- **静的はセーフティネット**：runtime がブロックされても静的なら endpoint を列挙できる

---

## 追加リソース

- Grep レシピ、`config.json` テンプレート、トラブルシューティング、Hook、パラメータ逆解析 J 節、site_map テンプレート：**[reference.md](reference.md)**
- 参考スクリプトのパスは[スクリプトとゲート](#スクリプトとゲート)の表を参照
