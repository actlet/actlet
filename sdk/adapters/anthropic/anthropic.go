// Package anthropic answers Actlet's model calls with Anthropic's
// Messages API, streaming: an adapter for sdk.Options.OnInference.
//
//	c := anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY")})
//	opts.OnInference = c.OnInference("claude-sonnet-4-5", adapters.Params{MaxTokens: 8192})
//
// The conversation maps to Messages: text, tool_use and tool_result
// blocks. When a route asks the model to think, its thinking blocks are
// kept in the call's continuation and sent back with the next request,
// as the API requires during tool use; the worker never looks inside.
package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
	"github.com/actlet/actlet/sdk/adapters"
)

// Defaults.
const (
	DefaultBaseURL   = "https://api.anthropic.com"
	APIVersion       = "2023-06-01"
	DefaultMaxTokens = 4096
)

// Config configures a client.
type Config struct {
	// BaseURL defaults to DefaultBaseURL.
	BaseURL string
	APIKey  string
	// HTTPClient defaults to http.DefaultClient. Give it no overall
	// timeout: an answer streams for as long as the model writes.
	HTTPClient *http.Client
	// Prices make calls report their cost.
	Prices adapters.Prices
}

// Client calls the Messages API.
type Client struct {
	cfg Config
}

// New returns a client.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
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

const provider = "anthropic"

// Infer implements adapters.Model.
func (c *Client) Infer(ctx context.Context, call *sdk.InferenceCall, model string, p adapters.Params) error {
	req, err := Request(call.Request, model, p)
	if err != nil {
		return err
	}
	header := http.Header{}
	header.Set("x-api-key", c.cfg.APIKey)
	header.Set("anthropic-version", APIVersion)
	header.Set("Accept", "text/event-stream")
	resp, err := adapters.PostJSON(ctx, c.cfg.HTTPClient, provider, c.cfg.BaseURL+"/v1/messages", header, req, c.cfg.APIKey)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	s := &stream{call: call, blocks: map[int]*block{}}
	if err := adapters.ReadEvents(resp.Body, s.event); err != nil {
		if st, ok := status.FromError(err); ok {
			return st.Err()
		}
		return adapters.TransportStatus(ctx, provider, err, c.cfg.APIKey)
	}
	if !s.stopped {
		return status.Error(codes.Unavailable, "anthropic: the answer ended early")
	}
	c.cfg.Prices.Cost(model, s.usage)
	return call.Done(&gatewayv0.InferenceDone{StopReason: s.stop, Usage: s.usage, Continuation: s.continuation()})
}

// The Messages API's request, as far as Actlet uses it.

// MessagesRequest is a request to the Messages API.
type MessagesRequest struct {
	Model       string    `json:"model"`
	MaxTokens   uint32    `json:"max_tokens"`
	System      string    `json:"system,omitempty"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Stream      bool      `json:"stream"`
	Temperature *float64  `json:"temperature,omitempty"`
	Thinking    *Thinking `json:"thinking,omitempty"`
}

// Message is a message of the conversation.
type Message struct {
	Role    string  `json:"role"`
	Content []Block `json:"content"`
}

// Block is a content block.
type Block struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	// thinking and redacted_thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

// Tool describes a tool.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Thinking asks the model to think first.
type Thinking struct {
	Type         string `json:"type"`
	BudgetTokens uint32 `json:"budget_tokens"`
}

// continuationData is what a call's continuation holds: the thinking
// blocks of the answer, which go back at the start of that assistant
// message, the Nth of the conversation.
type continuationData struct {
	Assistant int     `json:"assistant"`
	Thinking  []Block `json:"thinking"`
}

// Request maps a call to a Messages request.
func Request(r *gatewayv0.InferenceRequest, model string, p adapters.Params) (*MessagesRequest, error) {
	req := &MessagesRequest{Model: model, MaxTokens: r.GetMaxOutputTokens(), System: r.GetSystem(), Stream: true, Temperature: p.Temperature}
	if req.MaxTokens == 0 {
		req.MaxTokens = p.MaxTokens
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = DefaultMaxTokens
	}
	if p.ThinkingBudget > 0 {
		req.Thinking = &Thinking{Type: "enabled", BudgetTokens: p.ThinkingBudget}
		req.MaxTokens = max(req.MaxTokens, p.ThinkingBudget+1024)
		req.Temperature = nil // thinking takes none
	}
	var cont continuationData
	if c := r.GetContinuation(); len(c) > 0 {
		if err := json.Unmarshal(c, &cont); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "anthropic: a continuation this adapter didn't make: %v", err)
		}
	}
	assistant := 0
	for _, m := range r.GetMessages() {
		msg := Message{Role: "user"}
		if m.GetRole() == gatewayv0.Role_ROLE_ASSISTANT {
			msg.Role = "assistant"
			assistant++
			if assistant == cont.Assistant {
				msg.Content = append(msg.Content, cont.Thinking...)
			}
		}
		var texts []Block
		for _, part := range m.GetParts() {
			switch {
			case part.GetToolCall() != nil:
				tc := part.GetToolCall()
				input := json.RawMessage(tc.GetInput())
				if len(strings.TrimSpace(tc.GetInput())) == 0 {
					input = json.RawMessage("{}")
				}
				msg.Content = append(msg.Content, Block{Type: "tool_use", ID: tc.GetId(), Name: tc.GetName(), Input: input})
			case part.GetToolResult() != nil:
				tr := part.GetToolResult()
				msg.Content = append(msg.Content, Block{Type: "tool_result", ToolUseID: tr.GetCallId(), Content: tr.GetContent(), IsError: tr.GetIsError()})
			case part.GetText() != "":
				if msg.Role == "assistant" {
					msg.Content = append(msg.Content, Block{Type: "text", Text: part.GetText()})
				} else {
					// Text follows the tool results in a user message, as
					// the API wants them first.
					texts = append(texts, Block{Type: "text", Text: part.GetText()})
				}
			}
		}
		msg.Content = append(msg.Content, texts...)
		if len(msg.Content) == 0 {
			continue
		}
		req.Messages = append(req.Messages, msg)
	}
	for _, t := range r.GetTools() {
		schema := json.RawMessage(t.GetInputSchema())
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		req.Tools = append(req.Tools, Tool{Name: t.GetName(), Description: t.GetDescription(), InputSchema: schema})
	}
	return req, nil
}

// StopReason maps the API's stop reason.
func StopReason(s string) gatewayv0.StopReason {
	switch s {
	case "tool_use":
		return gatewayv0.StopReason_STOP_REASON_TOOL_USE
	case "max_tokens":
		return gatewayv0.StopReason_STOP_REASON_MAX_TOKENS
	case "refusal":
		return gatewayv0.StopReason_STOP_REASON_REFUSAL
	}
	return gatewayv0.StopReason_STOP_REASON_END_TURN // end_turn, stop_sequence, pause_turn
}

// stream turns the streamed events into the call's answer.
type stream struct {
	call     *sdk.InferenceCall
	blocks   map[int]*block
	thinking []Block
	usage    *gatewayv0.Usage
	stop     gatewayv0.StopReason
	stopped  bool
}

// block is a content block being streamed.
type block struct {
	Block
	json strings.Builder
}

// streamEvent is any of the stream's events, as far as Actlet reads them.
type streamEvent struct {
	Type    string `json:"type"`
	Message struct {
		Usage usage `json:"usage"`
	} `json:"message"`
	Index        int   `json:"index"`
	ContentBlock Block `json:"content_block"`
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage usage `json:"usage"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type usage struct {
	InputTokens              uint64 `json:"input_tokens"`
	OutputTokens             uint64 `json:"output_tokens"`
	CacheCreationInputTokens uint64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     uint64 `json:"cache_read_input_tokens"`
}

func (s *stream) event(e adapters.Event) error {
	var ev streamEvent
	if err := json.Unmarshal([]byte(e.Data), &ev); err != nil {
		return status.Errorf(codes.Unavailable, "anthropic: an event that isn't JSON: %v", err)
	}
	switch ev.Type {
	case "message_start":
		u := ev.Message.Usage
		s.usage = &gatewayv0.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			CacheReadTokens: u.CacheReadInputTokens, CacheWriteTokens: u.CacheCreationInputTokens}
	case "content_block_start":
		s.blocks[ev.Index] = &block{Block: ev.ContentBlock}
	case "content_block_delta":
		b := s.blocks[ev.Index]
		if b == nil {
			return status.Errorf(codes.Unavailable, "anthropic: a delta for block %d, which didn't start", ev.Index)
		}
		switch ev.Delta.Type {
		case "text_delta":
			b.Text += ev.Delta.Text
			if err := s.call.Text(ev.Delta.Text); err != nil {
				return err
			}
		case "input_json_delta":
			b.json.WriteString(ev.Delta.PartialJSON)
		case "thinking_delta":
			b.Thinking += ev.Delta.Thinking
		case "signature_delta":
			b.Signature += ev.Delta.Signature
		}
	case "content_block_stop":
		b := s.blocks[ev.Index]
		if b == nil {
			return nil
		}
		delete(s.blocks, ev.Index)
		switch b.Type {
		case "tool_use":
			input := strings.TrimSpace(b.json.String())
			if input == "" {
				input = "{}"
			}
			if !json.Valid([]byte(input)) {
				return status.Errorf(codes.Unavailable, "anthropic: tool call %s has input that isn't JSON", b.ID)
			}
			if err := s.call.ToolCall(b.ID, b.Name, input); err != nil {
				return err
			}
		case "thinking", "redacted_thinking":
			s.thinking = append(s.thinking, b.Block)
		}
	case "message_delta":
		s.stop = StopReason(ev.Delta.StopReason)
		if s.usage == nil {
			s.usage = &gatewayv0.Usage{}
		}
		s.usage.OutputTokens = ev.Usage.OutputTokens
	case "message_stop":
		s.stopped = true
	case "error":
		code := codes.Unknown
		switch ev.Error.Type {
		case "overloaded_error", "api_error":
			code = codes.Unavailable
		case "rate_limit_error":
			code = codes.ResourceExhausted
		}
		return status.Errorf(code, "anthropic: %s: %s", ev.Error.Type, ev.Error.Message)
	}
	return nil
}

// continuation keeps the answer's thinking blocks for the next request.
func (s *stream) continuation() []byte {
	if len(s.thinking) == 0 {
		return nil
	}
	n := 1
	for _, m := range s.call.Request.GetMessages() {
		if m.GetRole() == gatewayv0.Role_ROLE_ASSISTANT {
			n++
		}
	}
	data, err := json.Marshal(continuationData{Assistant: n, Thinking: s.thinking})
	if err != nil {
		return nil
	}
	return data
}
