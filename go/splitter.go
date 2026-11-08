package main

import (
	"encoding/json"
	"strings"
)

// splitResult is what a piece of the model's text turned out to be: the text
// the agent sees, the reasoning it wrote into it, and the tool calls it wrote
// as blocks.
type splitResult struct {
	Text      string
	Reasoning string
	Calls     []traeCall
	Err       string
}

const (
	openCall      = "<tool_call>"
	closeCall     = "</tool_call>"
	openCallShort = "<toolcall>"
	closeShort    = "</toolcall>"
	reasonMarker  = `{"reasoning_content"`
)

// textSplitter splits the model's text into what the agent sees and the tool
// calls it wrote as blocks, holding back the start of a block until it is
// whole. A block cut short at the stream's end is not sent on as the answer:
// the reply ends with an error so the gateway can try again.
type textSplitter struct {
	tools      []chatTool
	buf        string
	said       bool
	called     bool
	broken     bool
	brokenText string
}

func newTextSplitter(tools []chatTool) *textSplitter {
	return &textSplitter{tools: tools}
}

func (s *textSplitter) feed(text string) splitResult {
	s.buf += text
	return s.drain(false)
}

func (s *textSplitter) flush() splitResult {
	return s.drain(true)
}

func (s *textSplitter) drain(end bool) splitResult {
	var out splitResult
	emit := func(text string) {
		if text == "" {
			return
		}
		s.said = true
		out.Text += text
	}
	for {
		// A reasoning object written into the text is reasoning, not the
		// answer: {"reasoning_content": "..."}.
		if at := strings.Index(s.buf, reasonMarker); at >= 0 {
			if stop, ok := jsonObjectEnd(s.buf, at); ok {
				var obj struct {
					Reasoning string `json:"reasoning_content"`
				}
				if json.Unmarshal([]byte(s.buf[at:stop]), &obj) == nil && obj.Reasoning != "" {
					emit(s.buf[:at])
					out.Reasoning += obj.Reasoning
					s.buf = s.buf[stop:]
					continue
				}
			} else if !end {
				emit(s.buf[:at])
				s.buf = s.buf[at:]
				break
			}
		}
		open, closeTag := "", ""
		at := -1
		if i := strings.Index(s.buf, openCall); i >= 0 {
			at, open, closeTag = i, openCall, closeCall
		}
		if i := strings.Index(s.buf, openCallShort); i >= 0 && (at < 0 || i < at) {
			at, open, closeTag = i, openCallShort, closeShort
		}
		if at < 0 {
			hold := heldPrefix(s.buf)
			if end || hold == 0 {
				emit(s.buf)
				s.buf = ""
			} else {
				emit(s.buf[:len(s.buf)-hold])
				s.buf = s.buf[len(s.buf)-hold:]
			}
			break
		}
		if inCodeFence(s.buf, at) {
			// A block in backticks is text, not a call.
			emit(s.buf[:at+len(open)])
			s.buf = s.buf[at+len(open):]
			continue
		}
		start := at + len(open)
		stop := strings.Index(s.buf[start:], closeTag)
		if stop < 0 {
			emit(s.buf[:at])
			s.buf = s.buf[at:]
			if end {
				s.broken = true
				s.brokenText = cutShortText
			}
			break
		}
		inner := s.buf[start : start+stop]
		emit(s.buf[:at])
		call, err := parseTextCall(inner, s.tools)
		if err != "" {
			s.broken = true
			s.brokenText = err
			s.buf = ""
			break
		}
		s.called = true
		out.Calls = append(out.Calls, call)
		s.buf = s.buf[start+stop+len(closeTag):]
	}
	return out
}

const (
	cutShortText = "the model's tool call, written as text, was cut short; it is not sent on as the answer"
	unreadable   = "the model's tool call, written as text, can't be read; it is not sent on as the answer"
)

// heldPrefix is the length of the longest suffix of text that could be the
// start of a tool-call opener, so a chunk boundary inside the tag does not
// send half of it on as the answer.
func heldPrefix(text string) int {
	longest := 0
	for _, tag := range []string{openCall, openCallShort} {
		for n := 1; n < len(tag) && n <= len(text); n++ {
			if strings.HasSuffix(text, tag[:n]) && n > longest {
				longest = n
			}
		}
	}
	return longest
}

// inCodeFence reports whether position i is inside a triple-backtick fence.
func inCodeFence(text string, i int) bool {
	return strings.Count(text[:i], "```")%2 == 1
}

// jsonObjectEnd is where the JSON object starting at i ends, when it is whole.
// Strings and escapes are respected.
func jsonObjectEnd(text string, i int) (int, bool) {
	depth := 0
	inString := false
	escaped := false
	for j := i; j < len(text); j++ {
		c := text[j]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return j + 1, true
			}
		}
	}
	return 0, false
}

// parseTextCall reads a call a model wrote as text: the JSON block agents are
// told to write, or GLM's own template (name then arg_key/arg_value pairs).
func parseTextCall(inner string, tools []chatTool) (traeCall, string) {
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return traeCall{}, unreadable
	}
	name := ""
	var args any
	if strings.HasPrefix(inner, "{") {
		var obj map[string]any
		if json.Unmarshal([]byte(inner), &obj) != nil {
			return traeCall{}, unreadable
		}
		name = firstNonEmpty(rawString(obj["name"]), rawString(obj["tool"]))
		if fn, ok := obj["function"].(map[string]any); ok {
			name = firstNonEmpty(name, rawString(fn["name"]))
			args = fn["arguments"]
		}
		if args == nil {
			for _, key := range []string{"arguments", "input", "parameters", "args"} {
				if v, ok := obj[key]; ok {
					args = v
					break
				}
			}
		}
	} else {
		head, rest, found := strings.Cut(inner, "<arg_key>")
		name = strings.TrimSpace(head)
		if found {
			fields := map[string]any{}
			for {
				key, after, ok := strings.Cut(rest, "</arg_key>")
				if !ok {
					break
				}
				key = strings.TrimSpace(key)
				after = strings.TrimLeft(after, " \t\r\n")
				after = strings.TrimPrefix(after, "<arg_value>")
				value, tail, ok := strings.Cut(after, "</arg_value>")
				if !ok {
					break
				}
				var parsed any
				if json.Unmarshal([]byte(strings.TrimSpace(value)), &parsed) == nil {
					fields[key] = parsed
				} else {
					fields[key] = value
				}
				rest = tail
				next := strings.Index(rest, "<arg_key>")
				if next < 0 {
					break
				}
				rest = rest[next+len("<arg_key>"):]
			}
			args = fields
		}
	}
	if name == "" {
		return traeCall{}, unreadable
	}
	argsText := "{}"
	switch v := args.(type) {
	case nil:
	case string:
		if strings.TrimSpace(v) != "" {
			argsText = v
		}
	default:
		if raw, err := json.Marshal(v); err == nil {
			argsText = string(raw)
		}
	}
	if !json.Valid([]byte(argsText)) {
		argsText = "{}"
	}
	return traeCall{ID: randomCallID(), Name: toolNamed(name, tools), Args: argsText}, ""
}
