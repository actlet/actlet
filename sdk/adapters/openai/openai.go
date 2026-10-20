// Package openai answers Actlet's model calls with OpenAI's Chat
// Completions API, streaming: an adapter for sdk.Options.OnInference.
// With a BaseURL it serves any endpoint compatible with Chat Completions,
// such as GPT6 Luna's or nexos.ai's.
//
//	c := openai.New(openai.Config{APIKey: os.Getenv("OPENAI_API_KEY")})
//	opts.OnInference = c.OnInference("gpt-5", adapters.Params{MaxTokens: 8192})
//
// The conversation maps to chat messages: an assistant message with
// tool_calls, and one "tool" message per tool result. Chat Completions
// keeps no state between calls, so the continuation stays empty.
package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
	"github.com/actlet/actlet/sdk/adapters"
)

// DefaultBaseURL is OpenAI's API.
const DefaultBaseURL = "https://api.openai.com/v1"

// Config configures a client.
type Config struct {
	// BaseURL defaults to DefaultBaseURL; requests go to
	// BaseURL + "/chat/completions".
	BaseURL string
	APIKey  string
	// Name names the provider in errors; default "openai".
	Name string
	// MaxTokensField is the request field that bounds an answer:
	// "max_completion_tokens" (the default, which OpenAI's newer models
	// need) or "max_tokens", which some compatible endpoints want.
	MaxTokensField string
	// HTTPClient defaults to http.DefaultClient. Give it no overall
	// timeout: an answer streams for as long as the model writes.
	HTTPClient *http.Client
	// Prices make calls report their cost.
	Prices adapters.Prices
}

// Client calls Chat Completions.
type Client struct {
	cfg Config
}

// New returns a client.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	if cfg.Name == "" {
		cfg.Name = "openai"
	}
	if cfg.MaxTokensField == "" {
		cfg.MaxTokensField = "max_completion_tokens"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	return &Client{cfg: cfg}
}

// OnInference returns an sdk.Options.OnInference that answers every call
// with the model.
func (c *Client) OnInference(model string, p adapters.Params) func(context.Context, *sdk.InferenceCall) error {
	return func(ctx context.Context, call *sdk.InferenceCall) error { return c.Infer(ctx, call, model, p) }
}

// Infer implements adapters.Model.
func (c *Client) Infer(ctx context.Context, call *sdk.InferenceCall, model string, p adapters.Params) error {
	req := Request(call.Request, model, p, c.cfg.MaxTokensField)
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	header.Set("Accept", "text/event-stream")
	resp, err := adapters.PostJSON(ctx, c.cfg.HTTPClient, c.cfg.Name, c.cfg.BaseURL+"/chat/completions", header, req, c.cfg.APIKey)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	s := &stream{call: call, name: c.cfg.Name, calls: map[int]*toolCall{}}
	if err := adapters.ReadEvents(resp.Body, s.event); err != nil {
		if st, ok := status.FromError(err); ok {
			return st.Err()
		}
		return adapters.TransportStatus(ctx, c.cfg.Name, err, c.cfg.APIKey)
	}
	if !s.done {
		return status.Errorf(codes.Unavailable, "%s: the answer ended early", c.cfg.Name)
	}
	if err := s.flushToolCalls(); err != nil {
		return err
	}
	if s.usage == nil {
		s.usage = &gatewayv0.Usage{}
	}
	c.cfg.Prices.Cost(model, s.usage)
	return call.Done(&gatewayv0.InferenceDone{StopReason: s.stop, Usage: s.usage})
}

// ChatRequest is a Chat Completions request, as far as Actlet uses it.
type ChatRequest struct {
	Model               string         `json:"model"`
	Messages            []ChatMessage  `json:"messages"`
	Tools               []ChatTool     `json:"tools,omitempty"`
	Stream              bool           `json:"stream"`
	StreamOptions       *StreamOptions `json:"stream_options,omitempty"`
	MaxCompletionTokens uint32         `json:"max_completion_tokens,omitempty"`
	MaxTokens           uint32         `json:"max_tokens,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
}

// StreamOptions asks for usage at the end of a stream.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatMessage is a message of the conversation.
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []ChatCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ChatCall is a tool call in an assistant message.
type ChatCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ChatFunction `json:"function"`
}

// ChatFunction is a function call: its name and JSON arguments.
type ChatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatTool describes a tool.
type ChatTool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef describes a function.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

func text(s string) *string { return &s }

// Request maps a call to a Chat Completions request. maxTokensField is
// "max_completion_tokens" or "max_tokens".
func Request(r *gatewayv0.InferenceRequest, model string, p adapters.Params, maxTokensField string) *ChatRequest {
	req := &ChatRequest{Model: model, Stream: true, StreamOptions: &StreamOptions{IncludeUsage: true}, Temperature: p.Temperature}
	limit := r.GetMaxOutputTokens()
	if limit == 0 {
		limit = p.MaxTokens
	}
	if maxTokensField == "max_tokens" {
		req.MaxTokens = limit
	} else {
		req.MaxCompletionTokens = limit
	}
	if r.GetSystem() != "" {
		req.Messages = append(req.Messages, ChatMessage{Role: "system", Content: text(r.GetSystem())})
	}
	for _, m := range r.GetMessages() {
		var texts []string
		var calls []ChatCall
		var results []ChatMessage
		for _, part := range m.GetParts() {
			switch {
			case part.GetToolCall() != nil:
				tc := part.GetToolCall()
				args := tc.GetInput()
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				calls = append(calls, ChatCall{ID: tc.GetId(), Type: "function", Function: ChatFunction{Name: tc.GetName(), Arguments: args}})
			case part.GetToolResult() != nil:
				tr := part.GetToolResult()
				content := tr.GetContent()
				if tr.GetIsError() {
					// [unverified] Chat Completions has no error flag on
					// tool messages; the content says so instead.
					content = "Error: " + content
				}
				results = append(results, ChatMessage{Role: "tool", ToolCallID: tr.GetCallId(), Content: text(content)})
			case part.GetText() != "":
				texts = append(texts, part.GetText())
			}
		}
		if m.GetRole() == gatewayv0.Role_ROLE_ASSISTANT {
			msg := ChatMessage{Role: "assistant", ToolCalls: calls}
			if len(texts) > 0 {
				msg.Content = text(strings.Join(texts, "\n"))
			}
			req.Messages = append(req.Messages, msg)
			continue
		}
		// Tool results answer the assistant's calls directly; text
		// follows them as a user message.
		req.Messages = append(req.Messages, results...)
		if len(texts) > 0 {
			req.Messages = append(req.Messages, ChatMessage{Role: "user", Content: text(strings.Join(texts, "\n"))})
		}
	}
	for _, t := range r.GetTools() {
		schema := json.RawMessage(t.GetInputSchema())
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		req.Tools = append(req.Tools, ChatTool{Type: "function", Function: FunctionDef{Name: t.GetName(), Description: t.GetDescription(), Parameters: schema}})
	}
	return req
}

// StopReason maps a finish reason.
func StopReason(s string) gatewayv0.StopReason {
	switch s {
	case "tool_calls", "function_call":
		return gatewayv0.StopReason_STOP_REASON_TOOL_USE
	case "length":
		return gatewayv0.StopReason_STOP_REASON_MAX_TOKENS
	case "content_filter":
		return gatewayv0.StopReason_STOP_REASON_REFUSAL
	}
	return gatewayv0.StopReason_STOP_REASON_END_TURN
}

// stream turns the streamed chunks into the call's answer.
type stream struct {
	call    *sdk.InferenceCall
	name    string
	calls   map[int]*toolCall
	refusal strings.Builder
	usage   *gatewayv0.Usage
	stop    gatewayv0.StopReason
	done    bool
}

type toolCall struct {
	id, name string
	args     strings.Builder
}

// Chunk is a streamed chunk, as far as Actlet reads it.
type Chunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content   string `json:"content"`
			Refusal   string `json:"refusal"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        uint64 `json:"prompt_tokens"`
		CompletionTokens    uint64 `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens uint64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func (s *stream) event(e adapters.Event) error {
	if e.Data == "[DONE]" {
		s.done = true
		return nil
	}
	var ch Chunk
	if err := json.Unmarshal([]byte(e.Data), &ch); err != nil {
		return status.Errorf(codes.Unavailable, "%s: a chunk that isn't JSON: %v", s.name, err)
	}
	if ch.Error != nil {
		// [unverified] An error inside a stream, as compatible servers
		// send them; retried as a server error.
		return status.Errorf(codes.Unavailable, "%s: %s: %s", s.name, ch.Error.Type, ch.Error.Message)
	}
	for _, c := range ch.Choices {
		if c.Index != 0 {
			continue // only the first choice; Actlet asks for one
		}
		if c.Delta.Content != "" {
			if err := s.call.Text(c.Delta.Content); err != nil {
				return err
			}
		}
		s.refusal.WriteString(c.Delta.Refusal)
		for _, d := range c.Delta.ToolCalls {
			tc := s.calls[d.Index]
			if tc == nil {
				tc = &toolCall{}
				s.calls[d.Index] = tc
			}
			if d.ID != "" {
				tc.id = d.ID
			}
			tc.name += d.Function.Name
			tc.args.WriteString(d.Function.Arguments)
		}
		if c.FinishReason != nil {
			s.stop = StopReason(*c.FinishReason)
		}
	}
	if u := ch.Usage; u != nil {
		cached := min(u.PromptTokensDetails.CachedTokens, u.PromptTokens)
		s.usage = &gatewayv0.Usage{InputTokens: u.PromptTokens - cached, OutputTokens: u.CompletionTokens, CacheReadTokens: cached}
	}
	return nil
}

// flushToolCalls sends the tool calls, whole, in the order the model
// made them, and a refusal as the answer's text.
func (s *stream) flushToolCalls() error {
	if r := s.refusal.String(); r != "" {
		if err := s.call.Text(r); err != nil {
			return err
		}
		s.stop = gatewayv0.StopReason_STOP_REASON_REFUSAL
	}
	idx := make([]int, 0, len(s.calls))
	for i := range s.calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		tc := s.calls[i]
		args := strings.TrimSpace(tc.args.String())
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return status.Errorf(codes.Unavailable, "%s: tool call %s has arguments that aren't JSON", s.name, tc.id)
		}
		if err := s.call.ToolCall(tc.id, tc.name, args); err != nil {
			return err
		}
	}
	switch {
	case len(idx) > 0 && (s.stop == gatewayv0.StopReason_STOP_REASON_END_TURN || s.stop == gatewayv0.StopReason_STOP_REASON_UNSPECIFIED):
		s.stop = gatewayv0.StopReason_STOP_REASON_TOOL_USE
	case s.stop == gatewayv0.StopReason_STOP_REASON_UNSPECIFIED:
		s.stop = gatewayv0.StopReason_STOP_REASON_END_TURN
	}
	return nil
}
