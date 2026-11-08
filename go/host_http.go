package main

import (
	"errors"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type httpSender interface {
	Do(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

type cpaSender struct{ callbackID string }

func (s cpaSender) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	var out pluginapi.HTTPResponse
	err := hostRPC("host.http.do", map[string]any{"host_callback_id": s.callbackID, "request": req}, &out)
	if err != nil {
		return out, fmt.Errorf("upstream transport failed")
	}
	return out, nil
}

// statusOf is the upstream HTTP status of an error, 0 when there was no response.
func statusOf(err error) int {
	var e *upstreamError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// pluginabiError is a host-facing error with an HTTP status.
func pluginabiError(code, message string, status int) error {
	return pluginabi.NewError(code, message, status)
}
