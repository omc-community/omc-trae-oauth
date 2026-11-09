package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func collect(events *traeEvents, chunks ...string) []traeEvent {
	var out []traeEvent
	for _, c := range chunks {
		out = append(out, events.feed([]byte(c))...)
	}
	return append(out, events.flush()...)
}

func joined(events []traeEvent) (text, reasoning string, calls []traeCall, usage *openAIUsage, stop string, failure *traeFailure) {
	for _, e := range events {
		text += e.Text
		reasoning += e.Reasoning
		if e.Call != nil {
			calls = append(calls, *e.Call)
		}
		if e.Usage != nil {
			usage = e.Usage
		}
		if e.Stop != "" {
			stop = e.Stop
		}
		if e.Err != nil {
			failure = e.Err
		}
	}
	return
}

func TestEventsReadTextReasoningAndUsage(t *testing.T) {
	spec := mustSite(t, siteTraeCN)
	events := newTraeEvents(spec, nil)
	out := collect(events,
		"event: output\ndata: {\"response\":\"Hello \"}\n\n",
		"event: output\ndata: {\"reasoning_content\":\"think\"}\n\n",
		"event: token_usage\ndata: {\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12,\"cache_read_input_tokens\":4}\n\n",
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n",
	)
	text, reasoning, calls, usage, stop, failure := joined(out)
	if failure != nil {
		t.Fatal(failure.Message)
	}
	if text != "Hello " || reasoning != "think" || len(calls) != 0 || stop != "stop" {
		t.Fatalf("text=%q reasoning=%q calls=%v stop=%q", text, reasoning, calls, stop)
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 2 || usage.CachedTokens != 4 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestEventsReadNativeToolCall(t *testing.T) {
	events := newTraeEvents(mustSite(t, siteTraeCN), []chatTool{{Function: chatFunction{Name: "bash"}}})
	out := collect(events,
		"event: output\ndata: {\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"ls\\\"}\"}}]}\n\n",
		"event: done\ndata: {}\n\n",
	)
	_, _, calls, _, stop, failure := joined(out)
	if failure != nil {
		t.Fatal(failure.Message)
	}
	if len(calls) != 1 || calls[0].Name != "bash" || calls[0].Args != `{"command":"ls"}` || calls[0].ID != "call_1" {
		t.Fatalf("calls = %#v", calls)
	}
	if stop != "tool_calls" {
		t.Fatalf("stop = %q", stop)
	}
}

func TestTextToolCallBlocks(t *testing.T) {
	events := newTraeEvents(mustSite(t, siteTraeCN), []chatTool{{Function: chatFunction{Name: "read"}}})
	out := collect(events, "event: output\ndata: {\"response\":\"<tool_call>{\\\"name\\\":\\\"read\\\",\\\"arguments\\\":{\\\"path\\\":\\\"a\\\"}}</tool_call>\"}\n\n")
	_, _, calls, _, _, failure := joined(out)
	if failure != nil {
		t.Fatal(failure.Message)
	}
	if len(calls) != 1 || calls[0].Name != "read" || calls[0].Args != `{"path":"a"}` {
		t.Fatalf("calls = %#v", calls)
	}

	glm := newTraeEvents(mustSite(t, siteTraeCN), []chatTool{{Function: chatFunction{Name: "bash"}}})
	out = collect(glm, "event: output\ndata: {\"response\":\"<tool_call>bash<arg_key>command</arg_key><arg_value>ls -la</arg_value></tool_call>\"}\n\n")
	_, _, calls, _, _, failure = joined(out)
	if failure != nil {
		t.Fatal(failure.Message)
	}
	if len(calls) != 1 || calls[0].Name != "bash" || calls[0].Args != `{"command":"ls -la"}` {
		t.Fatalf("glm calls = %#v", calls)
	}
}

func TestCutShortToolCallIsAnError(t *testing.T) {
	events := newTraeEvents(mustSite(t, siteTraeCN), nil)
	out := collect(events, "event: output\ndata: {\"response\":\"<tool_call>{\\\"name\\\":\\\"bash\\\"\"}\n\n")
	_, _, _, _, _, failure := joined(out)
	if failure == nil || !strings.Contains(failure.Message, "cut short") {
		t.Fatalf("failure = %#v", failure)
	}
}

func TestBuildMessagesNative(t *testing.T) {
	chat := chatRequest{Messages: []chatMessage{
		{Role: "system", Content: json.RawMessage(`"sys"`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]`)},
		{Role: "assistant", Content: json.RawMessage(`"let me look"`)},
		{Role: "assistant", ToolCalls: []chatToolCall{{ID: "c1", Function: chatFunction{Name: "read", Arguments: json.RawMessage(`{"path":"a"}`)}}}},
		{Role: "tool", ToolCallID: "c1", Content: json.RawMessage(`"contents"`)},
		{Role: "user", Content: json.RawMessage(`"next"`)},
	}}
	messages := buildMessages(chat, true, true)
	if len(messages) != 5 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].Role != "system" || messages[1].Role != "user" || messages[2].Role != "assistant" || messages[3].Role != "tool" || messages[4].Role != "user" {
		t.Fatalf("roles = %#v", messages)
	}
	if messages[2].ToolCalls == nil {
		t.Fatalf("assistant call missing: %#v", messages[2])
	}
	userParts := messages[1].Content.([]traePart)
	if len(userParts) != 2 || userParts[0].Text != "hi" || userParts[1].ImageURL == nil {
		t.Fatalf("user parts = %#v", userParts)
	}
	assistantParts := messages[2].Content.([]traePart)
	if len(assistantParts) != 1 || assistantParts[0].Text != "let me look" {
		t.Fatalf("assistant parts = %#v", assistantParts)
	}
	if messages[3].ToolCallID != "c1" {
		t.Fatalf("tool = %#v", messages[3])
	}
}

func TestBuildMessagesTextFormKeepsCallsAsText(t *testing.T) {
	chat := chatRequest{Messages: []chatMessage{
		{Role: "assistant", ToolCalls: []chatToolCall{{ID: "c1", Function: chatFunction{Name: "read", Arguments: json.RawMessage(`{"path":"a"}`)}}}},
		{Role: "tool", ToolCallID: "c1", Content: json.RawMessage(`"contents"`)},
	}}
	messages := buildMessages(chat, true, false)
	if len(messages) != 2 || messages[0].Role != "assistant" || messages[1].Role != "user" {
		t.Fatalf("messages = %#v", messages)
	}
	parts := messages[0].Content.([]traePart)
	if !strings.Contains(parts[0].Text, "<tool_call>") || !strings.Contains(parts[0].Text, `"name":"read"`) {
		t.Fatalf("text = %q", parts[0].Text)
	}
	result := messages[1].Content.([]traePart)
	if !strings.Contains(result[0].Text, "Result of read") {
		t.Fatalf("result = %q", result[0].Text)
	}
}

func TestChatBodySendsDevModelAndEffort(t *testing.T) {
	chat := chatRequest{
		Messages:        []chatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		ReasoningEffort: "low",
		MaxTokens:       64000,
	}
	m := traeModel{ID: "glm-5.2", DevName: "glm-5.2__dev", DevMax: 32000, Efforts: []string{"low", "high", "xhigh"}}
	raw, err := chatBody(chat, m, "chat_v3", "session-1", true)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["config_name"] != "glm-5.2" || body["model_name"] != "glm-5.2__dev" || body["function"] != "chat_v3" {
		t.Fatalf("body = %#v", body)
	}
	if body["reasoning_effort"] != "light" {
		t.Fatalf("effort = %#v", body["reasoning_effort"])
	}
	if body["max_tokens"] != float64(32000) {
		t.Fatalf("max_tokens = %#v", body["max_tokens"])
	}
	if body["stream"] != true || body["session_id"] != "session-1" {
		t.Fatalf("body = %#v", body)
	}
}

func TestSessionIDIsStableAndAccountScoped(t *testing.T) {
	chat := chatRequest{Messages: []chatMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}}}
	a := sessionID(chat, http.Header{}, "u1")
	b := sessionID(chat, http.Header{}, "u1")
	c := sessionID(chat, http.Header{}, "u2")
	if a != b || a == c {
		t.Fatalf("ids = %q %q %q", a, b, c)
	}
	if !strings.Contains(a, "-") || len(a) != 36 {
		t.Fatalf("id = %q", a)
	}
}

func TestClassifyUpstream(t *testing.T) {
	spec := mustSite(t, siteTraeCN)
	cases := []struct {
		name   string
		status int
		body   string
		want   int
		lapsed bool
		quota  bool
		wrong  bool
	}{
		{"lapsed code", 200, `{"code":1001,"message":"not logged in"}`, 401, true, false, false},
		{"unauthorized", 401, `{}`, 401, true, false, false},
		{"quota code", 200, `{"code":1005,"message":"quota exceeded"}`, 429, false, true, false},
		{"rate limit", 429, `{}`, 429, false, true, false},
		{"wrong function", 400, `{"code":4001,"message":"bad function"}`, 502, false, false, true},
		{"bad request", 400, `{"code":123,"message":"bad"}`, 400, false, false, false},
		{"server", 500, `{}`, 502, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := classifyUpstream(spec, tc.status, nil, []byte(tc.body))
			if f.Status != tc.want || f.Lapsed != tc.lapsed || f.Quota != tc.quota || f.WrongFunction != tc.wrong {
				t.Fatalf("failure = %#v", f)
			}
		})
	}
}

// TestQuotaRefusalCarriesTheRetryWindow pins what a quota refusal hands CPA: the
// Retry-After Trae sends is the only statement of when the allowance returns, and
// without it the credential is retried while the quota is still spent.
func TestQuotaRefusalCarriesTheRetryWindow(t *testing.T) {
	spec := mustSite(t, siteTraeCN)
	at := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    int
	}{
		{"seconds", http.Header{"Retry-After": {"120"}}, 120},
		{"http date", http.Header{"Retry-After": {at}}, 90},
		{"no statement", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := classifyUpstream(spec, 429, tc.headers, []byte(`{}`))
			if !f.Quota || f.Status != 429 {
				t.Fatalf("failure = %#v", f)
			}
			if tc.want == 0 {
				if f.RetryAfter != 0 {
					t.Fatalf("RetryAfter = %d, want none", f.RetryAfter)
				}
				return
			}
			if f.RetryAfter < tc.want-2 || f.RetryAfter > tc.want {
				t.Fatalf("RetryAfter = %d, want about %d", f.RetryAfter, tc.want)
			}
		})
	}
}

// TestQuotaRefusalIsCredentialScoped pins the scope Trae's refusal carries: the
// account's entitlement packs cover every model, so a spent quota cools the
// whole credential; a lapsed sign-in or a refused function stays model-scoped.
func TestQuotaRefusalIsCredentialScoped(t *testing.T) {
	spec := mustSite(t, siteTraeCN)
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"quota 429", 429, `{}`, true},
		{"quota code", 200, `{"code":1005,"message":"quota exceeded"}`, true},
		{"lapsed sign-in", 401, `{}`, false},
		{"wrong function", 400, `{"code":4001,"message":"bad function"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := classifyUpstream(spec, tc.status, nil, []byte(tc.body))
			var perr *pluginabi.Error
			if err := decisionError(f); !errors.As(err, &perr) || perr.CredentialScoped != tc.want {
				t.Fatalf("decisionError = %#v", err)
			}
		})
	}
}

func TestBuildMessagesHidesImagesFromTextOnlyModel(t *testing.T) {
	chat := chatRequest{Messages: []chatMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]`)},
	}}
	messages := buildMessages(chat, false, true)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	parts := messages[0].Content.([]traePart)
	if len(parts) != 1 || !strings.Contains(parts[0].Text, "can't see") {
		t.Fatalf("parts = %#v", parts)
	}
}
