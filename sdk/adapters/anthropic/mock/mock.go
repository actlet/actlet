// Package mock is a mock of Anthropic's Messages API, for testing
// adapters and orchestrators without calling Anthropic: an HTTP server
// that reads requests as the API documents them, answers through a
// Responder, and streams the answer as the API's server-sent events.
package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk/adapters/anthropic"
)

// Answer is what the mock model says.
type Answer struct {
	// Thinking, if set, comes first, as a thinking block with Signature.
	Thinking, Signature string
	Text                string
	ToolCalls           []*gatewayv0.ToolCall
	// Stop is the stop reason; empty means end_turn, or tool_use with
	// tool calls.
	Stop  string
	Usage Usage
}

// Usage is what the mock reports.
type Usage struct {
	Input, Output, CacheRead, CacheWrite uint64
}

// Responder answers a request, read back into Actlet's neutral form.
type Responder func(ctx context.Context, req *gatewayv0.InferenceRequest) (*Answer, error)

// Server is a mock Messages API.
type Server struct {
	*httptest.Server
	// APIKey is the key requests must send.
	APIKey string

	respond Responder
	mu      sync.Mutex
	bodies  []anthropic.MessagesRequest
	fail    []failure
}

type failure struct {
	status    int
	body      string
	midStream string // an error event's type, sent after the first text
}

// New starts a mock that answers with respond and wants key. An error
// from respond is answered 400, which the worker doesn't retry; FailNext
// gives errors it does.
func New(key string, respond Responder) *Server {
	s := &Server{APIKey: key, respond: respond}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Requests returns the requests received, as sent.
func (s *Server) Requests() []anthropic.MessagesRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]anthropic.MessagesRequest(nil), s.bodies...)
}

// FailNext makes the next request fail with an HTTP status and an error
// of the type, as the API sends them.
func (s *Server) FailNext(status int, errType, message string) {
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": errType, "message": message}})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = append(s.fail, failure{status: status, body: string(body)})
}

// FailMidStream makes the next request's stream end with an error event
// of the type, such as overloaded_error, after its first text.
func (s *Server) FailMidStream(errType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = append(s.fail, failure{midStream: errType})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("x-api-key") != s.APIKey {
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid x-api-key")
		return
	}
	if r.Header.Get("anthropic-version") == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "anthropic-version: header is required")
		return
	}
	var req anthropic.MessagesRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := validate(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	s.mu.Lock()
	s.bodies = append(s.bodies, req)
	var f *failure
	if len(s.fail) > 0 {
		f = &s.fail[0]
		s.fail = s.fail[1:]
	}
	s.mu.Unlock()
	if f != nil && f.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
		return
	}
	a, err := s.respond(r.Context(), neutral(&req))
	if err != nil {
		// A request respond refuses, such as one a script didn't expect,
		// fails the call: as a server error, the worker would retry it
		// for ever.
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	midStream := ""
	if f != nil {
		midStream = f.midStream
	}
	stream(w, a, midStream)
}

// validate checks what the API checks that adapters could get wrong.
func validate(req *anthropic.MessagesRequest) error {
	switch {
	case req.Model == "":
		return fmt.Errorf("model: Field required")
	case req.MaxTokens == 0:
		return fmt.Errorf("max_tokens: Field required")
	case !req.Stream:
		return fmt.Errorf("this mock streams only")
	case req.Thinking != nil && req.MaxTokens <= req.Thinking.BudgetTokens:
		return fmt.Errorf("max_tokens must be greater than thinking.budget_tokens")
	}
	for i, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return fmt.Errorf("messages.%d.role: Input should be 'user' or 'assistant'", i)
		}
		if len(m.Content) == 0 {
			return fmt.Errorf("messages.%d: all messages must have non-empty content", i)
		}
		seenText := false
		for j, b := range m.Content {
			switch b.Type {
			case "tool_result":
				if seenText {
					return fmt.Errorf("messages.%d.content.%d: tool_result blocks must come first", i, j)
				}
			case "text":
				seenText = true
				if b.Text == "" {
					return fmt.Errorf("messages.%d.content.%d: text content blocks must be non-empty", i, j)
				}
			case "tool_use":
				if !json.Valid(b.Input) || !strings.HasPrefix(strings.TrimSpace(string(b.Input)), "{") {
					return fmt.Errorf("messages.%d.content.%d.input: Input should be a valid dictionary", i, j)
				}
			}
		}
	}
	return nil
}

// neutral reads a request back into Actlet's form.
func neutral(req *anthropic.MessagesRequest) *gatewayv0.InferenceRequest {
	out := &gatewayv0.InferenceRequest{Model: req.Model, System: req.System, MaxOutputTokens: req.MaxTokens}
	for _, m := range req.Messages {
		msg := &gatewayv0.Message{Role: gatewayv0.Role_ROLE_USER}
		if m.Role == "assistant" {
			msg.Role = gatewayv0.Role_ROLE_ASSISTANT
		}
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				msg.Parts = append(msg.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_Text{Text: b.Text}})
			case "tool_use":
				msg.Parts = append(msg.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_ToolCall{ToolCall: &gatewayv0.ToolCall{Id: b.ID, Name: b.Name, Input: string(b.Input)}}})
			case "tool_result":
				msg.Parts = append(msg.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_ToolResult{ToolResult: &gatewayv0.ToolResult{CallId: b.ToolUseID, Content: b.Content, IsError: b.IsError}}})
			}
		}
		out.Messages = append(out.Messages, msg)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, &gatewayv0.ToolSpec{Name: t.Name, Description: t.Description, InputSchema: string(t.InputSchema)})
	}
	return out
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": errType, "message": message}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// stream writes an answer as the API's events: the text in a few deltas,
// each tool call's input in two pieces.
func stream(w http.ResponseWriter, a *Answer, failMidStream string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_mock", "type": "message", "role": "assistant", "content": []any{}, "model": "mock",
		"stop_reason": nil, "usage": map[string]any{"input_tokens": a.Usage.Input, "output_tokens": 1,
			"cache_creation_input_tokens": a.Usage.CacheWrite, "cache_read_input_tokens": a.Usage.CacheRead},
	}})
	_, _ = fmt.Fprint(w, "event: ping\ndata: {\"type\": \"ping\"}\n\n")
	i := 0
	if a.Thinking != "" {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "thinking_delta", "thinking": a.Thinking}})
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "signature_delta", "signature": a.Signature}})
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
		i++
	}
	if a.Text != "" {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
		words := strings.SplitAfter(a.Text, " ")
		for n, word := range words {
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": word}})
			if n == 0 && failMidStream != "" {
				send("error", map[string]any{"type": "error", "error": map[string]string{"type": failMidStream, "message": "Overloaded"}})
				return
			}
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
		i++
	}
	for _, tc := range a.ToolCalls {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{
			"type": "tool_use", "id": tc.GetId(), "name": tc.GetName(), "input": map[string]any{}}})
		in := tc.GetInput()
		half := len(in) / 2
		for _, piece := range []string{in[:half], in[half:]} {
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": piece}})
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
		i++
	}
	stop := a.Stop
	if stop == "" {
		stop = "end_turn"
		if len(a.ToolCalls) > 0 {
			stop = "tool_use"
		}
	}
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": a.Usage.Output}})
	send("message_stop", map[string]any{"type": "message_stop"})
}
