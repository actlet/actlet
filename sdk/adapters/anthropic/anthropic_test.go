package anthropic_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
	"github.com/actlet/actlet/sdk/adapters"
	"github.com/actlet/actlet/sdk/adapters/adapterstest"
	"github.com/actlet/actlet/sdk/adapters/anthropic"
	"github.com/actlet/actlet/sdk/adapters/anthropic/mock"
)

const key = "sk-ant-test-6b2f0c9d"

var prices = adapters.Prices{"claude-test": {Currency: "USD", Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}}

// setup serves the adapter, answering with respond.
func setup(t *testing.T, p adapters.Params, respond mock.Responder) (*mock.Server, *adapterstest.Harness) {
	t.Helper()
	m := mock.New(key, respond)
	t.Cleanup(m.Close)
	c := anthropic.New(anthropic.Config{BaseURL: m.URL, APIKey: key, Prices: prices})
	h := adapterstest.Serve(t, sdk.Options{OnInference: c.OnInference("claude-test", p)})
	return m, h
}

var readTool = &gatewayv0.ToolSpec{Name: "read_file", Description: "Read a file.", InputSchema: `{"type":"object","properties":{"path":{"type":"string"}}}`}

// A tool round trip: the answer streams as deltas, the tool call comes
// whole, and the next request carries the call and its result as the
// API wants them.
func TestToolRoundTrip(t *testing.T) {
	turn := 0
	m, h := setup(t, adapters.Params{MaxTokens: 2048}, func(_ context.Context, req *gatewayv0.InferenceRequest) (*mock.Answer, error) {
		turn++
		if turn == 1 {
			return &mock.Answer{Text: "Let me read it.", ToolCalls: []*gatewayv0.ToolCall{{Id: "toolu_1", Name: "read_file", Input: `{"path":"a.txt"}`}},
				Usage: mock.Usage{Input: 1000, Output: 50, CacheRead: 200, CacheWrite: 100}}, nil
		}
		return &mock.Answer{Text: "It says hi.", Usage: mock.Usage{Input: 1100, Output: 10}}, nil
	})
	ctx := t.Context()
	user := adapterstest.User("Read a.txt.")
	a, err := h.Infer(ctx, &gatewayv0.InferenceRequest{Model: "large", System: "Be brief.", Messages: []*gatewayv0.Message{user}, Tools: []*gatewayv0.ToolSpec{readTool}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Text != "Let me read it." || len(a.Deltas) < 2 || len(a.ToolCalls) != 1 || a.ToolCalls[0].GetInput() != `{"path":"a.txt"}` ||
		a.Done.GetStopReason() != gatewayv0.StopReason_STOP_REASON_TOOL_USE {
		t.Fatalf("answer %+v", a)
	}
	u := a.Done.GetUsage()
	if u.GetInputTokens() != 1000 || u.GetOutputTokens() != 50 || u.GetCacheReadTokens() != 200 || u.GetCacheWriteTokens() != 100 || u.GetCurrency() != "USD" {
		t.Errorf("usage %v", u)
	}
	if want := (1000*3 + 50*15 + 200*0.3 + 100*3.75) / 1e6; u.GetCost() != want {
		t.Errorf("cost %v, want %v", u.GetCost(), want)
	}
	results := adapterstest.Results(&gatewayv0.ToolResult{CallId: "toolu_1", Content: "hi"})
	results.Parts = append(results.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_Text{Text: "[Operator note] be brief"}})
	results.Parts[0], results.Parts[1] = results.Parts[1], results.Parts[0] // text before the result, as the agent may add it
	b, err := h.Infer(ctx, &gatewayv0.InferenceRequest{Model: "large", Messages: []*gatewayv0.Message{user, adapterstest.Assistant(a), results},
		Tools: []*gatewayv0.ToolSpec{readTool}, Continuation: a.Done.GetContinuation()})
	if err != nil || b.Text != "It says hi." || b.Done.GetStopReason() != gatewayv0.StopReason_STOP_REASON_END_TURN {
		t.Fatalf("second answer %+v, %v", b, err)
	}

	reqs := m.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	first, second := reqs[0], reqs[1]
	if first.Model != "claude-test" || first.System != "Be brief." || first.MaxTokens != 2048 || len(first.Tools) != 1 || string(first.Tools[0].InputSchema) != readTool.GetInputSchema() {
		t.Errorf("first request %+v", first)
	}
	got, _ := json.Marshal(second.Messages)
	want := `[{"role":"user","content":[{"type":"text","text":"Read a.txt."}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"Let me read it."},{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a.txt"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"hi"},{"type":"text","text":"[Operator note] be brief"}]}]`
	if string(got) != want {
		t.Errorf("second request's messages:\n%s\nwant\n%s", got, want)
	}
}

// Thinking blocks go back, signed, at the start of their assistant
// message, by way of the continuation.
func TestThinkingContinuation(t *testing.T) {
	turn := 0
	m, h := setup(t, adapters.Params{ThinkingBudget: 2000}, func(_ context.Context, req *gatewayv0.InferenceRequest) (*mock.Answer, error) {
		turn++
		if turn == 1 {
			return &mock.Answer{Thinking: "I should read it.", Signature: "sig-abc", ToolCalls: []*gatewayv0.ToolCall{{Id: "toolu_1", Name: "read_file", Input: `{}`}}}, nil
		}
		return &mock.Answer{Text: "done"}, nil
	})
	user := adapterstest.User("go")
	a, err := h.Infer(t.Context(), &gatewayv0.InferenceRequest{Model: "large", Messages: []*gatewayv0.Message{user}})
	if err != nil || len(a.Done.GetContinuation()) == 0 || a.Text != "" {
		t.Fatalf("answer %+v, %v", a, err)
	}
	if _, err := h.Infer(t.Context(), &gatewayv0.InferenceRequest{Model: "large", Continuation: a.Done.GetContinuation(),
		Messages: []*gatewayv0.Message{user, adapterstest.Assistant(a), adapterstest.Results(&gatewayv0.ToolResult{CallId: "toolu_1", Content: "x"})}}); err != nil {
		t.Fatal(err)
	}
	reqs := m.Requests()
	if reqs[0].Thinking == nil || reqs[0].Thinking.BudgetTokens != 2000 || reqs[0].MaxTokens <= 2000 {
		t.Errorf("thinking %+v, max tokens %d", reqs[0].Thinking, reqs[0].MaxTokens)
	}
	asst := reqs[1].Messages[1].Content
	if len(asst) != 2 || asst[0].Type != "thinking" || asst[0].Thinking != "I should read it." || asst[0].Signature != "sig-abc" || asst[1].Type != "tool_use" {
		t.Errorf("assistant message sent back: %+v", asst)
	}
	if _, err := h.Infer(t.Context(), &gatewayv0.InferenceRequest{Model: "large", Continuation: []byte("not ours"), Messages: []*gatewayv0.Message{user}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a foreign continuation: %v", err)
	}
}

// Errors map to what the worker retries, without the key.
func TestErrors(t *testing.T) {
	m, h := setup(t, adapters.Params{}, func(context.Context, *gatewayv0.InferenceRequest) (*mock.Answer, error) {
		return &mock.Answer{Text: "fine and dandy"}, nil
	})
	call := func() error {
		_, err := h.Infer(t.Context(), &gatewayv0.InferenceRequest{Model: "large", Messages: []*gatewayv0.Message{adapterstest.User("hi")}})
		return err
	}
	for _, tc := range []struct {
		status       int
		errType, msg string
		code         codes.Code
	}{
		{429, "rate_limit_error", "slow down", codes.ResourceExhausted},
		{529, "overloaded_error", "Overloaded", codes.Unavailable},
		{500, "api_error", "oops", codes.Unavailable},
		{400, "invalid_request_error", "messages: bad", codes.InvalidArgument},
		{401, "authentication_error", "invalid x-api-key " + key, codes.Unauthenticated},
		{404, "not_found_error", "model: claude-nope", codes.FailedPrecondition},
	} {
		m.FailNext(tc.status, tc.errType, tc.msg)
		err := call()
		if status.Code(err) != tc.code || strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "anthropic: HTTP") {
			t.Errorf("HTTP %d: %v", tc.status, err)
		}
	}
	m.FailMidStream("overloaded_error")
	if err := call(); status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "overloaded_error") {
		t.Errorf("mid-stream: %v", err)
	}
	if err := call(); err != nil {
		t.Errorf("after the failures: %v", err)
	}
	bad := anthropic.New(anthropic.Config{BaseURL: m.URL, APIKey: "wrong"})
	h2 := adapterstest.Serve(t, sdk.Options{OnInference: bad.OnInference("claude-test", adapters.Params{})})
	if _, err := h2.Infer(t.Context(), &gatewayv0.InferenceRequest{Model: "large", Messages: []*gatewayv0.Message{adapterstest.User("hi")}}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("a wrong key: %v", err)
	}
}

// A call repeated with its intent ID reaches Anthropic once.
func TestIdempotent(t *testing.T) {
	m, h := setup(t, adapters.Params{}, func(context.Context, *gatewayv0.InferenceRequest) (*mock.Answer, error) {
		return &mock.Answer{Text: "once"}, nil
	})
	for range 3 {
		a, err := h.Infer(t.Context(), &gatewayv0.InferenceRequest{IntentId: "intent-1", Model: "large", Messages: []*gatewayv0.Message{adapterstest.User("hi")}})
		if err != nil || a.Text != "once" {
			t.Fatalf("%+v, %v", a, err)
		}
	}
	if n := len(m.Requests()); n != 1 {
		t.Errorf("%d upstream calls", n)
	}
}

// Stop reasons, and the default output limit.
func TestStopReasonsAndLimits(t *testing.T) {
	stop := "max_tokens"
	m, h := setup(t, adapters.Params{}, func(context.Context, *gatewayv0.InferenceRequest) (*mock.Answer, error) {
		return &mock.Answer{Text: "cut", Stop: stop}, nil
	})
	for s, want := range map[string]gatewayv0.StopReason{
		"max_tokens": gatewayv0.StopReason_STOP_REASON_MAX_TOKENS, "refusal": gatewayv0.StopReason_STOP_REASON_REFUSAL,
		"stop_sequence": gatewayv0.StopReason_STOP_REASON_END_TURN,
	} {
		stop = s
		a, err := h.Infer(t.Context(), &gatewayv0.InferenceRequest{Model: "large", Messages: []*gatewayv0.Message{adapterstest.User("hi")}})
		if err != nil || a.Done.GetStopReason() != want {
			t.Errorf("%s: %+v, %v", s, a, err)
		}
	}
	if r := m.Requests()[0]; r.MaxTokens != anthropic.DefaultMaxTokens {
		t.Errorf("max tokens %d", r.MaxTokens)
	}
	if _, err := h.Infer(t.Context(), &gatewayv0.InferenceRequest{Model: "large", MaxOutputTokens: 77, Messages: []*gatewayv0.Message{adapterstest.User("hi")}}); err != nil {
		t.Fatal(err)
	}
	if r := m.Requests(); r[len(r)-1].MaxTokens != 77 {
		t.Errorf("the worker's limit: %d", r[len(r)-1].MaxTokens)
	}
}
