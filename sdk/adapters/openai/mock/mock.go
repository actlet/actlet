// Package mock is a mock of OpenAI's Chat Completions API, for testing
// adapters and orchestrators without calling OpenAI or a compatible
// endpoint: an HTTP server that reads requests as the API documents
// them, answers through a Responder, and streams the answer as the
// API's chunks.
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
	"github.com/actlet/actlet/sdk/adapters/openai"
)

// Answer is what the mock model says.
type Answer struct {
	Text string
	// Refusal, if set, is sent instead of text.
	Refusal   string
	ToolCalls []*gatewayv0.ToolCall
	// Finish is the finish reason; empty means stop, or tool_calls with
	// tool calls.
	Finish string
	Usage  Usage
}

// Usage is what the mock reports.
type Usage struct {
	Prompt, Completion, Cached uint64
}

// Responder answers a request, read back into Actlet's neutral form.
type Responder func(ctx context.Context, req *gatewayv0.InferenceRequest) (*Answer, error)

// Server is a mock Chat Completions API, at URL + "/v1".
type Server struct {
	*httptest.Server
	APIKey string

	respond Responder
	mu      sync.Mutex
	bodies  []openai.ChatRequest
	fail    []failure
}

type failure struct {
	status    int
	body      string
	midStream bool
}

// New starts a mock that answers with respond and wants key.
func New(key string, respond Responder) *Server {
	s := &Server{APIKey: key, respond: respond}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// BaseURL is the base URL to configure the adapter with.
func (s *Server) BaseURL() string { return s.URL + "/v1" }

// Requests returns the requests received, as sent.
func (s *Server) Requests() []openai.ChatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]openai.ChatRequest(nil), s.bodies...)
}

// FailNext makes the next request fail with an HTTP status and an error
// as the API sends them.
func (s *Server) FailNext(status int, errType, code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = append(s.fail, failure{status: status, body: errorBody(errType, code, message)})
}

// FailMidStream makes the next request's stream end with an error chunk
// after its first text.
func (s *Server) FailMidStream() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = append(s.fail, failure{midStream: true})
}

func errorBody(errType, code, message string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": errType, "param": nil, "code": code}})
	return string(b)
}

func writeError(w http.ResponseWriter, status int, errType, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(errorBody(errType, code, message)))
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.APIKey {
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "Incorrect API key provided.")
		return
	}
	var req openai.ChatRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "", err.Error())
		return
	}
	if err := validate(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "", err.Error())
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
		writeError(w, http.StatusInternalServerError, "server_error", "", err.Error())
		return
	}
	stream(w, a, f != nil && f.midStream)
}

// validate checks what the API checks that adapters could get wrong.
func validate(req *openai.ChatRequest) error {
	switch {
	case req.Model == "":
		return fmt.Errorf("you must provide a model parameter")
	case !req.Stream || req.StreamOptions == nil || !req.StreamOptions.IncludeUsage:
		return fmt.Errorf("this mock streams only, with usage")
	}
	pending := map[string]bool{} // tool calls not answered yet
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "user":
			if len(pending) > 0 {
				return fmt.Errorf("messages.[%d]: an assistant message with 'tool_calls' must be followed by tool messages responding to each 'tool_call_id'", i)
			}
			if m.Content == nil {
				return fmt.Errorf("messages.[%d].content: required", i)
			}
		case "assistant":
			if len(pending) > 0 {
				return fmt.Errorf("messages.[%d]: tool calls not answered", i)
			}
			if m.Content == nil && len(m.ToolCalls) == 0 {
				return fmt.Errorf("messages.[%d]: content or tool_calls is required", i)
			}
			for _, c := range m.ToolCalls {
				if c.Type != "function" || !json.Valid([]byte(c.Function.Arguments)) {
					return fmt.Errorf("messages.[%d].tool_calls: invalid", i)
				}
				pending[c.ID] = true
			}
		case "tool":
			if !pending[m.ToolCallID] {
				return fmt.Errorf("messages.[%d]: messages with role 'tool' must be a response to a preceding message with 'tool_calls'", i)
			}
			delete(pending, m.ToolCallID)
		default:
			return fmt.Errorf("messages.[%d].role: invalid", i)
		}
	}
	return nil
}

// neutral reads a request back into Actlet's form.
func neutral(req *openai.ChatRequest) *gatewayv0.InferenceRequest {
	out := &gatewayv0.InferenceRequest{Model: req.Model, MaxOutputTokens: max(req.MaxCompletionTokens, req.MaxTokens)}
	var results *gatewayv0.Message
	add := func(m *gatewayv0.Message) {
		if results != nil {
			out.Messages = append(out.Messages, results)
			results = nil
		}
		if m != nil {
			out.Messages = append(out.Messages, m)
		}
	}
	for _, m := range req.Messages {
		content := ""
		if m.Content != nil {
			content = *m.Content
		}
		switch m.Role {
		case "system":
			out.System = content
		case "user":
			if results != nil {
				results.Parts = append(results.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_Text{Text: content}})
				add(nil)
				continue
			}
			add(&gatewayv0.Message{Role: gatewayv0.Role_ROLE_USER, Parts: []*gatewayv0.Part{{Part: &gatewayv0.Part_Text{Text: content}}}})
		case "assistant":
			msg := &gatewayv0.Message{Role: gatewayv0.Role_ROLE_ASSISTANT}
			if content != "" {
				msg.Parts = append(msg.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_Text{Text: content}})
			}
			for _, c := range m.ToolCalls {
				msg.Parts = append(msg.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_ToolCall{ToolCall: &gatewayv0.ToolCall{Id: c.ID, Name: c.Function.Name, Input: c.Function.Arguments}}})
			}
			add(msg)
		case "tool":
			if results == nil {
				results = &gatewayv0.Message{Role: gatewayv0.Role_ROLE_USER}
			}
			isError := strings.HasPrefix(content, "Error: ")
			results.Parts = append(results.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_ToolResult{ToolResult: &gatewayv0.ToolResult{
				CallId: m.ToolCallID, Content: strings.TrimPrefix(content, "Error: "), IsError: isError}}})
		}
	}
	add(nil)
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, &gatewayv0.ToolSpec{Name: t.Function.Name, Description: t.Function.Description, InputSchema: string(t.Function.Parameters)})
	}
	return out
}

// stream writes an answer as the API's chunks: the text in a few deltas,
// each tool call's arguments in two pieces, then the finish reason,
// usage and [DONE].
func stream(w http.ResponseWriter, a *Answer, failMidStream bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	send := func(data any) {
		b, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1700000000, "model": "mock",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "logprobs": nil, "finish_reason": finish}}, "usage": nil}
	}
	send(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	if a.Refusal != "" {
		send(chunk(map[string]any{"refusal": a.Refusal}, nil))
	}
	for n, word := range strings.SplitAfter(a.Text, " ") {
		if word == "" {
			continue
		}
		send(chunk(map[string]any{"content": word}, nil))
		if n == 0 && failMidStream {
			send(map[string]any{"error": map[string]any{"message": "The server had an error while processing your request.", "type": "server_error"}})
			return
		}
	}
	for i, tc := range a.ToolCalls {
		in := tc.GetInput()
		half := len(in) / 2
		send(chunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": tc.GetId(), "type": "function",
			"function": map[string]any{"name": tc.GetName(), "arguments": in[:half]}}}}, nil))
		send(chunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"arguments": in[half:]}}}}, nil))
	}
	finish := a.Finish
	if finish == "" {
		finish = "stop"
		if len(a.ToolCalls) > 0 {
			finish = "tool_calls"
		}
	}
	send(chunk(map[string]any{}, finish))
	send(map[string]any{"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1700000000, "model": "mock", "choices": []any{},
		"usage": map[string]any{"prompt_tokens": a.Usage.Prompt + a.Usage.Cached, "completion_tokens": a.Usage.Completion,
			"total_tokens": a.Usage.Prompt + a.Usage.Cached + a.Usage.Completion, "prompt_tokens_details": map[string]any{"cached_tokens": a.Usage.Cached}}})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}
