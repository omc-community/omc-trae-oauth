package main

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// chatPath is the SSE endpoint Trae's IDE agent talks to.
const chatPath = "/api/agent/v3/llm_utils_chat"

type credentialGate struct {
	cond   *sync.Cond
	active int
}

var gateMu sync.Mutex
var gates = map[string]*credentialGate{}

// acquire bounds simultaneous requests per credential.
func acquire(id string) func() {
	gateMu.Lock()
	g := gates[id]
	if g == nil {
		g = &credentialGate{cond: sync.NewCond(&gateMu)}
		gates[id] = g
	}
	for g.active >= getConfig().MaxConcurrency {
		g.cond.Wait()
	}
	g.active++
	gateMu.Unlock()
	return func() { gateMu.Lock(); g.active--; g.cond.Broadcast(); gateMu.Unlock() }
}

type requestPlan struct {
	Chat       chatRequest
	Model      traeModel
	ModelName  string
	Functions  []string
	Session    string
	Sees       bool
	Credential credential
	Spec       siteSpec
}

// prepareRequest turns an OpenAI chat completion into the request Trae's IDE
// agent sends.
func prepareRequest(req pluginapi.ExecutorRequest, sender httpSender) (requestPlan, error) {
	var plan requestPlan
	c, err := readCredential(req.StorageJSON)
	if err != nil {
		return plan, pluginabi.NewError("invalid_auth", "invalid Trae credential", 401)
	}
	spec, ok := siteFor(c.Site)
	if !ok {
		return plan, pluginabi.NewError("invalid_auth", "invalid Trae credential", 401)
	}
	model := trimModelPrefix(req.Model)
	if model == "" {
		return plan, pluginabi.NewError("invalid_request", "model is required", 400)
	}
	chat, err := parseChatRequest(req.Payload)
	if err != nil {
		return plan, pluginabi.NewError("invalid_request", "invalid chat completion request", 400)
	}
	m, ok := findModel(sender, c, spec, model)
	if !ok {
		return plan, pluginabi.NewError("unknown_model", unknownModelMessage(sender, c, spec, model), 400)
	}
	session := sessionID(chat, req.Headers, c.UID)
	functions := []string{m.Function}
	for _, fn := range chatFunctions {
		if fn != m.Function {
			functions = append(functions, fn)
		}
	}
	return requestPlan{
		Chat:       chat,
		Model:      m,
		ModelName:  model,
		Functions:  functions,
		Session:    session,
		Sees:       m.Images,
		Credential: c,
		Spec:       spec,
	}, nil
}

func (p requestPlan) httpFor(fn string) (pluginapi.HTTPRequest, error) {
	body, err := chatBody(p.Chat, p.Model, fn, p.Session, p.Sees)
	if err != nil {
		return pluginapi.HTTPRequest{}, pluginabi.NewError("invalid_request", err.Error(), 400)
	}
	return pluginapi.HTTPRequest{
		Method:  "POST",
		URL:     p.Credential.modelHost(p.Spec) + chatPath,
		Headers: ideHeaders(p.Credential, p.Spec, map[string]string{"Accept": "text/event-stream"}),
		Body:    body,
	}, nil
}

// execute answers one non-streaming chat completion. Trae always streams, so
// the buffered body is read as SSE either way.
func execute(req pluginapi.ExecutorRequest, sender httpSender) (pluginapi.ExecutorResponse, error) {
	plan, err := prepareRequest(req, sender)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	release := acquire(req.AuthID)
	defer release()
	var last error
	for _, fn := range plan.Functions {
		last = nil
		httpReq, err := plan.httpFor(fn)
		if err != nil {
			return pluginapi.ExecutorResponse{}, err
		}
		resp, err := sender.Do(httpReq)
		if err != nil {
			return pluginapi.ExecutorResponse{}, pluginabi.NewError("transport_error", plan.Spec.Realm+" upstream transport failed", 503)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			f := classifyUpstream(plan.Spec, resp.StatusCode, resp.Headers, resp.Body)
			last = decisionError(f)
			if f.WrongFunction {
				continue
			}
			return pluginapi.ExecutorResponse{}, last
		}
		if !bytes.Contains(resp.Body, []byte("data:")) {
			// A 200 that is not an SSE stream is Trae refusing in JSON.
			f := classifyUpstream(plan.Spec, http.StatusBadGateway, resp.Headers, resp.Body)
			last = decisionError(f)
			if f.WrongFunction {
				continue
			}
			return pluginapi.ExecutorResponse{}, last
		}
		events := newTraeEvents(plan.Spec, plan.Chat.Tools)
		var out []traeEvent
		out = append(out, events.feed(resp.Body)...)
		out = append(out, events.flush()...)
		for _, e := range out {
			if e.Err != nil {
				last = decisionError(e.Err)
				if e.Err.WrongFunction {
					break
				}
				return pluginapi.ExecutorResponse{}, last
			}
		}
		if last != nil {
			continue
		}
		payload, failure := completionFromEvents(plan.ModelName, out)
		if failure != nil {
			return pluginapi.ExecutorResponse{}, decisionError(failure)
		}
		return pluginapi.ExecutorResponse{
			Payload: payload,
			Headers: http.Header{"Content-Type": {"application/json"}},
			Metadata: map[string]any{
				"upstream_host":  plan.Spec.ID,
				"upstream_model": plan.Model.ID,
			},
		}, nil
	}
	if last == nil {
		last = pluginabi.NewError("upstream_error", plan.Spec.Realm+" did not answer", 502)
	}
	return pluginapi.ExecutorResponse{}, last
}

type hostStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}

type hostReadResponse struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

func readHostStream(id string) (hostReadResponse, error) {
	var r hostReadResponse
	if err := hostRPC("host.http.stream_read", map[string]any{"stream_id": id}, &r); err != nil {
		return r, errors.New("upstream stream read failed")
	}
	if r.Error != "" {
		return r, errors.New("upstream stream failed")
	}
	return r, nil
}

func closeHostStream(id string) {
	_ = hostRPC("host.http.stream_close", map[string]any{"stream_id": id}, nil)
}

func emitStreamChunk(id string, payload []byte) error {
	return hostRPC("host.stream.emit", map[string]any{"stream_id": id, "payload": payload}, nil)
}

func closeStream(id, message string) {
	_ = hostRPC("host.stream.close", map[string]any{"stream_id": id, "error": message}, nil)
}

// closeStreamWithError ends a stream with the structured error the host's router
// reads: without error_detail the host sees a statusless plugin failure and
// answers every mid-stream refusal as a 500, so a 403 or a 429 never reaches the
// routing and cooldown decisions.
func closeStreamWithError(id, message string, err error) {
	var detail *pluginabi.Error
	_ = errors.As(err, &detail)
	if detail == nil {
		closeStream(id, message)
		return
	}
	_ = hostRPC("host.stream.close", map[string]any{"stream_id": id, "error": message, "error_detail": detail}, nil)
}

// readWholeStream drains and closes a host HTTP stream, with a bound so a
// misbehaving upstream cannot exhaust memory.
func readWholeStream(id string) ([]byte, error) {
	defer closeHostStream(id)
	var out []byte
	for {
		r, err := readHostStream(id)
		if err != nil {
			return nil, err
		}
		out = append(out, r.Payload...)
		if len(out) > 1<<20 {
			return nil, errors.New("upstream error body too large")
		}
		if r.Done {
			return out, nil
		}
	}
}

// Streaming seams. Tests replace them to exercise the pump without a plugin host.
var (
	readStreamChunk = readHostStream
	emitStreamFrame = emitStreamChunk
	openHTTPStream  = openUpstreamStream
)

func openUpstreamStream(plan requestPlan, fn, callbackID string) (hostStreamResponse, error) {
	var response hostStreamResponse
	httpReq, err := plan.httpFor(fn)
	if err != nil {
		return response, err
	}
	err = hostRPC("host.http.do_stream", map[string]any{"host_callback_id": callbackID, "request": httpReq}, &response)
	return response, err
}

// streamPump copies one Trae SSE stream into the host stream as OpenAI chunks.
type streamPump struct {
	source  string
	target  string
	events  *traeEvents
	writer  *streamAnswer
	emitted bool
}

func (p *streamPump) deliver(events []traeEvent) bool {
	for _, e := range events {
		if e.Err != nil {
			closeStreamWithError(p.target, e.Err.Message, decisionError(e.Err))
			return false
		}
		for _, frame := range p.writer.frames(e) {
			if emitStreamFrame(p.target, frame) != nil {
				closeStream(p.target, "client stream closed")
				return false
			}
			p.emitted = true
		}
	}
	return true
}

func (p *streamPump) run() {
	for {
		r, err := readStreamChunk(p.source)
		if err != nil {
			closeStream(p.target, "Trae stream failed")
			return
		}
		if !p.deliver(p.events.feed(r.Payload)) {
			return
		}
		if r.Done {
			if !p.deliver(p.events.flush()) {
				return
			}
			if !p.writer.saidSomething() {
				closeStream(p.target, "Trae ended without an answer")
				return
			}
			if frame := p.writer.done(); len(frame) > 0 {
				_ = emitStreamFrame(p.target, frame)
			}
			closeStream(p.target, "")
			return
		}
	}
}

// executeStream opens the upstream stream, rejects a refusal before any chunk
// reaches the client, and then pumps asynchronously.
func executeStream(req pluginapi.ExecutorRequest, callbackID, streamID string, sender httpSender) (map[string]any, error) {
	if streamID == "" {
		return nil, pluginabi.NewError("invalid_request", "stream ID required", 400)
	}
	plan, err := prepareRequest(req, sender)
	if err != nil {
		return nil, err
	}
	release := acquire(req.AuthID)
	var last error
	for _, fn := range plan.Functions {
		response, err := openHTTPStream(plan, fn, callbackID)
		if err != nil {
			release()
			return nil, pluginabi.NewError("transport_error", plan.Spec.Realm+" upstream stream failed", 503)
		}
		if response.StreamID == "" {
			release()
			return nil, pluginabi.NewError("transport_error", plan.Spec.Realm+" upstream stream unavailable", 503)
		}
		contentType := strings.ToLower(response.Headers.Get("Content-Type"))
		if response.StatusCode < 200 || response.StatusCode >= 300 || strings.Contains(contentType, "application/json") {
			raw, readErr := readWholeStream(response.StreamID)
			if readErr != nil {
				release()
				return nil, pluginabi.NewError("transport_error", plan.Spec.Realm+" upstream stream failed", 503)
			}
			f := classifyUpstream(plan.Spec, response.StatusCode, response.Headers, raw)
			last = decisionError(f)
			if f.WrongFunction {
				continue
			}
			release()
			return nil, last
		}
		events := newTraeEvents(plan.Spec, plan.Chat.Tools)
		writer := newStreamAnswer(plan.ModelName)
		// Read until the answer starts so a refusal carried inside an OK frame
		// (a lapsed token, a model this account lacks) keeps its real status
		// instead of arriving as a 200 stream that ends in an error.
		var pending []traeEvent
		retry := false
		for attempt := 0; attempt < 16 && len(pending) == 0; attempt++ {
			r, err := readStreamChunk(response.StreamID)
			if err != nil {
				closeHostStream(response.StreamID)
				release()
				return nil, pluginabi.NewError("transport_error", plan.Spec.Realm+" upstream stream failed", 503)
			}
			pending = append(pending, events.feed(r.Payload)...)
			if r.Done {
				pending = append(pending, events.flush()...)
				break
			}
		}
		for _, e := range pending {
			if e.Err == nil {
				continue
			}
			if e.Err.WrongFunction {
				retry = true
				last = decisionError(e.Err)
				break
			}
			closeHostStream(response.StreamID)
			release()
			return nil, decisionError(e.Err)
		}
		if retry {
			closeHostStream(response.StreamID)
			continue
		}
		pump := &streamPump{source: response.StreamID, target: streamID, events: events, writer: writer}
		go func() {
			defer release()
			defer closeHostStream(pump.source)
			if !pump.deliver(pending) {
				return
			}
			pump.run()
		}()
		return map[string]any{"headers": response.Headers}, nil
	}
	release()
	if last == nil {
		last = pluginabi.NewError("upstream_error", plan.Spec.Realm+" did not answer", 502)
	}
	return nil, last
}

// countTokens is not supported: Trae documents no token-count endpoint, and a
// guess would misbill clients that trust it.
func countTokens() (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, pluginabi.NewError("not_supported", "Trae does not provide a verified token-count endpoint", http.StatusNotImplemented)
}
