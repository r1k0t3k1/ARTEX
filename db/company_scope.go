package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

// ParsedScope is one parsed asset-scope entry. Its kind selects Domain, Net, or Value.
// Used internally by the company scope parsers and CompanyStore.
type ParsedScope struct {
	Kind   string // "domain" | "ip" | "cidr" | "icp" | "keyword"
	Domain string // normalized registrable/root domain (kind=domain)
	Net    string // normalized CIDR, single IP as /32 or /128 (kind=ip|cidr)
	Value  string // normalized text (kind=icp|keyword)
	Raw    string // original input line
}

// ScopeInput is the structured API form for a company scope rule. Empty Kind
// uses the same automatic classification as the single-textarea UI.
type ScopeInput struct {
	Kind  string `json:"kind,omitempty"`
	Value string `json:"value"`
}

// NormalizeICP removes every Unicode whitespace character and folds case. ICP
// matching intentionally performs no fuzzy or punctuation normalization.
func NormalizeICP(value string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(value)))
}

func normalizeKeyword(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func looksLikeIPAddress(value string) bool {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "://") || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return false
	}
	if strings.Count(value, ":") >= 2 {
		// Require an IPv6-looking prefix. This still catches malformed values such
		// as 2001:db8::zz without treating ordinary colon-delimited keywords as IPs.
		parts := strings.Split(value, ":")
		validSegments := 0
		for _, part := range parts {
			if part == "" {
				if validSegments > 0 || strings.HasPrefix(value, "::") {
					return true
				}
				continue
			}
			if len(part) > 4 {
				return false
			}
			for _, r := range part {
				if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
					return false
				}
			}
			validSegments++
			if validSegments >= 2 {
				return true
			}
		}
		return false
	}
	if !strings.Contains(value, ".") {
		return false
	}
	for _, r := range value {
		if r != '.' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// ParseScopeInput validates an explicitly typed rule. Legacy callers can omit
// Kind and use the same automatic classification as the single-textarea UI.
func ParseScopeInput(input ScopeInput) (ParsedScope, error) {
	kind := strings.ToLower(strings.TrimSpace(input.Kind))
	raw := strings.TrimSpace(input.Value)
	if kind == "" {
		return ParseAutoScopeLine(raw)
	}
	switch kind {
	case "domain", "ip", "cidr":
		rule, err := ParseScopeLine(raw)
		if err != nil {
			return rule, err
		}
		if rule.Kind != kind {
			return ParsedScope{Kind: kind, Raw: raw}, fmt.Errorf("%q は有効な %s スコープではありません", raw, kind)
		}
		return rule, nil
	case "icp":
		value := NormalizeICP(raw)
		if value == "" {
			return ParsedScope{Kind: kind, Raw: raw}, fmt.Errorf("ICP は空にできません")
		}
		return ParsedScope{Kind: kind, Value: value, Raw: raw}, nil
	case "keyword":
		value := normalizeKeyword(raw)
		if value == "" {
			return ParsedScope{Kind: kind, Raw: raw}, fmt.Errorf("企業キーワードは空にできません")
		}
		return ParsedScope{Kind: kind, Value: value, Raw: raw}, nil
	default:
		return ParsedScope{Kind: kind, Raw: raw}, fmt.Errorf("未対応のスコープ種別: %s", kind)
	}
}

// ParseAutoScopeLine classifies one untyped textarea line. Network-looking and
// domain-looking values remain strict so malformed ranges do not silently become
// Agent keywords; all other non-empty text is a keyword.
func ParseAutoScopeLine(line string) (ParsedScope, error) {
	raw := strings.TrimSpace(line)
	if raw == "" {
		return ParsedScope{}, fmt.Errorf("空行")
	}

	if _, _, err := net.ParseCIDR(raw); err == nil {
		return ParseScopeLine(raw)
	}
	if ip := net.ParseIP(raw); ip != nil {
		return ParseScopeLine(raw)
	}
	if slash := strings.LastIndexByte(raw, '/'); slash > 0 {
		address := strings.TrimSpace(raw[:slash])
		if net.ParseIP(address) != nil || looksLikeIPAddress(address) {
			return ParsedScope{Raw: raw}, fmt.Errorf("無効な CIDR: %s", raw)
		}
	}

	if looksLikeIPAddress(raw) {
		return ParsedScope{Raw: raw}, fmt.Errorf("無効な IP: %s", raw)
	}

	looksLikeDomain := strings.Contains(raw, "://") ||
		(strings.Contains(raw, ".") && strings.IndexFunc(raw, unicode.IsSpace) < 0)
	if looksLikeDomain {
		return ParseScopeLine(raw)
	}
	// 備案番号自体はドットを含まない（例：京ICP備12345678号-1）。ドットを含むテキストは
	// ドメインやバージョン番号が混ざっていることが多く、ICP として保存してもどの資産にも
	// 永遠に一致しない死んだルールになるだけである。ICP 帰属は完全一致比較で行う
	// （companies.go の kind='icp' 帰属クエリ参照）ため、こうしたテキストはキーワードに
	// 分類する。
	lower := strings.ToLower(raw)
	if !strings.ContainsAny(raw, ".．。") &&
		(strings.Contains(lower, "icp") || strings.Contains(raw, "备案")) {
		return ParseScopeInput(ScopeInput{Kind: "icp", Value: raw})
	}
	return ParseScopeInput(ScopeInput{Kind: "keyword", Value: raw})
}

func scopeHostname(raw string) (string, error) {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return "", fmt.Errorf("ホスト名が空です")
	}
	if strings.HasPrefix(candidate, "//") {
		candidate = "http:" + candidate
	} else if !strings.Contains(candidate, "://") {
		candidate = "http://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Host == "" {
		if err == nil {
			err = fmt.Errorf("ホスト名がありません")
		}
		return "", err
	}
	host := strings.TrimSuffix(strings.TrimSpace(parsed.Hostname()), ".")
	if host == "" {
		return "", fmt.Errorf("ホスト名が空です")
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	host, err = idna.Lookup.ToASCII(host)
	if err != nil {
		return "", err
	}
	host = strings.ToLower(host)
	if len(host) > 253 {
		return "", fmt.Errorf("ドメインが 253 文字を超えています")
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("ドメインには少なくとも 2 つのラベルが必要です")
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("ドメインラベルが無効です")
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				return "", fmt.Errorf("ドメインに無効な文字が含まれています")
			}
		}
	}
	return host, nil
}

// ParseScopeLine classifies and validates one scope line (root domain / IP /
// CIDR). Guardrails reject bare TLDs and over-broad networks so a rule can never
// swallow the internet. IP ranges must be expressed as CIDR.
func ParseScopeLine(line string) (ParsedScope, error) {
	raw := strings.TrimSpace(line)
	r := ParsedScope{Raw: raw}
	if raw == "" {
		return r, fmt.Errorf("空行")
	}
	// CIDR first because URL parsing treats its slash as a path separator.
	if _, ipnet, err := net.ParseCIDR(raw); err == nil {
		ones, bits := ipnet.Mask.Size()
		if bits == 32 && ones < 16 {
			return r, fmt.Errorf("セグメントが広すぎます（IPv4 は >= /16 が必要）: %s", raw)
		}
		if bits == 128 && ones < 32 {
			return r, fmt.Errorf("セグメントが広すぎます（IPv6 は >= /32 が必要）: %s", raw)
		}
		r.Kind, r.Net = "cidr", ipnet.String()
		return r, nil
	}
	// Single IP.
	if ip := net.ParseIP(raw); ip != nil {
		r.Kind = "ip"
		if ip.To4() != nil {
			r.Net = ip.String() + "/32"
		} else {
			r.Net = ip.String() + "/128"
		}
		return r, nil
	}
	host, err := scopeHostname(raw)
	if err != nil {
		return r, fmt.Errorf("有効なドメイン/IP/CIDR として認識できません: %s", raw)
	}
	if ip := net.ParseIP(host); ip != nil {
		r.Kind = "ip"
		if ip.To4() != nil {
			r.Net = ip.String() + "/32"
		} else {
			r.Net = ip.String() + "/128"
		}
		return r, nil
	}
	if looksLikeIPAddress(host) {
		return r, fmt.Errorf("無効な IP: %s", raw)
	}
	if strings.Contains(raw, "-") && strings.Count(raw, ".") >= 6 {
		return r, fmt.Errorf("IP 範囲は CIDR で指定してください（例: 1.2.3.0/24）: %s", raw)
	}
	// Domain (registrable). Reject bare TLDs / public suffixes.
	d := DomainKey(host)
	if suf, icann := publicsuffix.PublicSuffix(d); icann && suf == d {
		return r, fmt.Errorf("裸の TLD をスコープにはできません: %s", raw)
	}
	r.Kind, r.Domain = "domain", d
	return r, nil
}
