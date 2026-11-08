package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// entitlementPack is one of Trae's entitlement packs: the allowance's base
// information and what the account used of it.
type entitlementPack struct {
	EntitlementBaseInfo struct {
		Quota struct {
			CreditsLimit    *float64 `json:"credits_limit"`
			BasicUsageLimit *float64 `json:"basic_usage_limit"`
			BonusUsageLimit *float64 `json:"bonus_usage_limit"`
		} `json:"quota"`
		EndTime  json.RawMessage `json:"end_time"`
		ExpireAt json.RawMessage `json:"expire_time"`
	} `json:"entitlement_base_info"`
	Usage struct {
		CreditsAmount    float64 `json:"credits_amount"`
		BasicUsageAmount float64 `json:"basic_usage_amount"`
		BonusUsageAmount float64 `json:"bonus_usage_amount"`
	} `json:"usage"`
	DisplayDesc string          `json:"display_desc"`
	EndTime     json.RawMessage `json:"end_time"`
}

type entitlementResponse struct {
	Packs          []entitlementPack `json:"user_entitlement_pack_list"`
	CreditsBilling *bool             `json:"is_credits_billing"`
	Data           *struct {
		Packs          []entitlementPack `json:"user_entitlement_pack_list"`
		CreditsBilling *bool             `json:"is_credits_billing"`
	} `json:"data"`
}

func (r entitlementResponse) packs() []entitlementPack {
	if len(r.Packs) > 0 {
		return r.Packs
	}
	if r.Data != nil {
		return r.Data.Packs
	}
	return nil
}

func (r entitlementResponse) creditsBilling() bool {
	if r.CreditsBilling != nil {
		return *r.CreditsBilling
	}
	return r.Data != nil && r.Data.CreditsBilling != nil && *r.Data.CreditsBilling
}

func quotaDescription() pluginapi.QuotaDescribeResponse {
	return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{credentialProvider}, DisplayName: "Trae", SupportsReset: false}
}

func quotaReset() pluginapi.QuotaResetResponse {
	return pluginapi.QuotaResetResponse{Success: false, Message: "Reset Trae credits on the vendor website"}
}

// quotaFetch reads the account's usage page and normalizes it: Trae CN counts
// credits, the international deployment bills dollars.
func quotaFetch(req pluginapi.QuotaFetchRequest, sender httpSender) (pluginapi.QuotaFetchResponse, error) {
	out := pluginapi.QuotaFetchResponse{}
	c, err := readCredential(req.StorageJSON)
	if err != nil {
		return out, pluginabiError("invalid_auth", "invalid Trae credential", 401)
	}
	spec, ok := siteFor(c.Site)
	if !ok {
		return out, pluginabiError("invalid_auth", "invalid Trae credential", 401)
	}
	raw, status, err := readUsage(sender, spec, c)
	if err != nil {
		return out, pluginabiError("usage_error", err.Error(), status)
	}
	var env entitlementResponse
	if json.Unmarshal(raw, &env) != nil {
		return out, pluginabiError("usage_error", spec.Realm+" returned an invalid usage response", 502)
	}
	if spec.UsageKind == "credits" {
		return creditsQuota(out, spec, c, env), nil
	}
	quota, err := dollarQuota(out, spec, c, env)
	if err != nil {
		return out, err
	}
	return quota, nil
}

func creditsQuota(out pluginapi.QuotaFetchResponse, spec siteSpec, c credential, env entitlementResponse) pluginapi.QuotaFetchResponse {
	limit, used, unlimited, until := creditsOf(env.packs())
	plan := "Free"
	if env.creditsBilling() {
		plan = "Credits"
	}
	out.Subscription = &pluginapi.QuotaSubscription{Plan: plan, TierName: spec.Realm}
	group := pluginapi.QuotaGroup{DisplayName: spec.Realm}
	reset := ""
	if until > 0 {
		reset = time.UnixMilli(until).UTC().Format(time.RFC3339)
	}
	if unlimited {
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: "Credits", RemainingFraction: 1, Description: "unlimited credits", ResetTime: reset})
		out.Groups = append(out.Groups, group)
		return out
	}
	if limit <= 0 {
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: "Credits", RemainingFraction: 0, Description: "0 credits left", ResetTime: reset})
		out.Summary = append(out.Summary, pluginapi.QuotaMetric{Key: "credits", Label: "Credits", Value: 0, Unit: "credits", Format: "number"})
		out.Groups = append(out.Groups, group)
		return out
	}
	remaining := math.Max(0, limit-used)
	group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{
		Window:            "Credits",
		RemainingFraction: clamp01(remaining / limit),
		Description:       fmt.Sprintf("%s / %s credits left", formatAmount(remaining), formatAmount(limit)),
		ResetTime:         reset,
	})
	out.Summary = append(out.Summary, pluginapi.QuotaMetric{Key: "credits", Label: "Credits", Value: remaining, Unit: "credits", Format: "number"})
	out.Groups = append(out.Groups, group)
	return out
}

func dollarQuota(out pluginapi.QuotaFetchResponse, spec siteSpec, c credential, env entitlementResponse) (pluginapi.QuotaFetchResponse, error) {
	pack := firstUsagePack(env.packs())
	if pack == nil {
		return out, pluginabiError("usage_error", spec.Realm+" usage: no entitlement pack in the answer", 502)
	}
	basic := valueOr(pack.EntitlementBaseInfo.Quota.BasicUsageLimit, 0)
	bonus := valueOr(pack.EntitlementBaseInfo.Quota.BonusUsageLimit, 0)
	total := basic + bonus
	used := pack.Usage.BasicUsageAmount + pack.Usage.BonusUsageAmount
	plan := firstNonEmpty(strings.TrimSpace(pack.DisplayDesc), "Free")
	out.Subscription = &pluginapi.QuotaSubscription{Plan: plan, TierName: spec.Realm}
	reset := ""
	if at := whenOf(pack.EntitlementBaseInfo.EndTime); at > 0 {
		reset = time.UnixMilli(at).UTC().Format(time.RFC3339)
	}
	group := pluginapi.QuotaGroup{DisplayName: spec.Realm}
	if total > 0 {
		remaining := math.Max(0, total-used)
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{
			Window:            "Dollar Usage",
			RemainingFraction: clamp01(remaining / total),
			Description:       fmt.Sprintf("$%s / $%s left", formatAmount(remaining), formatAmount(total)),
			ResetTime:         reset,
		})
		out.Summary = append(out.Summary, pluginapi.QuotaMetric{Key: "usd", Label: "Dollar Usage", Value: round2(remaining), Unit: "usd", Format: "currency", Currency: "USD"})
	}
	out.Groups = append(out.Groups, group)
	return out, nil
}

func creditsOf(packs []entitlementPack) (limit, used float64, unlimited bool, until int64) {
	for _, p := range packs {
		l := p.EntitlementBaseInfo.Quota.CreditsLimit
		if l == nil {
			continue
		}
		if *l == -1 {
			unlimited = true
		} else if *l > 0 {
			limit += *l
		}
		if p.Usage.CreditsAmount > 0 {
			used += p.Usage.CreditsAmount
		}
		at := whenOf(p.EntitlementBaseInfo.EndTime)
		if at == 0 {
			at = whenOf(p.EntitlementBaseInfo.ExpireAt)
		}
		if at == 0 {
			at = whenOf(p.EndTime)
		}
		if at > 0 && (until == 0 || at < until) {
			until = at
		}
	}
	return limit, used, unlimited, until
}

func firstUsagePack(packs []entitlementPack) *entitlementPack {
	for i := range packs {
		if packs[i].EntitlementBaseInfo.Quota.BasicUsageLimit != nil || packs[i].EntitlementBaseInfo.Quota.BonusUsageLimit != nil {
			return &packs[i]
		}
	}
	return nil
}

func valueOr(v *float64, fallback float64) float64 {
	if v == nil || math.IsNaN(*v) {
		return fallback
	}
	return *v
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// readUsage asks the site's usage page with the account's IDE headers.
func readUsage(sender httpSender, spec siteSpec, c credential) ([]byte, int, error) {
	host := spec.Auth
	path := "/trae/api/v2/pay/ide_user_ent_usage"
	body := map[string]any{"require_usage": true, "req_source": 0}
	if spec.UsageKind == "dollar" {
		host = spec.Pay
		if isUSRegion(c.Region) || isTraeGlobalUSHost(c.API) {
			host = spec.USPay
		}
		path = "/trae/api/v1/pay/user_current_entitlement_list"
		body = map[string]any{}
	}
	raw, _ := json.Marshal(body)
	resp, err := sender.Do(pluginapi.HTTPRequest{
		Method:  "POST",
		URL:     strings.TrimRight(host, "/") + path,
		Headers: ideHeaders(c, spec, map[string]string{"Accept": "application/json"}),
		Body:    raw,
	})
	if err != nil {
		return nil, http.StatusBadGateway, errors.New(spec.Realm + " usage request failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("%s usage failed (HTTP %d)", spec.Realm, resp.StatusCode)
	}
	return resp.Body, 0, nil
}

// formatAmount writes a credit or dollar count as Trae's own panel does: whole
// units with thousands separators, up to two decimals for a part unit.
func formatAmount(v float64) string {
	var text string
	if v == math.Trunc(v) {
		text = fmt.Sprintf("%.0f", v)
	} else {
		text = fmt.Sprintf("%.2f", v)
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	}
	whole, fraction, hasFraction := strings.Cut(text, ".")
	var b strings.Builder
	for i := 0; i < len(whole); i++ {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(whole[i])
	}
	if hasFraction {
		b.WriteByte('.')
		b.WriteString(fraction)
	}
	return b.String()
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}
