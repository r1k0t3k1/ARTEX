package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// assetInterceptCandidates は、挿入待ち資産の入力項目からドメイン/IP/URL の候補文字列を抽出し、資産インターセプトのマッチングに使用する。
// URL の host は分解して分類するため、「URL のみ」のサービス/エンドポイント資産もドメイン/IP ルールにヒットできる。
func assetInterceptCandidates(item assetInputItem) (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, item.Domain)
	for _, d := range item.BoundDomains {
		add(&domains, d)
	}
	add(&ips, item.IP)
	add(&ips, item.ServiceIP)
	add(&urls, item.URL)
	if item.URL != "" {
		if u, err := url.Parse(item.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// assetInputLabel は挿入待ち資産の短い識別子を返し、インターセプトの説明メッセージに使用する。
func assetInputLabel(item assetInputItem) string {
	typ := strings.TrimSpace(item.Type)
	var target string
	switch {
	case strings.TrimSpace(item.Domain) != "":
		target = strings.TrimSpace(item.Domain)
	case strings.TrimSpace(item.URL) != "":
		target = strings.TrimSpace(item.URL)
	case strings.TrimSpace(item.IP) != "":
		target = strings.TrimSpace(item.IP)
	case strings.TrimSpace(item.ServiceIP) != "":
		target = strings.TrimSpace(item.ServiceIP)
	default:
		target = "(不明)"
	}
	if typ != "" {
		return fmt.Sprintf("[%s] %s", typ, target)
	}
	return target
}

// =====================================================================
// Unified asset insertion tools
// =====================================================================

// SetAssetStore wires the asset store and company store onto this ToolSet
// so the insert_assets, add_company_scope, and list_assets tools are active.
func (t *ToolSet) SetAssetStore(as *db.AssetStore, cs *db.CompanyStore) {
	t.as = as
	t.cs = cs
}

// assetInputItem is one element of the insert_assets "assets" array.
type assetInputItem struct {
	Type string `json:"type"` // root_domain|ip|subdomain|app|service|endpoint

	// ---- root_domain / subdomain ----
	Domain      string   `json:"domain"`
	ICP         string   `json:"icp"`
	RecordType  string   `json:"record_type"`
	RecordValue []string `json:"record_value"`

	// ---- ip ----
	IP           string           `json:"ip"`
	BoundDomains []string         `json:"bound_domains"`
	OpenPorts    []db.PortService `json:"open_ports"`

	// ---- app ----
	AppName     string `json:"app_name"`
	BundleID    string `json:"bundle_id"`
	Category    string `json:"category"`
	Description string `json:"description"`
	AppICP      string `json:"app_icp"`
	CompanyID   *int64 `json:"company_id"` // explicit company link (app only; others auto-attribute via scope)

	// ---- service (http) ----
	URL           string           `json:"url"`
	Technologies  []string         `json:"technologies"`
	StatusCode    *int             `json:"status_code"`
	ContentLength *int64           `json:"content_length"`
	PageTitle     string           `json:"page_title"`
	FaviconMMH3   string           `json:"favicon_mmh3"`
	Auth          []map[string]any `json:"auth"`
	ServiceName   string           `json:"service_name"`
	ServiceIP     string           `json:"service_ip"` // optional enrichment IP

	// ---- service (other) ----
	Port  int    `json:"port"`
	Proto string `json:"proto"`

	// ---- endpoint ----
	Method string           `json:"method"`
	Params []map[string]any `json:"params"`
}

// insertAssets is the unified insert_assets agent tool.
func (t *ToolSet) insertAssets() actool.CoreTool {
	return writeTool(
		"insert_assets",
		"新たに発見した資産を一括登録する。1 回で複数タイプを混在させられる（type は列挙値を参照）。\n"+
			"各タイプの必須フィールド：root_domain→domain；ip→ip（IPv4/IPv6 であること。ホスト名は不可）；subdomain→domain；app→app_name；service(HTTP)→url；service(非 HTTP)→service_name+port（ip/domain のいずれか必須）；endpoint→url+method。その他のフィールドの意味は各説明を参照。\n"+
			"auth/technologies/params は追加マージ(append)で、元の値を上書きしない。\n"+
			"戻り値：{results:[{index,id,type}], errors:[{index,error}]}",
		obj(map[string]any{
			// task_id はモデルに公開しない：worker がどの task に属するかはプログラムが SetTaskID で権威的に設定する（handler を参照）。
			"assets": map[string]any{
				"type":        "array",
				"description": "資産配列。各要素が 1 件の資産レコードに対応する",
				"items": obj(map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"root_domain", "ip", "subdomain", "app", "service", "endpoint"},
						"description": "資産タイプ",
					},
					// root_domain / subdomain
					"domain":      str("ルートドメインまたはサブドメイン（root_domain/subdomain では必須）"),
					"icp":         str("ICP 登録番号（任意）"),
					"record_type": str("DNS レコードタイプ：A/AAAA/CNAME/MX など（subdomain では任意）"),
					"record_value": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "DNS レコード値のリスト（subdomain では任意。例 [\"1.2.3.4\",\"2.3.4.5\"]）",
					},
					// ip
					"ip": str("IP アドレス。IPv4/IPv6 アドレスであること。ホスト名は不可（ホスト名は type=subdomain の domain フィールドを使う）；ip タイプでは必須；service/endpoint タイプでは任意で、IP の関連付けに使う"),
					"bound_domains": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "この IP にバインドされたドメインのリスト（ip タイプでは任意）",
					},
					"open_ports": map[string]any{
						"type":        "array",
						"description": "開放ポートのリスト（ip タイプでは任意）",
						"items": obj(map[string]any{
							"port":    intp("ポート番号"),
							"service": str("サービス名。例 http/ssh/mysql など（任意）"),
						}, "port"),
					},
					// app
					"app_name":    str("アプリ名（app タイプでは必須）"),
					"bundle_id":   str("Bundle ID（app タイプでは任意）"),
					"category":    str("アプリの分類（任意）"),
					"description": str("アプリの説明（任意）"),
					"app_icp":     str("アプリの ICP 登録（任意）"),
					"company_id":  intp("帰属先企業 id（app タイプでは任意；app は scope による自動帰属ができないため明示指定が必要。id は add_company_scope が返す）"),
					// service (http)
					"url":         str("完全な URL（プロトコルとポートを含む）（HTTP サービスでは必須；service_type は自動で http になる）"),
					"status_code": intp("HTTP レスポンスのステータスコード。例 200/301/403/404（任意）"),
					"content_length": map[string]any{
						"type":        "integer",
						"description": "HTTP レスポンスボディのバイト数（任意）",
					},
					"page_title":   str("ページの <title> 内容（任意）"),
					"favicon_mmh3": str("favicon MMH3 ハッシュ（任意）"),
					"technologies": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "フィンガープリント/技術スタックのリスト。例 [\"Nginx\",\"Vue\",\"Bootstrap\"]（任意）",
					},
					"auth": map[string]any{
						"type":        "array",
						"description": "発見した認証情報のリスト。各要素は type/username/password などのフィールドを持つ（任意。追加のみで上書きしない）",
						"items":       map[string]any{"type": "object"},
					},
					// service（other、非 HTTP）
					"service_name": str("サービス名。例 ssh/mysql/redis（service が非 HTTP の場合は必須）"),
					"port":         intp("ポート番号（service が非 HTTP の場合は必須）"),
					// endpoint
					"method": str("HTTP メソッド：GET/POST/PUT/PATCH/DELETE など（endpoint では必須）"),
					"params": map[string]any{
						"type":        "array",
						"description": "リクエストパラメータのリスト。各要素は location(query/body/header/path)/name/value/type を持つ（任意。追加のみで上書きしない）",
						"items":       map[string]any{"type": "object"},
					},
				}, "type"),
			},
		}, "assets"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("insert_assets は無効：AssetStore が初期化されていない"), nil
			}
			var a struct {
				Assets []assetInputItem `json:"assets"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("invalid input: " + err.Error()), nil
			}
			// task_id はプログラムが権威的に設定する（worker: SetTaskID）ため、モデルからの入力は受け付けない——モデルが渡し忘れ/誤って渡すことで
			// 資産がタスクに紐付かない、または誤ったタスクに紐付くのを防ぐ。タスクコンテキストのない呼び出し元(auto/pentest/chat)では t.taskID=0。
			taskID := t.taskID

			type result struct {
				Index int    `json:"index"`
				ID    int64  `json:"id"`
				Type  string `json:"type"`
			}
			type errEntry struct {
				Index int    `json:"index"`
				Error string `json:"error"`
			}

			var results []result
			var errs []errEntry

			// 資産ゲートのルールを一度に読み込む；読み取りに失敗した場合は判定をスキップする（挿入はブロックしない）。
			// インターセプトルール = グローバル ∪ タスクレベルの block；許可ルール = タスクレベルの allow。
			blockRules, _ := t.as.ListAssetInterceptRules()
			var allowRules []db.AssetInterceptRule
			if t.taskID > 0 {
				if tb, ta, err := t.as.TaskInterceptRulesSplit(t.taskID); err == nil {
					blockRules = append(blockRules, tb...)
					allowRules = ta
				}
			}

			for i, item := range a.Assets {
				// 資産ゲート：まずインターセプトを評価し、次に許可を評価する。拒否された資産は挿入禁止（Upsert と後続の副作用をスキップ）。
				domains, ips, urls := assetInterceptCandidates(item)
				if d := db.EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
					errs = append(errs, errEntry{
						Index: i,
						Error: fmt.Sprintf("資産 %s %s、挿入は禁止されている", assetInputLabel(item), d.Reason),
					})
					continue
				}

				typ := strings.TrimSpace(item.Type)
				var id int64
				var err error

				switch typ {
				case "root_domain":
					id, err = t.as.UpsertRootDomain(db.UpsertRootDomainReq{
						Domain: item.Domain,
						ICP:    item.ICP,
						TaskID: taskID,
					})

				case "ip":
					id, err = t.as.UpsertIP(db.UpsertIPReq{
						IP:           item.IP,
						BoundDomains: item.BoundDomains,
						OpenPorts:    item.OpenPorts,
						TaskID:       taskID,
					})

				case "subdomain":
					id, err = t.as.UpsertSubdomain(db.UpsertSubdomainReq{
						Domain:      item.Domain,
						RecordType:  item.RecordType,
						RecordValue: item.RecordValue,
						ICP:         item.ICP,
						TaskID:      taskID,
					})

				case "app":
					id, err = t.as.UpsertApp(db.UpsertAppReq{
						Name:        item.AppName,
						BundleID:    item.BundleID,
						Category:    item.Category,
						Description: item.Description,
						ICP:         item.AppICP,
						CompanyID:   item.CompanyID,
						TaskID:      taskID,
					})

				case "service":
					// distinguish HTTP vs other by presence of url
					if item.URL != "" {
						// agent may send "ip" or "service_ip" for the enrichment IP; accept both
						svcIP := item.ServiceIP
						if svcIP == "" {
							svcIP = item.IP
						}
						id, err = t.as.UpsertHTTPService(db.UpsertHTTPServiceReq{
							URL:           item.URL,
							Technologies:  item.Technologies,
							StatusCode:    item.StatusCode,
							ContentLength: item.ContentLength,
							PageTitle:     item.PageTitle,
							FaviconMMH3:   item.FaviconMMH3,
							Auth:          item.Auth,
							IP:            svcIP,
							TaskID:        taskID,
						})
					} else {
						id, err = t.as.UpsertOtherService(db.UpsertOtherServiceReq{
							Domain:      item.Domain,
							IP:          item.IP,
							Port:        item.Port,
							ServiceName: item.ServiceName,
							Auth:        item.Auth,
							TaskID:      taskID,
						})
					}

				case "endpoint":
					id, err = t.as.UpsertEndpoint(db.UpsertEndpointReq{
						URL:    item.URL,
						Method: item.Method,
						Params: item.Params,
						IP:     item.ServiceIP,
						TaskID: taskID,
					})

				default:
					errs = append(errs, errEntry{Index: i, Error: "unknown type: " + typ})
					continue
				}

				if err != nil {
					errs = append(errs, errEntry{Index: i, Error: err.Error()})
					continue
				}
				results = append(results, result{Index: i, ID: id, Type: typ})
				t.writes.Assets++
				t.anchorOwner(id)
				if taskID > 0 {
					var sourceNodeID *int64
					if t.ownerNode > 0 {
						nodeID := t.ownerNode
						sourceNodeID = &nodeID
					}
					summary := "Agent が insert_assets で登録"
					if t.ownerNode > 0 {
						summary = fmt.Sprintf("Worker 意図 #%d が insert_assets で登録", t.ownerNode)
					}
					_ = t.as.SetTaskAssetSource(taskID, id, "agent", summary, sourceNodeID)
				}
				// テスト範囲へ自動追加(source='auto')：worker がトップレベルで明示的に挿入したこの項目に限り、その
				// タイプに応じた保守的な範囲を追加する。副作用で派生した資産はここを通らないため、範囲をいたずらに広げない。taskID=0 のときは何もしない。
				// カバレッジのスイッチとは無関係：task_scope はタスクの範囲境界（一覧/照会のフィルタ基準）であり、
				// カバレッジのスイッチはそれを分母として指標を計算するかどうかだけを決め、範囲そのものを累積するかどうかは決めない。
				{
					svcIP := item.ServiceIP
					if svcIP == "" {
						svcIP = item.IP
					}
					_ = t.as.AddAutoScope(taskID, typ, item.Domain, item.URL, svcIP)
				}
			}

			return jsonResult(map[string]any{
				"results": results,
				"errors":  errs,
			})
		},
	)
}

// addCompanyScope writes to company_scope table and triggers asset attribution.
func (t *ToolSet) addCompanyScope() actool.CoreTool {
	return writeTool(
		"add_company_scope",
		"ドメイン/IP/CIDR/ICP 登録/企業キーワードを、ある会社の【資産範囲】に追加する——ドメイン、ネットワーク、ICP はヒットした資産を自動的に取り込むが、キーワードは Agent への範囲ヒントとして提供されるだけである。\n"+
			"会社名は一意：company が存在しなければ新規作成し、存在すれば再利用する（範囲をマージするだけ）。\n"+
			"scope は 1 行 1 件で、システムが自動判別する：ルートドメイン / URL / 単一 IP / CIDR 網 / ICP 登録 / 企業キーワード。\n"+
			"reason には帰属の根拠（whois/証明書/ASN など）を必ず記載すること。\n"+
			"ガードレール：裸の TLD と広すぎる網（IPv4 プレフィックスは /16-/32、IPv6 プレフィックスは /32-/128 であること）は拒否し、不正な行はスキップして errors で返す。",
		obj(map[string]any{
			"company": str("会社名（存在しなければ新規作成、存在すれば再利用；名称は一意）"),
			"scope":   str("資産範囲。1 行 1 件：ドメイン / URL / IP / CIDR / ICP 登録 / 企業キーワード"),
			"reason":  str("帰属の根拠（証拠/出所）。必ず記入する"),
			"logo":    str("会社アイコン URL（任意；新規作成時のみ有効）"),
		}, "company", "scope"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("add_company_scope は無効：CompanyStore が初期化されていない"), nil
			}
			var a struct {
				Company string `json:"company"`
				Scope   string `json:"scope"`
				Reason  string `json:"reason"`
				Logo    string `json:"logo"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if strings.TrimSpace(a.Company) == "" {
				return actool.Errorf("company は空にできない"), nil
			}
			companyID, _, err := t.cs.UpsertCompany(a.Company, a.Logo)
			if err != nil {
				return actool.Errorf("会社の作成/取得に失敗: " + err.Error()), nil
			}
			lines := splitLines(a.Scope)
			added, skipped, invalid, errMsgs := t.cs.AddScope(companyID, lines, a.Reason)
			out := map[string]any{
				"company_id": companyID,
				"added":      added,
				"skipped":    skipped,
				"invalid":    invalid,
			}
			if len(errMsgs) > 0 {
				out["errors"] = errMsgs
			}
			return jsonResult(out)
		},
	)
}

// addTaskScope lets the plan agent add test scope to THE CURRENT TASK — the coverage
// denominator and the task's authorization edge. Worker discoveries are auto-scoped
// (precise host) by insertAssets; this tool is for DELIBERATELY WIDENING: pull a whole
// root domain or whole company into scope, or add a specific subdomain / ip.
func (t *ToolSet) addTaskScope() actool.CoreTool {
	return writeTool(
		"add_task_scope",
		"テスト範囲を【本タスク】に追加する——これは本タスクの許可境界であり、資産テストのカバレッジの分母でもある。\n"+
			"kind の種類：company(会社全体の資産) / root_domain(ルートドメイン全体、すべてのサブドメインを含む) / subdomain(単一の正確なサブドメイン) / ip / cidr / icp / keyword。\n"+
			"説明：worker が個別に遭遇したホストはシステムが【自動】で範囲に追加する（正確なサブドメイン）。本ツールは【能動的な拡大】に使う——ルートドメイン全体/会社全体を取り込む、または特定のサブドメイン/IP を追加する。\n"+
			"value：company には会社名または id（会社が既に存在すること）；root_domain/subdomain にはドメイン；ip/cidr には IP または CIDR 網；icp/keyword には登録番号または企業キーワードを渡す。\n"+
			"reason には根拠を必ず記載する（監査可能）。複数ある場合は entries 配列を使う。",
		obj(map[string]any{
			"entries": map[string]any{"type": "array", "description": "一括：[{kind, value}]。kind∈company/root_domain/subdomain/ip/cidr/icp/keyword。", "items": map[string]any{"type": "object"}},
			"kind":    str("[単一] company / root_domain / subdomain / ip / cidr / icp / keyword"),
			"value":   str("[単一] 会社名または id / ドメイン / IP / CIDR / ICP / キーワード"),
			"reason":  str("追加の根拠（監査用）。必ず記入する"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("add_task_scope は無効：AssetStore が初期化されていない"), nil
			}
			if t.taskID <= 0 {
				return actool.Errorf("add_task_scope にはタスクコンテキストが必要（現在 task なし）"), nil
			}
			type scopeEntry struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			}
			var a struct {
				Entries    []scopeEntry `json:"entries"`
				scopeEntry              // 単一モード
				Reason     string       `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			items := a.Entries
			if len(items) == 0 {
				items = []scopeEntry{a.scopeEntry}
			}
			var added []map[string]any
			errs := map[string]string{}
			for i, e := range items {
				ts, err := t.as.AddAgentScope(t.taskID, strings.TrimSpace(e.Kind), e.Value, a.Reason, "agent")
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				added = append(added, map[string]any{"kind": ts.Kind, "domain": ts.Domain, "net": ts.Net, "value": ts.Value, "company_id": ts.CompanyID})
			}
			out := map[string]any{"added": added}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		},
	)
}

// listUntestedAssets lets the plan agent pull the current + directly inherited
// scope's not-yet-tested assets on demand (filter by type, paginated).
func (t *ToolSet) listUntestedAssets() actool.CoreTool {
	return readTool(
		"list_untested_assets",
		"【本タスクおよび直接関連タスク】の範囲内で、まだ事実アンカーにカバーされていない資産を照会する（関連範囲は読み取り専用で、追加テストの要否を自分で判断するためのもの。判断の代行ではない）。\n"+
			"任意で資産タイプでフィルタできる：root_domain/subdomain/service/app/endpoint/ip。\n"+
			"ページング：page は 1 から、page_size の既定値は 10。戻り値は {assets:[{id,type,label}], total, page, page_size}。タスクコンテキストでのみ利用可能。",
		obj(map[string]any{
			"type":      str("資産タイプのフィルタ（任意）：root_domain/subdomain/service/app/endpoint/ip"),
			"page":      intp("ページ番号。1 から（既定値 1）"),
			"page_size": intp("1 ページあたりの件数（既定値 10）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_untested_assets は無効：AssetStore が初期化されていない"), nil
			}
			if t.taskID <= 0 || t.ts == nil {
				return actool.Errorf("list_untested_assets にはタスクコンテキストが必要"), nil
			}
			var a struct {
				Type     string `json:"type"`
				Page     int    `json:"page"`
				PageSize int    `json:"page_size"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Page <= 0 {
				a.Page = 1
			}
			if a.PageSize <= 0 {
				a.PageSize = 10
			}
			offset := (a.Page - 1) * a.PageSize
			assets, total, err := t.as.ListUntestedAssetsWithSources(t.taskID, strings.TrimSpace(a.Type), a.PageSize, offset)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{
				"assets": assets, "total": total, "page": a.Page, "page_size": a.PageSize,
			})
		},
	)
}

// listAssets lets an agent query the asset table.
func (t *ToolSet) listAssets() actool.CoreTool {
	return readTool(
		"list_assets",
		"資産ライブラリを照会する：DSL 式による検索、または id/ids による直接取得。ページング対応。【本タスクおよび直接関連タスク】のテスト範囲内の資産のみを返す。\n"+
			"DSL：field=value はあいまい(ILIKE) | field==value は完全一致 | field!=value は除外 | 数値フィールドは > >= < <= に対応 | 裸の語は全文あいまい検索；AND/OR で組み合わせ（AND が優先）、括弧でグループ化できる。資産タイプは独立した type パラメータで指定し、DSL には書かない。\n"+
			"id/ids を渡さない場合、dsl は空であってはならない（無条件の全件照会は許可しない）。\n"+
			"使用可能なフィールド：domain(ルート/サブ/サービスドメイン)、root_domain、ip、url、page_title、icp、service_name、app_name、method(例 GET/POST)、service_type(http|other)、record_type(例 A/CNAME)、technology(配列、=あいまい ==完全一致)、port/status_code/company_id(整数)。\n"+
			"例：status_code>=400 AND technology=shiro ；(port==80 OR port==443) AND technology=nginx",
		obj(map[string]any{
			"dsl":    str(`DSL クエリ式（構文/フィールドはツール説明を参照）。id/ids を渡さない場合は必須。`),
			"type":   str("資産タイプのフィルタ：root_domain|ip|subdomain|app|service|endpoint（独立フィールドで、dsl と併用可能；type 単独では照会できず、dsl が必要）"),
			"id":     intp("単一の資産 id で直接取得（任意。dsl/type とは排他）"),
			"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "複数の資産 id で直接取得（任意。dsl/type とは排他）"},
			"limit":  intp("返却上限。既定値 10（任意）"),
			"offset": intp("ページングのオフセット。既定値 0（任意）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_assets は無効：AssetStore が初期化されていない"), nil
			}
			var a struct {
				DSL    string  `json:"dsl"`
				Type   string  `json:"type"`
				ID     int64   `json:"id"`
				IDs    []int64 `json:"ids"`
				Limit  int     `json:"limit"`
				Offset int     `json:"offset"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Limit <= 0 {
				a.Limit = 10
			}

			var assets []*db.Asset
			var err error
			switch {
			case a.ID > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, []int64{a.ID})
			case len(a.IDs) > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, a.IDs)
			case a.DSL != "":
				assets, err = t.as.QueryDSLInScope(a.DSL, a.Type, t.taskID, a.Limit, a.Offset)
			default:
				return actool.Errorf("id/ids を渡さない場合 dsl は空にできない：全資産の無条件照会は許可しない。照会条件を指定すること"), nil
			}
			if err != nil {
				return actool.Errorf("DSL エラー: " + err.Error()), nil
			}
			return jsonResult(map[string]any{
				"count":  len(assets),
				"assets": assets,
			})
		},
	)
}

// listCompanies は Agent が企業/会社を scope と帰属資産数つきで列挙できるようにする。
func (t *ToolSet) listCompanies() actool.CoreTool {
	return readTool(
		"list_companies",
		"資産ライブラリ内の【企業/会社】と、その資産範囲(scope)、帰属済み資産数を一覧する。どの会社があるかを確認し、"+
			"company_id を取得するために使う（insert_assets で app を関連付けるとき、list_assets を company_id で絞り込むときなど）。"+
			"任意の search で会社名をあいまい絞り込みできる（大文字小文字は区別しない）。空なら全件返す。",
		obj(map[string]any{
			"search": str("会社名であいまい絞り込み（任意。大文字小文字を区別しない）；空なら全件返す"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("list_companies は無効：CompanyStore が初期化されていない"), nil
			}
			var a struct {
				Search string `json:"search"`
			}
			_ = json.Unmarshal(in, &a)
			cos, err := t.cs.ListCompanies()
			if err != nil {
				return actool.Errorf("会社の照会に失敗: " + err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Search))
			type companyOut struct {
				ID         int64    `json:"id"`
				Name       string   `json:"name"`
				AssetCount int      `json:"asset_count"`
				Scope      []string `json:"scope"`
			}
			out := make([]companyOut, 0, len(cos))
			for _, c := range cos {
				if q != "" && !strings.Contains(strings.ToLower(c.Name), q) {
					continue
				}
				scope := make([]string, 0, len(c.Scope))
				for _, r := range c.Scope {
					scope = append(scope, r.Raw)
				}
				out = append(out, companyOut{ID: c.ID, Name: c.Name, AssetCount: c.AssetCount, Scope: scope})
			}
			return jsonResult(map[string]any{"count": len(out), "companies": out})
		},
	)
}

// splitLines splits a multi-line string into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// WorkerTools returns the tool set for a work agent.
func (t *ToolSet) WorkerTools() []actool.CoreTool {
	return []actool.CoreTool{
		// list_findings は残す：脆弱性を報告する前に本タスクで確認済みの脆弱性を調べ、同じ脆弱性の重複報告を避ける。
		t.listFindings(),
		t.addFinding(), t.recordFact(),
		// 資産管理（handler 側で nil ストアを保護している）。
		// add_company_scope は worker に渡さない：企業の資産範囲の定義はプランナー/主 Agent/Auto の責務であり、worker は探索の実行のみを行う。
		t.insertAssets(), t.listAssets(),
		// work をまたいだ振り返り：worker も他の work の観察を再利用でき、重複作業を避けられる。
		// search_all_worker_traces：intent_id を先に知らなくても、キーワードでヒットしたステップをグローバルに抽出する；
		// get_worker_trace：ある work を特定した後、ステップ一覧/その場での検索/完全な内容の取得を行う。
		t.searchAllWorkerTraces(), t.getWorkerTrace(),
		// node_detail：worker は intent_id/ノード id を取得すれば、そのノードの完全な詳細を照会できる（上記の振り返りと併用）。
		t.nodeDetail(),
		// 以下のツールは引き続き worker に【渡さず】、planner/main のみに残す（コンテキストの読み取り、cross-work の振り返りは計画の責務であり、
		// worker は単一意図の実行と書き戻しのみを行う）：list_facts / list_companies / list_worker_traces。
	}
}

// MainAgentTools returns the human-interface tool set.
func (t *ToolSet) MainAgentTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.expandDigest(), // cold-digest §6.1
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.addHint(), t.addIntent(),
		// steer_work：人間は実行中の意図(work)にリアルタイムで軌道修正の指示を注入できる（中断せず、進捗も失わない）。
		t.steerWorkTool(),
		// set_goals：人間は実行時に本タスクへ新たな最終ゴールを追加できる（プランナーはそれに基づいて達成を再判定する）。
		t.setGoals(),
		// set_constraints：人間は実行時に本タスクへ操作制約（allow/deny）を追加/変更でき、planner/worker の探索境界を制約する。
		t.setConstraints(),
		// 資産管理（handler 側で nil ストアを保護している）
		t.insertAssets(), t.addCompanyScope(), t.listAssets(),
		t.addFinding(), t.recordFact(),
		t.addTaskScope(),
		// list_untested_assets：本タスクの範囲内で未テストの資産をオンデマンドに照会し（タイプ+ページング）、追加テストを自分で判断する。
		t.listUntestedAssets(),
	}
}

// AllDomainTools returns the union of all domain tools across all agent types,
// deduped by name (mainagent order wins). Used by the server to build a registry
// for injecting domain tools into agents (Auto, custom) that don't own a per-task
// ToolSet. The caller provides real stores; tools are callable at taskID=0 scope.
func (t *ToolSet) AllDomainTools() []actool.CoreTool {
	seen := map[string]bool{}
	var out []actool.CoreTool
	all := append(append(t.MainAgentTools(), t.PlannerTools()...), t.WorkerTools()...)
	for _, tool := range all {
		if !seen[tool.Name()] {
			seen[tool.Name()] = true
			out = append(out, tool)
		}
	}
	return out
}
