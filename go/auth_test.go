package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func stateFlow(t *testing.T, dir, state string) []byte {
	t.Helper()
	key, err := loadStateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openState(key, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeCallbackFlow(dir, s.Flow) })
	return s.Flow
}

func TestLoginStartPublishesBothPortals(t *testing.T) {
	t.Cleanup(func() { _ = configure(nil) })
	for _, tc := range []struct {
		provider string
		host     string
		digits   int
	}{
		{siteTrae, "www.trae.ai", 19},
		{siteTraeCN, "www.trae.cn", 16},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			if err := configure(nil); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			start, err := startLogin(pluginapi.AuthLoginStartRequest{Provider: tc.provider, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, &fakeSender{})
			if err != nil {
				t.Fatal(err)
			}
			stateFlow(t, dir, start.State)
			u, err := url.Parse(start.URL)
			if err != nil {
				t.Fatal(err)
			}
			if u.Host != tc.host || u.Path != "/authorization" {
				t.Fatalf("url = %s", start.URL)
			}
			q := u.Query()
			if q.Get("client_id") != "ono9krqynydwx5" || q.Get("hide_saas_login") != "false" {
				t.Fatalf("query = %v", q)
			}
			if len(q.Get("device_id")) != tc.digits {
				t.Fatalf("device id = %q", q.Get("device_id"))
			}
			if !strings.HasPrefix(q.Get("auth_callback_url"), "http://127.0.0.1:") || !strings.HasSuffix(q.Get("auth_callback_url"), "/authorize") {
				t.Fatalf("callback = %q", q.Get("auth_callback_url"))
			}
			if start.ExpiresAt.Before(time.Now().Add(9 * time.Minute)) {
				t.Fatalf("expiry = %v", start.ExpiresAt)
			}
		})
	}
}

func TestLoginStartRejectsInvalidEntryWithoutState(t *testing.T) {
	dir := t.TempDir()
	sender := &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		t.Error("invalid entry contacted the vendor")
		return pluginapi.HTTPResponse{}, fmt.Errorf("unexpected")
	}}
	if _, err := startLogin(pluginapi.AuthLoginStartRequest{Provider: "other", Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender); err == nil {
		t.Fatal("expected an unknown-entry error")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".trae-oauth-") {
			t.Fatalf("rejected entry created %s", e.Name())
		}
	}
}

func TestCallbackCompletesGlobalSignIn(t *testing.T) {
	if err := configure(nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	start, err := startLogin(pluginapi.AuthLoginStartRequest{Provider: siteTrae, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, &fakeSender{})
	if err != nil {
		t.Fatal(err)
	}
	stateFlow(t, dir, start.State)
	u, _ := url.Parse(start.URL)
	callback := u.Query().Get("auth_callback_url")
	jwt := `{"Token":"jwt-1","RefreshToken":"refresh-1","TokenExpireAt":4102444800,"ClientID":"ono9krqynydwx5"}`
	info := `{"UserID":"u1","ScreenName":"Tester","Host":"https://coresg-normal.trae.ai"}`
	resp, err := http.Get(callback + "?userJwt=" + url.QueryEscape(jwt) + "&userInfo=" + url.QueryEscape(info))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	poll, err := pollLogin(pluginapi.AuthLoginPollRequest{State: start.State, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, &fakeSender{})
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll = %#v", poll)
	}
	c, err := readCredential(poll.Auth.StorageJSON)
	if err != nil {
		t.Fatal(err)
	}
	if c.Site != siteTrae || c.Access != "jwt-1" || c.Refresh != "refresh-1" || c.UID != "u1" || c.Name != "Tester" {
		t.Fatalf("credential = %#v", c)
	}
	if c.API != "https://coresg-normal.trae.ai" || len(c.DeviceID) != 19 {
		t.Fatalf("credential = %#v", c)
	}
	if poll.Auth.Provider != credentialProvider || poll.Auth.Metadata["oauth_provider"] != siteTrae {
		t.Fatalf("auth = %#v", poll.Auth)
	}
}

func TestCallbackCompletesCNBoundSignIn(t *testing.T) {
	if err := configure(nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(req.URL, "/trae/api/v3/oauth/ExchangeToken") {
			return pluginapi.HTTPResponse{}, fmt.Errorf("unexpected request %s", req.URL)
		}
		var body map[string]any
		if err := json.Unmarshal(req.Body, &body); err != nil {
			t.Fatal(err)
		}
		proof, _ := body["DeviceProof"].(map[string]any)
		if proof["Signature"] == "" || proof["Nonce"] == "" {
			t.Fatalf("device proof = %#v", proof)
		}
		info, _ := body["DeviceInfo"].(map[string]any)
		if info["DeviceID"] == "" || info["DevicePublicKey"] == "" {
			t.Fatalf("device info = %#v", info)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"Result":{"Token":"bound-token","RefreshToken":"refresh-2","TokenExpireAt":4102444800}}`)}, nil
	}}
	start, err := startLogin(pluginapi.AuthLoginStartRequest{Provider: siteTraeCN, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	if err != nil {
		t.Fatal(err)
	}
	stateFlow(t, dir, start.State)
	u, _ := url.Parse(start.URL)
	jwt := `{"Token":"jwt-1","RefreshToken":"refresh-1","TokenExpireAt":4102444800}`
	info := `{"UserID":"u2","ScreenName":"CN Tester","Host":"https://www.trae.cn"}`
	resp, err := http.Get(u.Query().Get("auth_callback_url") + "?userJwt=" + url.QueryEscape(jwt) + "&userInfo=" + url.QueryEscape(info))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	poll, err := pollLogin(pluginapi.AuthLoginPollRequest{State: start.State, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("poll = %#v", poll)
	}
	c, err := readCredential(poll.Auth.StorageJSON)
	if err != nil {
		t.Fatal(err)
	}
	if c.Access != "bound-token" || c.Refresh != "refresh-2" || len(c.DeviceID) != 16 {
		t.Fatalf("credential = %#v", c)
	}
	if len(sender.requests) != 1 {
		t.Fatalf("requests = %d", len(sender.requests))
	}
}

func TestPollWithoutCallbackStaysPending(t *testing.T) {
	dir := t.TempDir()
	start, err := startLogin(pluginapi.AuthLoginStartRequest{Provider: siteTrae, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, &fakeSender{})
	if err != nil {
		t.Fatal(err)
	}
	stateFlow(t, dir, start.State)
	poll, err := pollLogin(pluginapi.AuthLoginPollRequest{State: start.State, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, &fakeSender{})
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("poll = %#v", poll)
	}
}

func TestParseAuthPreservesHostMetadata(t *testing.T) {
	c := testCredential(siteTrae)
	raw, _ := json.Marshal(map[string]any{
		"type": "trae", "site": c.Site, "access": c.Access, "refresh": c.Refresh,
		"expires": c.Expires, "uid": c.UID, "name": c.Name, "client_id": c.ClientID,
		"device_id": c.DeviceID, "machine_id": c.MachineID,
		"note": "kept", "prefix": "t-", "priority": float64(2),
	})
	parsed := parseAuth(pluginapi.AuthParseRequest{Provider: "trae", FileName: "trae-x.json", RawJSON: raw})
	if !parsed.Handled {
		t.Fatal("credential not recognized")
	}
	if parsed.Auth.ID != "trae-x.json" || parsed.Auth.Provider != credentialProvider {
		t.Fatalf("auth = %#v", parsed.Auth)
	}
	if parsed.Auth.Metadata["note"] != "kept" || parsed.Auth.Metadata["prefix"] != "t-" {
		t.Fatalf("metadata = %#v", parsed.Auth.Metadata)
	}
	if parsed.Auth.Metadata["type"] != credentialProvider || parsed.Auth.Metadata["oauth_provider"] != siteTrae {
		t.Fatalf("metadata = %#v", parsed.Auth.Metadata)
	}
	if _, exists := parsed.Auth.Metadata["access"]; exists {
		t.Fatal("credential field leaked into metadata")
	}
	if other := parseAuth(pluginapi.AuthParseRequest{Provider: "trae", RawJSON: []byte(`{"type":"other"}`)}); other.Handled {
		t.Fatal("other credential recognized")
	}
}

func TestRefreshUpdatesTokenAndKeepsMetadata(t *testing.T) {
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if !strings.HasSuffix(req.URL, "/cloudide/api/v3/trae/oauth/ExchangeToken") {
			return pluginapi.HTTPResponse{}, fmt.Errorf("unexpected request %s", req.URL)
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"Result":{"Token":"token-2","RefreshToken":"refresh-2","TokenExpireAt":4102444800}}`)}, nil
	}}
	c := testCredential(siteTrae)
	resp, err := refreshCredential(pluginapi.AuthRefreshRequest{
		AuthID: "auth-1", StorageJSON: credentialJSON(c), Metadata: map[string]any{"note": "keep"},
	}, sender)
	if err != nil {
		t.Fatal(err)
	}
	next, err := readCredential(resp.Auth.StorageJSON)
	if err != nil {
		t.Fatal(err)
	}
	if next.Access != "token-2" || next.Refresh != "refresh-2" || next.DeviceID != c.DeviceID {
		t.Fatalf("credential = %#v", next)
	}
	if resp.Auth.Metadata["note"] != "keep" || resp.Auth.Metadata["type"] != credentialProvider {
		t.Fatalf("metadata = %#v", resp.Auth.Metadata)
	}
}

func TestRefreshExpiredSignInIsReauth(t *testing.T) {
	sender := &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 400, Body: []byte(`{"code":10101,"message":"refresh token expired"}`)}, nil
	}}
	c := testCredential(siteTrae)
	_, err := refreshCredential(pluginapi.AuthRefreshRequest{AuthID: "auth-1", StorageJSON: credentialJSON(c)}, sender)
	var expired *expiredError
	if !errors.As(err, &expired) {
		t.Fatalf("err = %v, want an expired sign-in", err)
	}
}
