package main

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestQuotaFetchCredits(t *testing.T) {
	c := testCredential(siteTraeCN)
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(req.URL, "/trae/api/v2/pay/ide_user_ent_usage") {
			t.Fatalf("unexpected request %s", req.URL)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{
			"is_credits_billing": true,
			"user_entitlement_pack_list": [
				{"entitlement_base_info":{"quota":{"credits_limit":100},"end_time":"2026-12-01T00:00:00Z"},"usage":{"credits_amount":25}}
			]
		}`)}, nil
	}}
	resp, err := quotaFetch(pluginapi.QuotaFetchRequest{StorageJSON: credentialJSON(c)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Subscription == nil || resp.Subscription.Plan != "Credits" || resp.Subscription.TierName != "Trae CN" {
		t.Fatalf("subscription = %#v", resp.Subscription)
	}
	if len(resp.Groups) != 1 || len(resp.Groups[0].Buckets) != 1 {
		t.Fatalf("groups = %#v", resp.Groups)
	}
	bucket := resp.Groups[0].Buckets[0]
	if bucket.Window != "Credits" || bucket.RemainingFraction != 0.75 || !strings.Contains(bucket.Description, "75 / 100") {
		t.Fatalf("bucket = %#v", bucket)
	}
	if bucket.ResetTime == "" {
		t.Fatal("reset time missing")
	}
	if len(resp.Summary) != 1 || resp.Summary[0].Value != 75 || resp.Summary[0].Unit != "credits" {
		t.Fatalf("summary = %#v", resp.Summary)
	}
}

func TestQuotaFetchDollar(t *testing.T) {
	c := testCredential(siteTrae)
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(req.URL, "/trae/api/v1/pay/user_current_entitlement_list") {
			t.Fatalf("unexpected request %s", req.URL)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{
			"user_entitlement_pack_list": [
				{"display_desc":"Pro","entitlement_base_info":{"quota":{"basic_usage_limit":10,"bonus_usage_limit":5},"end_time":"2026-12-01T00:00:00Z"},"usage":{"basic_usage_amount":4,"bonus_usage_amount":1}}
			]
		}`)}, nil
	}}
	resp, err := quotaFetch(pluginapi.QuotaFetchRequest{StorageJSON: credentialJSON(c)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Subscription == nil || resp.Subscription.Plan != "Pro" {
		t.Fatalf("subscription = %#v", resp.Subscription)
	}
	bucket := resp.Groups[0].Buckets[0]
	if bucket.Window != "Dollar Usage" || bucket.RemainingFraction != 0.6666666666666666 || !strings.Contains(bucket.Description, "$10 / $15") {
		t.Fatalf("bucket = %#v", bucket)
	}
	if len(resp.Summary) != 1 || resp.Summary[0].Value != 10 || resp.Summary[0].Format != "currency" {
		t.Fatalf("summary = %#v", resp.Summary)
	}
}

func TestQuotaFetchUnlimited(t *testing.T) {
	c := testCredential(siteTraeCN)
	sender := &fakeSender{reply: jsonReply(`{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":-1}},"usage":{"credits_amount":12}}]}`)}
	resp, err := quotaFetch(pluginapi.QuotaFetchRequest{StorageJSON: credentialJSON(c)}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Groups) != 1 || resp.Groups[0].Buckets[0].Description != "unlimited credits" {
		t.Fatalf("groups = %#v", resp.Groups)
	}
}
