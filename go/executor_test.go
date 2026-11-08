package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func executorSender(t *testing.T, chatBody string) *fakeSender {
	t.Helper()
	return &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasSuffix(req.URL, "/api/ide/v1/batch_get_detail_param"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(testListing)}, nil
		case strings.HasSuffix(req.URL, chatPath):
			return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, Body: []byte(chatBody)}, nil
		default:
			return pluginapi.HTTPResponse{}, fmt.Errorf("unexpected request %s", req.URL)
		}
	}}
}

func TestExecuteAggregatesAnswer(t *testing.T) {
	c := testCredential(siteTraeCN)
	c.UID = "exec-1"
	sender := executorSender(t, "event: output\ndata: {\"response\":\"Hello\"}\n\nevent: output\ndata: {\"reasoning_content\":\"why\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	resp, err := execute(pluginapi.ExecutorRequest{
		AuthID: "a", Model: "glm-5.2", StorageJSON: credentialJSON(c),
		Payload: []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
	}, sender)
	if err != nil {
		t.Fatal(err)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(resp.Payload, &completion); err != nil {
		t.Fatal(err)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "Hello" || completion.Choices[0].Message.ReasoningContent != "why" {
		t.Fatalf("completion = %s", resp.Payload)
	}
	if completion.Usage["prompt_tokens"] != float64(3) {
		t.Fatalf("usage = %#v", completion.Usage)
	}
	var sent map[string]any
	for _, req := range sender.requests {
		if strings.HasSuffix(req.URL, chatPath) {
			_ = json.Unmarshal(req.Body, &sent)
			if req.Headers.Get("Authorization") != "Cloud-IDE-JWT token-1" {
				t.Fatalf("authorization = %q", req.Headers.Get("Authorization"))
			}
		}
	}
	if sent["config_name"] != "glm-5.2" || sent["model_name"] != "glm-5.2__dev" {
		t.Fatalf("sent = %#v", sent)
	}
}

func TestExecuteMapsLapsedSignIn(t *testing.T) {
	c := testCredential(siteTraeCN)
	c.UID = "exec-2"
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(req.URL, "/api/ide/v1/batch_get_detail_param") {
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(testListing)}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"code":1001,"message":"not logged in"}`)}, nil
	}}
	_, err := execute(pluginapi.ExecutorRequest{
		AuthID: "a", Model: "glm-5.2", StorageJSON: credentialJSON(c),
		Payload: []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
	}, sender)
	var detail *pluginabi.Error
	if !errors.As(err, &detail) || detail.Code != "reauth_required" || detail.HTTPStatus != 401 {
		t.Fatalf("err = %#v", err)
	}
}

func TestExecuteRetriesAnotherFunction(t *testing.T) {
	c := testCredential(siteTraeCN)
	c.UID = "exec-3"
	calls := 0
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(req.URL, "/api/ide/v1/batch_get_detail_param") {
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(testListing)}, nil
		}
		calls++
		if calls == 1 {
			return pluginapi.HTTPResponse{StatusCode: 400, Body: []byte(`{"code":4001,"message":"wrong function"}`)}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, Body: []byte("event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {}\n\n")}, nil
	}}
	resp, err := execute(pluginapi.ExecutorRequest{
		AuthID: "a", Model: "glm-5.2", StorageJSON: credentialJSON(c),
		Payload: []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
	}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !strings.Contains(string(resp.Payload), `"ok"`) {
		t.Fatalf("calls=%d payload=%s", calls, resp.Payload)
	}
}

func TestExecuteStreamPumpsOpenAIFrames(t *testing.T) {
	c := testCredential(siteTraeCN)
	c.UID = "exec-stream"
	sender := executorSender(t, "")
	oldOpen, oldRead, oldEmit := openHTTPStream, readStreamChunk, emitStreamFrame
	defer func() { openHTTPStream, readStreamChunk, emitStreamFrame = oldOpen, oldRead, oldEmit }()
	openHTTPStream = func(plan requestPlan, fn, callbackID string) (hostStreamResponse, error) {
		return hostStreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, StreamID: "up"}, nil
	}
	chunks := []hostReadResponse{
		{Payload: []byte("event: output\ndata: {\"response\":\"Hi\"}\n\n")},
		{Payload: []byte("event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"), Done: true},
	}
	readStreamChunk = func(string) (hostReadResponse, error) {
		if len(chunks) == 0 {
			return hostReadResponse{Done: true}, nil
		}
		r := chunks[0]
		chunks = chunks[1:]
		return r, nil
	}
	frames := make(chan []byte, 32)
	emitStreamFrame = func(_ string, payload []byte) error {
		frames <- payload
		return nil
	}
	if _, err := executeStream(pluginapi.ExecutorRequest{
		AuthID: "a", Model: "glm-5.2", StorageJSON: credentialJSON(c),
		Payload: []byte(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	}, "cb", "down", sender); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	var all []byte
	for {
		select {
		case frame := <-frames:
			all = append(all, frame...)
			if strings.Contains(string(frame), "[DONE]") {
				goto done
			}
		case <-deadline:
			t.Fatalf("timed out; frames = %s", all)
		}
	}
done:
	if !strings.Contains(string(all), `"content":"Hi"`) || !strings.Contains(string(all), `"finish_reason":"stop"`) {
		t.Fatalf("frames = %s", all)
	}
}
