package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// 資産インターセプトルールのマッチング/実行層。asset_intercept.go はルールの保存だけを
// 担い、ここでは「対象資産」のドメイン/IP/URL を有効なルールと照合する。agent ツール
// （add_intent、insert_assets）が意図の下達 / 資産の挿入前に呼び、一致すれば拒否する。

// AssetInterceptKindLabel は kind の日本語ラベルを返し、agent への説明メッセージに使う。
func AssetInterceptKindLabel(kind string) string {
	switch kind {
	case "exact_domain":
		return "ドメイン（完全一致）"
	case "exact_ip":
		return "IP（完全一致）"
	case "exact_url":
		return "URL（完全一致）"
	case "fuzzy_domain":
		return "ドメイン（部分一致）"
	case "fuzzy_ip":
		return "IP（部分一致）"
	case "fuzzy_url":
		return "URL（部分一致）"
	case "cidr":
		return "CIDR セグメント"
	}
	return kind
}

// Reason は人が読める一致理由を返す。形式は「資産インターセプトルールに一致 [ドメイン（部分一致）: .gov.cn]（備考）」。
func (r AssetInterceptRule) Reason() string {
	s := fmt.Sprintf("資産インターセプトルールに一致 [%s: %s]", AssetInterceptKindLabel(r.Kind), r.Pattern)
	if note := strings.TrimSpace(r.Note); note != "" {
		s += "（" + note + "）"
	}
	return s
}

// matchOne は有効なルール 1 件が与えられたドメイン/IP/URL 候補に一致するか判定し、一致した具体値を返す。
func matchOne(r AssetInterceptRule, domains, ips, urls []string) (string, bool) {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return "", false
	}
	switch r.Kind {
	case "exact_domain":
		for _, d := range domains {
			if strings.EqualFold(strings.TrimSpace(d), p) {
				return d, true
			}
		}
	case "exact_ip":
		for _, ip := range ips {
			if strings.TrimSpace(ip) == p {
				return ip, true
			}
		}
	case "exact_url":
		for _, u := range urls {
			if strings.TrimSpace(u) == p {
				return u, true
			}
		}
	case "fuzzy_domain":
		lp := strings.ToLower(p)
		for _, d := range domains {
			if d != "" && strings.Contains(strings.ToLower(d), lp) {
				return d, true
			}
		}
	case "fuzzy_ip":
		for _, ip := range ips {
			if ip != "" && strings.Contains(ip, p) {
				return ip, true
			}
		}
	case "fuzzy_url":
		lp := strings.ToLower(p)
		for _, u := range urls {
			if u != "" && strings.Contains(strings.ToLower(u), lp) {
				return u, true
			}
		}
	case "cidr":
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return "", false
		}
		for _, ip := range ips {
			if pip := net.ParseIP(strings.TrimSpace(ip)); pip != nil && ipnet.Contains(pip) {
				return ip, true
			}
		}
	}
	return "", false
}

// MatchAssetInterceptRules は与えられたドメイン/IP/URL 候補に一致する最初の有効ルールと、
// 一致した具体値を返す。insert_assets が生の入力（まだ DB に入っていない assetInputItem）
// で照合するのに使う。
func MatchAssetInterceptRules(rules []AssetInterceptRule, domains, ips, urls []string) (AssetInterceptRule, string, bool) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if v, ok := matchOne(r, domains, ips, urls); ok {
			return r, v, true
		}
	}
	return AssetInterceptRule{}, "", false
}

// interceptCandidates は DB 格納済み資産からインターセプト照合用のドメイン/IP/URL
// 候補を抽出する。URL の host は切り出して分類するため、「URL だけ」のサービス系資産も
// ドメイン/IP ルールに一致させられる。
func (a *Asset) interceptCandidates() (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, a.Domain)
	add(&domains, a.RootDomain)
	for _, d := range a.BoundDomains {
		add(&domains, d)
	}
	add(&ips, a.IP)
	add(&urls, a.URL)
	if a.URL != "" {
		if u, err := url.Parse(a.URL); err == nil {
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

// InterceptLabel は資産の短い識別子を返し、agent への説明メッセージに使う。
func (a *Asset) InterceptLabel() string {
	var target string
	switch {
	case a.Domain != "":
		target = a.Domain
	case a.URL != "":
		target = a.URL
	case a.IP != "":
		target = a.IP
	default:
		target = fmt.Sprintf("#%d", a.ID)
	}
	return fmt.Sprintf("資産#%d[%s] %s", a.ID, a.Type, target)
}

// hasEnabledRule はルール集合に有効なルールが 1 つでもあるか判定する。
func hasEnabledRule(rules []AssetInterceptRule) bool {
	for _, r := range rules {
		if r.Enabled {
			return true
		}
	}
	return false
}

// AssetGateDecision は「まず拒否、次に許可」ゲートによる候補集合の判定結果である。
type AssetGateDecision struct {
	Allowed bool
	Reason  string // 拒否理由（資産識別子を含まない）。Allowed=true のとき空
}

// EvaluateAssetGate はタスクレベルのゲート判定を実行する：
//  1. 有効な blockRules のいずれかに一致 → 拒否（インターセプト理由）。
//  2. そうでなく、allowRules に有効なものがあり、どれにも一致しない → 拒否（許可範囲外）。
//  3. それ以外は許可。
//
// allowRules が空/有効項目なしのとき、許可ゲートは機能しない（ホワイトリスト未使用で
// 全件許可）。「許可ルール未設定」ですべての資産が弾かれるのを避ける。
func EvaluateAssetGate(blockRules, allowRules []AssetInterceptRule, domains, ips, urls []string) AssetGateDecision {
	if rule, _, ok := MatchAssetInterceptRules(blockRules, domains, ips, urls); ok {
		return AssetGateDecision{Allowed: false, Reason: rule.Reason()}
	}
	if hasEnabledRule(allowRules) {
		if _, _, ok := MatchAssetInterceptRules(allowRules, domains, ips, urls); !ok {
			return AssetGateDecision{Allowed: false, Reason: "タスクの許可（ホワイトリスト）範囲外のため、テストできません"}
		}
	}
	return AssetGateDecision{Allowed: true}
}

// AssetInterceptHit はゲートに拒否された資産を表す（インターセプト一致、または許可範囲外）。
type AssetInterceptHit struct {
	Asset  *Asset
	Reason string // 人が読める理由
}

// Describe は人が読める説明（資産情報 + 理由）を返す。
func (h AssetInterceptHit) Describe() string {
	return fmt.Sprintf("%s → %s", h.Asset.InterceptLabel(), h.Reason)
}

// ListAssetInterceptRules は *DB の同名メソッドへの委譲で、AssetStore だけを保持する
// 呼び出し側（agent ツールなど）でもルールを読めるようにする。
func (s *AssetStore) ListAssetInterceptRules() ([]AssetInterceptRule, error) {
	return s.db.ListAssetInterceptRules()
}

// CheckAssetsIntercept は id で資産をロードし、1 件ずつ「まず拒否、次に許可」ゲートを
// 実行して、拒否された資産をすべて返す。インターセプトルール = グローバル ∪ タスク
// レベルの block、許可ルール = タスクレベルの allow（このタスクのみ）。
// id がないときは即座に返す。グローバルの GetByIDs（タスク範囲で絞らない）を使い、
// scope によってインターセプトが弱められないようにする。
func (s *AssetStore) CheckAssetsIntercept(taskID int64, ids []int64) ([]AssetInterceptHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	blockRules, err := s.db.ListAssetInterceptRules()
	if err != nil {
		return nil, err
	}
	var allowRules []AssetInterceptRule
	if taskID > 0 {
		tb, ta, err := s.TaskInterceptRulesSplit(taskID)
		if err != nil {
			return nil, err
		}
		blockRules = append(blockRules, tb...)
		allowRules = ta
	}
	// インターセプトルールも有効な許可ルールもない → 判定不要で全件許可。
	if len(blockRules) == 0 && !hasEnabledRule(allowRules) {
		return nil, nil
	}
	assets, err := s.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	var hits []AssetInterceptHit
	for _, a := range assets {
		domains, ips, urls := a.interceptCandidates()
		if d := EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
			hits = append(hits, AssetInterceptHit{Asset: a, Reason: d.Reason})
		}
	}
	return hits, nil
}
