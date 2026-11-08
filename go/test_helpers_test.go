package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type fakeSender struct {
	requests []pluginapi.HTTPRequest
	reply    func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

func (f *fakeSender) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.requests = append(f.requests, req)
	if f.reply == nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("unexpected request")
	}
	return f.reply(req)
}

func jsonReply(body string) func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	return func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(body)}, nil
	}
}

func testCredential(site string) credential {
	return credential{
		Type:      credentialProvider,
		Site:      site,
		Access:    "token-1",
		Refresh:   "refresh-1",
		Expires:   4102444800000,
		UID:       "user-1",
		Name:      "Tester",
		ClientID:  "ono9krqynydwx5",
		DeviceID:  "1234567890123456",
		MachineID: strings.Repeat("a", 32),
	}
}

func credentialJSON(c credential) []byte {
	raw, _ := json.Marshal(c)
	return raw
}
