package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// streamAnswer turns normalized events into OpenAI chat-completion chunks.
type streamAnswer struct {
	id       string
	model    string
	created  int64
	roleSent bool
	finished bool
	emitted  bool
	content  bool
	calls    int
}

func newStreamAnswer(model string) *streamAnswer {
	return &streamAnswer{id: randomChatID(), model: model, created: time.Now().Unix()}
}

func (a *streamAnswer) frame(delta map[string]any, finish any, usage *openAIUsage) []byte {
	chunk := map[string]any{
		"id":      a.id,
		"object":  "chat.completion.chunk",
		"created": a.created,
		"model":   a.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		chunk["usage"] = usageJSON(usage)
	}
	raw, _ := json.Marshal(chunk)
	return append(append([]byte("data: "), raw...), '\n', '\n')
}

func (a *streamAnswer) preamble() [][]byte {
	if a.roleSent {
		return nil
	}
	a.roleSent = true
	a.emitted = true
	return [][]byte{a.frame(map[string]any{"role": "assistant", "content": ""}, nil, nil)}
}

// frames returns the SSE frames one event produces.
func (a *streamAnswer) frames(e traeEvent) [][]byte {
	if e.Err != nil || a.finished {
		return nil
	}
	switch {
	case e.Text != "":
		out := a.preamble()
		a.emitted = true
		a.content = true
		return append(out, a.frame(map[string]any{"content": e.Text}, nil, nil))
	case e.Reasoning != "":
		out := a.preamble()
		a.emitted = true
		a.content = true
		return append(out, a.frame(map[string]any{"reasoning_content": e.Reasoning}, nil, nil))
	case e.Call != nil:
		out := a.preamble()
		a.emitted = true
		a.content = true
		call := map[string]any{
			"index": a.calls,
			"id":    e.Call.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      e.Call.Name,
				"arguments": e.Call.Args,
			},
		}
		a.calls++
		return append(out, a.frame(map[string]any{"tool_calls": []any{call}}, nil, nil))
	case e.Stop != "":
		out := a.preamble()
		a.finished = true
		a.emitted = true
		return append(out, a.frame(map[string]any{}, e.Stop, e.Usage))
	}
	return nil
}

// saidSomething reports whether the answer carried anything the client keeps.
func (a *streamAnswer) saidSomething() bool { return a.content }

// done is the terminal frame of the stream.
func (a *streamAnswer) done() []byte {
	if !a.emitted {
		return nil
	}
	return []byte("data: [DONE]\n\n")
}

func usageJSON(u *openAIUsage) map[string]any {
	if u == nil {
		return nil
	}
	out := map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
	if u.CachedTokens > 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CachedTokens}
	}
	if u.ReasoningTokens > 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.ReasoningTokens}
	}
	return out
}

type completionMessage struct {
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []completionCall `json:"tool_calls,omitempty"`
}

type completionCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type completionResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int               `json:"index"`
		Message completionMessage `json:"message"`
		Finish  string            `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]any `json:"usage,omitempty"`
}

// completionFromEvents assembles one non-streaming answer.
func completionFromEvents(model string, events []traeEvent) ([]byte, *traeFailure) {
	var text, reasoning, stop string
	var usage *openAIUsage
	var calls []completionCall
	for _, e := range events {
		if e.Err != nil {
			return nil, e.Err
		}
		text += e.Text
		reasoning += e.Reasoning
		if e.Call != nil {
			call := completionCall{ID: e.Call.ID, Type: "function"}
			call.Function.Name = e.Call.Name
			call.Function.Arguments = e.Call.Args
			calls = append(calls, call)
		}
		if e.Stop != "" {
			stop = e.Stop
			usage = e.Usage
		}
	}
	if strings.TrimSpace(text) == "" && reasoning == "" && len(calls) == 0 {
		return nil, &traeFailure{Status: 502, Message: "Trae ended without an answer"}
	}
	if stop == "" {
		stop = "stop"
	}
	if len(calls) > 0 {
		stop = "tool_calls"
	}
	msg := completionMessage{Role: "assistant", Content: text, ReasoningContent: reasoning}
	msg.ToolCalls = calls
	resp := completionResponse{ID: randomChatID(), Object: "chat.completion", Created: time.Now().Unix(), Model: model}
	resp.Choices = []struct {
		Index   int               `json:"index"`
		Message completionMessage `json:"message"`
		Finish  string            `json:"finish_reason"`
	}{{Index: 0, Message: msg, Finish: stop}}
	resp.Usage = usageJSON(usage)
	if resp.Usage == nil {
		resp.Usage = map[string]any{}
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, &traeFailure{Status: 500, Message: fmt.Sprintf("cannot encode answer: %v", err)}
	}
	return raw, nil
}
