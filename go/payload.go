package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type chatToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function chatFunction `json:"function"`
}

type chatTool struct {
	Type     string       `json:"type,omitempty"`
	Function chatFunction `json:"function"`
}

type chatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content,omitempty"`
	Name             string          `json:"name,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolCalls        []chatToolCall  `json:"tool_calls,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
}

// chatRequest is the OpenAI chat completion the host hands the executor.
type chatRequest struct {
	Model               string          `json:"model"`
	Stream              bool            `json:"stream,omitempty"`
	Messages            []chatMessage   `json:"messages"`
	Tools               []chatTool      `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	PromptCacheKey      string          `json:"prompt_cache_key,omitempty"`
	User                string          `json:"user,omitempty"`
}

func parseChatRequest(raw []byte) (chatRequest, error) {
	var chat chatRequest
	if err := json.Unmarshal(raw, &chat); err != nil {
		return chat, errors.New("invalid chat completion request")
	}
	if len(chat.Messages) == 0 {
		return chat, errors.New("messages array required")
	}
	return chat, nil
}

// contentPart is one part of an OpenAI message's content.
type contentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL json.RawMessage `json:"image_url,omitempty"`
	URL      string          `json:"url,omitempty"`
}

// contentText is a message's text, with a placeholder for an image the model
// can't see.
func contentText(raw json.RawMessage, sees bool) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []contentPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			if p.Text != "" {
				out = append(out, p.Text)
			}
		case "image_url", "input_image":
			if !sees {
				out = append(out, "[an image, which this model can't see]")
			}
		}
	}
	return strings.Join(out, "\n")
}

type traeImage struct {
	URL string `json:"url"`
}

type traePart struct {
	Type     string     `json:"type"`
	Text     string     `json:"text,omitempty"`
	ImageURL *traeImage `json:"image_url,omitempty"`
}

// contentImages is a message's images, as Trae's chat takes them.
func contentImages(raw json.RawMessage) []traePart {
	var parts []contentPart
	if len(raw) == 0 || json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var out []traePart
	for _, p := range parts {
		if p.Type != "image_url" && p.Type != "input_image" {
			continue
		}
		url := p.URL
		if len(p.ImageURL) > 0 {
			var s string
			if json.Unmarshal(p.ImageURL, &s) == nil {
				url = s
			} else {
				var obj struct {
					URL string `json:"url"`
				}
				if json.Unmarshal(p.ImageURL, &obj) == nil {
					url = obj.URL
				}
			}
		}
		if strings.TrimSpace(url) != "" {
			out = append(out, traePart{Type: "image_url", ImageURL: &traeImage{URL: url}})
		}
	}
	return out
}

type traeFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type traeOutCall struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	FunctionCall traeFunctionCall `json:"function_call"`
}

type traeMessage struct {
	Role       string        `json:"role"`
	Content    any           `json:"content"`
	ToolCalls  []traeOutCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

// draft is a message before it is folded and rendered for Trae.
type draft struct {
	role       string
	text       string
	images     []traePart
	calls      []traeOutCall
	toolCallID string
}

// buildMessages turns the OpenAI conversation into Trae's. sees is whether the
// model takes images. native is whether the body names the tools to Trae: past
// calls go back as Trae's own turns only then; a request without them keeps the
// text form.
func buildMessages(chat chatRequest, sees, native bool) []traeMessage {
	names := map[string]string{}
	dropped := map[string]bool{}
	var out []draft
	for _, m := range chat.Messages {
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		text := contentText(m.Content, sees)
		// A model Trae says doesn't take images is sent the text placeholder,
		// never the image itself.
		var images []traePart
		if sees {
			images = contentImages(m.Content)
		}
		if !native && role == "assistant" && len(m.ToolCalls) > 0 {
			var blocks []string
			for _, c := range m.ToolCalls {
				name := strings.TrimSpace(c.Function.Name)
				if name == "" {
					continue
				}
				names[c.ID] = name
				args := argumentsText(c.Function.Arguments)
				blocks = append(blocks, "<tool_call>"+`{"name":`+quoteJSON(name)+`,"arguments":`+args+`}`+"</tool_call>")
			}
			if text != "" {
				blocks = append([]string{text}, blocks...)
			}
			out = append(out, draft{role: "assistant", text: strings.Join(blocks, "\n")})
			continue
		}
		if role == "assistant" && len(m.ToolCalls) > 0 {
			var calls []traeOutCall
			for _, c := range m.ToolCalls {
				name := strings.TrimSpace(c.Function.Name)
				if name == "" {
					if c.ID != "" {
						dropped[c.ID] = true
					}
					continue
				}
				names[c.ID] = name
				id := c.ID
				if id == "" {
					id = "call_" + randomHex(8)
				}
				calls = append(calls, traeOutCall{ID: id, Type: "function", FunctionCall: traeFunctionCall{Name: name, Arguments: argumentsText(c.Function.Arguments)}})
			}
			if len(calls) > 0 || text != "" || len(images) > 0 {
				out = append(out, draft{role: "assistant", text: text, images: images, calls: calls})
			}
			continue
		}
		if role == "tool" {
			id := strings.TrimSpace(m.ToolCallID)
			if dropped[id] {
				continue
			}
			name := names[id]
			if !native || name == "" {
				out = append(out, draft{role: "user", text: "Result of " + firstNonEmpty(name, m.Name, "the tool") + " (call " + firstNonEmpty(id, "?") + "):\n" + text, images: images})
				continue
			}
			result := text
			if result == "" {
				if len(images) > 0 {
					result = "(the result's image follows)"
				} else {
					result = "(no output)"
				}
			}
			out = append(out, draft{role: "tool", text: result, toolCallID: id, images: images})
			continue
		}
		if role == "assistant" {
			out = append(out, draft{role: "assistant", text: text, images: images})
			continue
		}
		if role == "system" {
			out = append(out, draft{role: "system", text: text, images: images})
			continue
		}
		out = append(out, draft{role: "user", text: text, images: images})
	}
	// Trae takes no two messages of one role in a row, so an assistant turn is
	// folded into the turn after it: text into the calls turn it introduces,
	// and parallel calls joined into one list. Tool results stay turns of their
	// own, each answering its own call.
	var merged []draft
	pendingImages := []traePart{}
	flushImages := func() {
		if len(pendingImages) > 0 {
			merged = append(merged, draft{role: "user", images: pendingImages})
			pendingImages = nil
		}
	}
	for _, m := range out {
		if m.role == "tool" {
			if len(m.images) > 0 {
				pendingImages = append(pendingImages, m.images...)
			}
			merged = append(merged, draft{role: "tool", text: m.text, toolCallID: m.toolCallID})
			continue
		}
		flushImages()
		if m.role == "assistant" {
			if last := lastOf(merged, "assistant"); last != nil {
				last.text = joinText(last.text, m.text)
				last.images = append(last.images, m.images...)
				last.calls = append(last.calls, m.calls...)
				if last.text == "" && len(last.images) == 0 && len(last.calls) == 0 {
					merged = merged[:len(merged)-1]
				}
				continue
			}
			if m.text == "" && len(m.images) == 0 && len(m.calls) == 0 {
				continue
			}
			merged = append(merged, m)
			continue
		}
		if m.text == "" && len(m.images) == 0 {
			continue
		}
		if last := lastOf(merged, m.role); last != nil && m.role != "system" {
			last.text = joinText(last.text, m.text)
			last.images = append(last.images, m.images...)
			continue
		}
		merged = append(merged, m)
	}
	flushImages()
	var messages []traeMessage
	for _, m := range merged {
		content := make([]traePart, 0, 1+len(m.images))
		if m.text != "" {
			content = append(content, traePart{Type: "text", Text: m.text})
		}
		content = append(content, m.images...)
		msg := traeMessage{Role: m.role, ToolCallID: m.toolCallID}
		if len(content) > 0 {
			msg.Content = content
		} else {
			msg.Content = nil
		}
		msg.ToolCalls = m.calls
		messages = append(messages, msg)
	}
	return messages
}

func lastOf(items []draft, role string) *draft {
	if len(items) == 0 || items[len(items)-1].role != role {
		return nil
	}
	return &items[len(items)-1]
}

func joinText(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\n\n" + b
}

// argumentsText is a tool call's arguments as Trae takes them: a JSON string.
func argumentsText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return "{}"
		}
		return s
	}
	return strings.TrimSpace(string(raw))
}

func quoteJSON(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// nativeTools are the tools as Trae names them: parameters a JSON string.
func nativeTools(tools []chatTool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		f := t.Function
		params := "{}"
		if len(f.Parameters) > 0 && string(f.Parameters) != "null" {
			var s string
			if json.Unmarshal(f.Parameters, &s) == nil {
				params = s
			} else {
				params = string(f.Parameters)
			}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        f.Name,
				"description": f.Description,
				"parameters":  params,
			},
		})
	}
	return out
}

// toolChoice is the tool choice as Trae takes it: "none" also turns the native
// form off for past calls.
func toolChoice(raw json.RawMessage) (value any, native bool) {
	native = true
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.EqualFold(strings.TrimSpace(s), "none") {
			return "none", false
		}
		return strings.TrimSpace(s), true
	}
	var obj struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if n := firstNonEmpty(obj.Function.Name, obj.Name); n != "" {
			return n, true
		}
	}
	return nil, true
}

// requestedEffort maps an OpenAI effort to Trae's name, when the model takes
// that level. Trae rejects a level a model has no entry for, so nothing is sent
// instead.
func requestedEffort(levels []string, effort string) string {
	effort = strings.TrimSpace(effort)
	if effort == "" || len(levels) == 0 {
		return ""
	}
	want := effort
	if mapped, ok := effortNames[effort]; ok {
		want = mapped
	}
	for _, level := range levels {
		if level == want {
			for trae, mapped := range effortNames {
				if mapped == want {
					return trae
				}
			}
		}
	}
	return ""
}

// sessionID is the conversation Trae counts a request under. The id is a hash
// of the session the host names, else the client's prompt_cache_key or user,
// else the conversation's opening, together with the account answering it.
func sessionID(chat chatRequest, headers http.Header, uid string) string {
	seed := ""
	for _, key := range []string{"X-Session-Id", "X-Session-Affinity", "X-Magpie-Trae-Session"} {
		if v := strings.TrimSpace(headers.Get(key)); v != "" {
			seed = "session\x00" + v
			break
		}
	}
	if seed == "" {
		switch {
		case strings.TrimSpace(chat.PromptCacheKey) != "":
			seed = "key\x00" + strings.TrimSpace(chat.PromptCacheKey)
		case strings.TrimSpace(chat.User) != "":
			seed = "user\x00" + strings.TrimSpace(chat.User)
		default:
			var systems []string
			var first string
			for _, m := range chat.Messages {
				if m.Role == "system" || m.Role == "developer" {
					systems = append(systems, contentText(m.Content, true))
					continue
				}
				if first == "" && m.Role == "user" {
					first = contentText(m.Content, true)
				}
			}
			raw, _ := json.Marshal([]any{systems, first})
			seed = "chat\x00" + string(raw)
		}
	}
	sum := sha256.Sum256([]byte(seed + "\x00" + uid))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return uuidString(b)
}

// chatBody is the request for llm_utils_chat. The model is asked through fn,
// under the config name Trae routes on, and with the __dev or __max model the
// function's list names for it when it names one.
func chatBody(chat chatRequest, m traeModel, fn, session string, sees bool) ([]byte, error) {
	nativeChoice, native := toolChoice(chat.ToolChoice)
	native = native && len(chat.Tools) > 0
	config := m.ID
	modelName := ""
	most := m.DevMax
	window := int64(0)
	if m.Base != "" {
		config = m.Base
		modelName = m.DevName
		if m.Max != nil {
			window = m.Max.Context
			if m.Max.MaxTokens > 0 {
				most = m.Max.MaxTokens
			}
		}
	} else if m.DevName != "" {
		modelName = m.DevName
	}
	body := map[string]any{
		"messages":    buildMessages(chat, sees, native),
		"function":    fn,
		"config_name": config,
		"model":       config,
		"stream":      true,
		"request_id":  uuidString(mustNonce()),
		"session_id":  session,
	}
	// A __dev model is the one its own function serves; another function's
	// list may name the model differently, so it is sent only through the
	// function that listed it. A Max name goes with every attempt.
	if modelName != "" && (m.Function == "" || fn == m.Function || m.Base != "") {
		body["model_name"] = modelName
	}
	if effort := requestedEffort(m.Efforts, chat.ReasoningEffort); effort != "" {
		body["reasoning_effort"] = effort
	}
	asked := chat.MaxCompletionTokens
	if asked == 0 {
		asked = chat.MaxTokens
	}
	if asked > 0 {
		if most > 0 && int64(asked) > most {
			asked = int(most)
		}
		body["max_tokens"] = asked
	} else if m.Base != "" && most > 0 {
		body["max_tokens"] = most
	}
	if window > 0 {
		max := int64(0)
		if v, ok := body["max_tokens"].(int); ok {
			max = int64(v)
		}
		body["user_message_context"] = map[string]any{"model_info": map[string]any{"prompt_max_tokens": window - max}}
	}
	if chat.Temperature != nil {
		body["temperature"] = *chat.Temperature
	}
	if native {
		body["tools"] = nativeTools(chat.Tools)
		if nativeChoice != nil {
			body["tool_choice"] = nativeChoice
		}
		if chat.ParallelToolCalls != nil {
			body["parallel_tool_calls"] = *chat.ParallelToolCalls
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("cannot encode the Trae request")
	}
	return raw, nil
}

func mustNonce() []byte {
	b, err := newNonce()
	if err != nil {
		return make([]byte, 16)
	}
	return b
}

// ideHeaders are the IDE's, for the account and its device.
func ideHeaders(c credential, spec siteSpec, extra map[string]string) http.Header {
	h := http.Header{
		"Content-Type":       {"application/json"},
		"Authorization":      {"Cloud-IDE-JWT " + c.Access},
		"X-Cloudide-Token":   {c.Access},
		"x-ide-token":        {c.Access},
		"x-uid":              {c.UID},
		"x-app-id":           {spec.AppID},
		"x-device-id":        {c.DeviceID},
		"x-machine-id":       {c.MachineID},
		"x-request-id":       {uuidString(mustNonce())},
		"x-app-version":      {getConfig().ClientVersion},
		"x-app-version-code": {defaultClientCode},
		"x-ide-version":      {getConfig().ClientVersion},
		"x-ide-version-code": {defaultClientCode},
		"x-ide-version-type": {"stable"},
		"x-device-cpu":       {"AMD"},
		"x-device-brand":     {spec.Brand},
		"x-device-type":      {"windows"},
		"x-os-version":       {"Windows 10"},
		"x-system-type":      {"Windows"},
	}
	for k, v := range extra {
		h.Set(k, v)
	}
	return h
}
