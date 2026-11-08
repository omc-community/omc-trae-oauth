package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// traeFailure is a normalized upstream refusal.
type traeFailure struct {
	Status        int
	Code          string
	Message       string
	Lapsed        bool
	Quota         bool
	WrongFunction bool
}

func decisionError(f *traeFailure) error {
	if f == nil {
		return nil
	}
	status := f.Status
	if status == 0 {
		status = 502
	}
	err := pluginabi.NewError(failureCode(f), f.Message, status)
	return err
}

func failureCode(f *traeFailure) string {
	switch {
	case f.Lapsed:
		return "reauth_required"
	case f.Quota || f.Status == 429:
		return "quota_exhausted"
	case f.Status >= 400 && f.Status < 500:
		return "invalid_request"
	default:
		return "upstream_error"
	}
}

// lapsed reports a business code that means the sign-in is over.
func lapsed(spec siteSpec, code string) bool {
	code = strings.TrimSpace(code)
	if code == "" {
		return false
	}
	for _, c := range spec.Lapsed {
		if c == code {
			return true
		}
	}
	return false
}

func quotaCode(spec siteSpec, code string) bool {
	code = strings.TrimSpace(code)
	if code == "" {
		return false
	}
	for _, c := range spec.Quota {
		if c == code {
			return true
		}
	}
	return false
}

func wrongFunction(code string) bool {
	switch strings.TrimSpace(code) {
	case "4001", "4023", "1005":
		return true
	}
	return false
}

// errorCode is the business code in a JSON failure body, "" when it has none.
func errorCode(body []byte) string {
	code, _ := errorOf(body)
	return code
}

var lapsedWords = []string{"not log in", "not logged in", "unauthorized", "token is expired", "token expired", "token invalid", "未登录", "登录已过期", "登录失效"}
var quotaWords = []string{"quota", "credit", "insufficient", "exceed", "limit", "额度", "积分", "次数", "上限", "用完"}

// classifyUpstream maps an HTTP failure or an error frame to a normalized
// failure with the status the client should see.
func classifyUpstream(spec siteSpec, status int, body []byte) *traeFailure {
	if status < 400 || status > 599 {
		status = 502
	}
	code, message := errorOf(body)
	if strings.TrimSpace(message) == "" {
		message = http.StatusText(status)
	}
	if status == 401 || lapsed(spec, code) || (status == 403 && matchesAny(message, lapsedWords...)) {
		return &traeFailure{Status: 401, Code: code, Message: spec.Realm + "'s sign-in has expired (" + message + "); sign in again", Lapsed: true}
	}
	if status == 429 || quotaCode(spec, code) || (status == 403 && matchesAny(message, quotaWords...)) {
		return &traeFailure{Status: 429, Code: code, Message: spec.Realm + ": " + message, Quota: true}
	}
	if status == 400 && wrongFunction(code) {
		return &traeFailure{Status: 502, Code: code, Message: spec.Realm + ": " + message, WrongFunction: true}
	}
	if status == 403 {
		return &traeFailure{Status: 403, Code: code, Message: spec.Realm + ": " + message}
	}
	if status >= 400 && status < 500 {
		return &traeFailure{Status: 400, Code: code, Message: spec.Realm + " refused the request (" + code + "): " + message}
	}
	return &traeFailure{Status: 502, Code: code, Message: spec.Realm + ": " + message}
}

// openAIUsage is an answer's token counts.
type openAIUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int
	ReasoningTokens  int
}

// tokensOf maps Trae's token counts to OpenAI's. Trae's token_usage event says
// what the prompt cache read under Anthropic's names; DeepSeek's
// prompt_cache_hit_tokens and a bare cached_tokens are read too.
func tokensOf(raw map[string]any) *openAIUsage {
	if raw == nil {
		return nil
	}
	n := func(keys ...string) int {
		for _, k := range keys {
			if v, ok := raw[k]; ok {
				if f, ok := number(v); ok {
					return int(f)
				}
			}
		}
		return 0
	}
	prompt := n("prompt_tokens", "input_tokens", "promptTokens", "inputTokens")
	completion := n("completion_tokens", "output_tokens", "completionTokens", "outputTokens")
	if prompt == 0 && completion == 0 {
		return nil
	}
	u := &openAIUsage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: n("total_tokens", "totalTokens")}
	if u.TotalTokens == 0 {
		u.TotalTokens = prompt + completion
	}
	read := 0
	for _, key := range []string{"cache_read_input_tokens", "cacheReadInputTokens", "prompt_cache_hit_tokens", "cached_tokens"} {
		if v, ok := number(raw[key]); ok && int(v) > read {
			read = int(v)
		}
	}
	if details, ok := raw["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := number(details["cached_tokens"]); ok && int(v) > read {
			read = int(v)
		}
	}
	if details, ok := raw["input_tokens_details"].(map[string]any); ok {
		if v, ok := number(details["cached_tokens"]); ok && int(v) > read {
			read = int(v)
		}
	}
	u.CachedTokens = read
	u.ReasoningTokens = n("reasoning_tokens")
	if details, ok := raw["completion_tokens_details"].(map[string]any); ok {
		if v, ok := number(details["reasoning_tokens"]); ok {
			u.ReasoningTokens = int(v)
		}
	}
	return u
}

// stringValue is a string field as it is, without trimming the spaces a stream
// chunk may begin or end with.
func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return rawString(v)
}

func firstString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// traeCall is a tool call the answer asked for.
type traeCall struct {
	ID   string
	Name string
	Args string
}

// traeEvent is one normalized piece of Trae's reply.
type traeEvent struct {
	Text      string
	Reasoning string
	Call      *traeCall
	Usage     *openAIUsage
	Stop      string
	Err       *traeFailure
}

type sseFrame struct {
	Event string
	Data  string
}

// sseDecoder turns arbitrary stream chunks into complete SSE frames.
type sseDecoder struct {
	pending []byte
	event   string
	data    []string
}

func (d *sseDecoder) feed(chunk []byte) []sseFrame {
	d.pending = append(d.pending, chunk...)
	var out []sseFrame
	for {
		i := bytes.IndexByte(d.pending, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(d.pending[:i]), "\r")
		d.pending = d.pending[i+1:]
		out = append(out, d.line(line)...)
	}
	return out
}

func (d *sseDecoder) flush() []sseFrame {
	var out []sseFrame
	if line := strings.TrimSpace(string(d.pending)); line != "" {
		out = append(out, d.line(line)...)
	}
	d.pending = nil
	out = append(out, d.frame()...)
	return out
}

func (d *sseDecoder) line(line string) []sseFrame {
	if line == "" {
		return d.frame()
	}
	if strings.HasPrefix(line, ":") {
		return nil
	}
	if strings.HasPrefix(line, "event:") {
		out := d.frame()
		d.event = strings.TrimSpace(line[len("event:"):])
		return out
	}
	if strings.HasPrefix(line, "data:") {
		d.data = append(d.data, strings.TrimSpace(line[len("data:"):]))
	}
	return nil
}

func (d *sseDecoder) frame() []sseFrame {
	if len(d.data) == 0 {
		d.event = ""
		return nil
	}
	out := sseFrame{Event: d.event, Data: strings.Join(d.data, "\n")}
	d.data = nil
	d.event = ""
	return []sseFrame{out}
}

// eventName normalizes Trae's event names: "TokenUsage" and "token-usage" both
// read as token_usage.
func eventName(s string) string {
	var b strings.Builder
	prevLower := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			if prevLower {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			prevLower = false
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			prevLower = r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		default:
			b.WriteByte('_')
			prevLower = false
		}
	}
	return strings.Trim(b.String(), "_")
}

var answerEvents = map[string]bool{
	"": true, "output": true, "message": true, "delta": true, "content": true, "text": true,
	"answer": true, "chunk": true, "output_delta": true, "message_delta": true,
}

// finishOf is OpenAI's finish_reason for the one Trae's last event gives.
func finishOf(d map[string]any) string {
	f := strings.ToLower(firstNonEmpty(rawString(d["finish_reason"]), rawString(d["stop_reason"]), rawString(d["finishReason"])))
	switch f {
	case "length", "max_tokens", "max_output_tokens", "max_length":
		return "length"
	case "content_filter", "sensitive":
		return "content_filter"
	}
	return ""
}

// traeEvents reads Trae's SSE frames and yields normalized events.
type traeEvents struct {
	site   siteSpec
	dec    sseDecoder
	split  *textSplitter
	calls  *nativeCalls
	tools  []chatTool
	finish string
	usage  *openAIUsage
	said   bool
	done   bool
	closed bool
}

func newTraeEvents(spec siteSpec, tools []chatTool) *traeEvents {
	return &traeEvents{site: spec, split: newTextSplitter(tools), calls: &nativeCalls{}, tools: tools}
}

func (t *traeEvents) feed(chunk []byte) []traeEvent {
	var out []traeEvent
	for _, frame := range t.dec.feed(chunk) {
		out = append(out, t.frame(frame)...)
	}
	return out
}

// flush ends the answer: the text still held, the calls in it and the native
// calls streamed beside it, and the stop with the usage Trae sent after its
// finish.
func (t *traeEvents) flush() []traeEvent {
	var out []traeEvent
	for _, frame := range t.dec.flush() {
		out = append(out, t.frame(frame)...)
	}
	if t.closed {
		return out
	}
	found := t.split.flush()
	out = append(out, t.pieces(found)...)
	if t.split.broken {
		t.closed = true
		return append(out, traeEvent{Err: &traeFailure{Status: 502, Message: t.split.brokenText}})
	}
	for _, c := range t.calls.take(t.tools) {
		t.said = true
		out = append(out, traeEvent{Call: &c})
	}
	t.closed = true
	stop := "stop"
	if t.split.called || len(t.calls.calls) > 0 {
		stop = "tool_calls"
	} else if t.finish == "length" || t.finish == "content_filter" {
		stop = t.finish
	}
	return append(out, traeEvent{Stop: stop, Usage: t.usage})
}

func (t *traeEvents) pieces(found splitResult) []traeEvent {
	var out []traeEvent
	if found.Reasoning != "" {
		t.said = true
		out = append(out, traeEvent{Reasoning: found.Reasoning})
	}
	if found.Text != "" {
		t.said = true
		out = append(out, traeEvent{Text: found.Text})
	}
	for _, c := range found.Calls {
		t.said = true
		out = append(out, traeEvent{Call: &c})
	}
	if found.Err != "" {
		t.closed = true
		out = append(out, traeEvent{Err: &traeFailure{Status: 502, Message: found.Err}})
	}
	return out
}

func (t *traeEvents) frame(frame sseFrame) []traeEvent {
	if t.closed || t.done {
		return nil
	}
	data := strings.TrimSpace(frame.Data)
	if data == "" {
		return nil
	}
	if data == "[DONE]" {
		t.done = true
		return nil
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(data), &parsed) != nil {
		return nil
	}
	name := eventName(frame.Event)
	if name == "" {
		name = eventName(firstNonEmpty(rawString(parsed["event"]), rawString(parsed["type"])))
	}
	if name == "error" || (name != "output" && parsed["code"] != nil && parsed["message"] != nil && parsed["response"] == nil && parsed["content"] == nil) {
		return []traeEvent{{Err: classifyUpstream(t.site, 502, []byte(data))}}
	}
	if name == "token_usage" {
		if usageRaw, ok := parsed["usage"].(map[string]any); ok {
			if u := tokensOf(usageRaw); u != nil {
				t.usage = u
			}
		} else if u := tokensOf(parsed); u != nil {
			t.usage = u
		}
		return nil
	}
	if name == "done" || name == "response_done" || name == "stream_done" {
		if usageRaw, ok := parsed["usage"].(map[string]any); ok {
			if u := tokensOf(usageRaw); u != nil {
				t.usage = u
			}
		}
		if f := finishOf(parsed); f != "" {
			t.finish = f
		}
		t.done = true
		return nil
	}
	if !answerEvents[name] {
		return nil
	}
	var out []traeEvent
	delta, _ := parsed["delta"].(map[string]any)
	reasoning := firstString(stringValue(parsed["reasoning_content"]), stringValue(parsed["reasoning"]))
	if reasoning == "" && delta != nil {
		reasoning = firstString(stringValue(delta["reasoning_content"]), stringValue(delta["reasoning"]))
	}
	if reasoning != "" {
		t.said = true
		out = append(out, traeEvent{Reasoning: reasoning})
	}
	text := firstString(stringValue(parsed["response"]), stringValue(parsed["content"]))
	if text == "" && delta != nil {
		text = firstString(stringValue(delta["content"]))
	}
	if text == "" {
		if s, ok := parsed["delta"].(string); ok {
			text = s
		}
	}
	if text != "" && !strings.HasPrefix(text, "Building prompt:") && !strings.HasPrefix(text, "Completed building prompt") {
		out = append(out, t.pieces(t.split.feed(text))...)
	}
	for _, raw := range asSlice(parsed["tool_calls"]) {
		if tc, ok := raw.(map[string]any); ok {
			t.calls.push(tc)
		}
	}
	if delta != nil {
		for _, raw := range asSlice(delta["tool_calls"]) {
			if tc, ok := raw.(map[string]any); ok {
				t.calls.push(tc)
			}
		}
	}
	if usageRaw, ok := parsed["usage"].(map[string]any); ok {
		if u := tokensOf(usageRaw); u != nil {
			t.usage = u
		}
	}
	if delta != nil {
		if usageRaw, ok := delta["usage"].(map[string]any); ok {
			if u := tokensOf(usageRaw); u != nil {
				t.usage = u
			}
		}
	}
	if f := finishOf(parsed); f != "" {
		t.finish = f
	}
	return out
}

func asSlice(v any) []any {
	if list, ok := v.([]any); ok {
		return list
	}
	return nil
}

// ---- native tool calls -------------------------------------------------------

type nativeCall struct {
	ID    string
	Index *int
	Name  string
	Args  string
}

type nativeCalls struct {
	calls []*nativeCall
}

func callField(tc map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := tc[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

func callArgs(tc map[string]any) string {
	for _, k := range []string{"arguments", "args", "params", "input", "parameters"} {
		if v, ok := tc[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
			raw, _ := json.Marshal(v)
			return string(raw)
		}
	}
	return ""
}

func (n *nativeCalls) push(tc map[string]any) {
	fn, _ := tc["function"].(map[string]any)
	if fn == nil {
		fn, _ = tc["function_call"].(map[string]any)
	}
	name := firstNonEmpty(rawString(tc["name"]), rawString(tc["tool_name"]))
	args := callArgs(tc)
	if fn != nil {
		name = firstNonEmpty(name, rawString(fn["name"]))
		args = firstNonEmpty(callArgs(fn), args)
	}
	id := firstNonEmpty(rawString(tc["id"]), rawString(tc["tool_call_id"]))
	var index *int
	if v, ok := number(tc["index"]); ok {
		i := int(v)
		index = &i
	}
	if name == "" && id == "" && args == "" && index == nil {
		return
	}
	var call *nativeCall
	for _, c := range n.calls {
		if id != "" && c.ID == id {
			call = c
			break
		}
		if id == "" && index != nil && c.Index != nil && *c.Index == *index {
			call = c
			break
		}
	}
	if call == nil && id == "" && index == nil {
		for i := len(n.calls) - 1; i >= 0; i-- {
			c := n.calls[i]
			if c.Name == name && (c.Args == args || !json.Valid([]byte(c.Args)) && strings.HasPrefix(args, c.Args)) {
				call = c
				break
			}
		}
	}
	if call == nil && name == "" && id == "" {
		for i := len(n.calls) - 1; i >= 0; i-- {
			if !json.Valid([]byte(n.calls[i].Args)) {
				call = n.calls[i]
				break
			}
		}
	}
	if call == nil {
		if name == "" {
			return
		}
		call = &nativeCall{}
		n.calls = append(n.calls, call)
	}
	if call.Name == "" {
		call.Name = name
	} else if name != "" && strings.HasPrefix(name, call.Name) && name != call.Name {
		call.Name = name
	}
	if call.ID == "" && id != "" {
		call.ID = id
	}
	if call.Index == nil && index != nil {
		call.Index = index
	}
	if args == "" || strings.HasPrefix(call.Args, args) {
		return
	}
	switch {
	case strings.HasPrefix(args, call.Args), json.Valid([]byte(call.Args)):
		call.Args = args
	default:
		call.Args += args
	}
}

func (n *nativeCalls) take(tools []chatTool) []traeCall {
	var out []traeCall
	for _, c := range n.calls {
		name := toolNamed(c.Name, tools)
		if strings.TrimSpace(name) == "" {
			continue
		}
		args := strings.TrimSpace(c.Args)
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		id := c.ID
		if id == "" {
			id = randomCallID()
		}
		out = append(out, traeCall{ID: id, Name: name, Args: args})
	}
	return out
}

// toolNamed maps a call's name to the request's tool when they differ only by
// case or punctuation; any other name goes on as written.
func toolNamed(name string, tools []chatTool) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	for _, t := range tools {
		if t.Function.Name == name {
			return name
		}
		if norm(t.Function.Name) == norm(name) {
			return t.Function.Name
		}
	}
	return name
}

func randomCallID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "call_000000000000000000000000"
	}
	return "call_" + hex.EncodeToString(b)
}

// randomChatID is the id an OpenAI answer carries.
func randomChatID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "chatcmpl-000000000000000000000000"
	}
	return "chatcmpl-" + hex.EncodeToString(b)
}
