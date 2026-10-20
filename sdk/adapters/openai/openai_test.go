package openai_test

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
	"github.com/actlet/actlet/sdk/adapters/nexos"
	"github.com/actlet/actlet/sdk/adapters/openai"
	"github.com/actlet/actlet/sdk/adapters/openai/mock"
)

const key = "sk-proj-test-41e7a0c2"

var prices = adapters.Prices{"gpt-test": {Currency: "USD", Input: 1.25, Output: 10, CacheRead: 0.125}}

func setup(t *testing.T, cfg openai.Config, respond mock.Responder) (*mock.Server, *adapterstest.Harness) {
	t.Helper()
	m := mock.New(key, respond)
	t.Cleanup(m.Close)
	cfg.BaseURL, cfg.APIKey, cfg.Prices = m.BaseURL(), key, prices
	c := openai.New(cfg)
	h := adapterstest.Serve(t, sdk.Options{OnInference: c.OnInference("gpt-test", adapters.Params{MaxTokens: 1000})})
	return m, h
}

var readTool = &gatewayv0.ToolSpec{Name: "read_file", Description: "Read a file.", InputSchema: `{"type":"object","properties":{"path":{"type":"string"}}}`}

// A tool round trip, with two parallel calls whose arguments stream in
// pieces, and results that go back as tool messages.
func TestToolRoundTrip(t *testing.T) {
	turn := 0
	m, h := setup(t, openai.Config{}, func(_ context.Context, req *gatewayv0.InferenceRequest) (*mock.Answer, error) {
		turn++
		if turn == 1 {
			return &mock.Answer{Text: "Reading both.", ToolCalls: []*gatewayv0.ToolCall{
				{Id: "call_a", Name: "read_file", Input: `{"path":"a.txt"}`}, {Id: "call_b", Name: "read_file", Input: `{"path":"b.txt"}`},
			}, Usage: mock.Usage{Prompt: 800, Completion: 40, Cached: 200}}, nil
		}
		return &mock.Answer{Text: "Both say hi."}, nil
	})
	ctx := t.Context()
	user := adapterstest.User("Read a.txt and b.txt.")
	a, err := h.Infer(ctx, &gatewayv0.InferenceRequest{Model: "large", System: "Be brief.", Messages: []*gatewayv0.Message{user}, Tools: []*gatewayv0.ToolSpec{readTool}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Text != "Reading both." || len(a.ToolCalls) != 2 || a.ToolCalls[0].GetId() != "call_a" || a.ToolCalls[1].GetInput() != `{"path":"b.txt"}` ||
		a.Done.GetStopReason() != gatewayv0.StopReason_STOP_REASON_TOOL_USE {
		t.Fatalf("answer %+v", a)
	}
	u := a.Done.GetUsage()
	if u.GetInputTokens() != 800 || u.GetCacheReadTokens() != 200 || u.GetOutputTokens() != 40 || u.GetCost() != (800*1.25+40*10+200*0.125)/1e6 {
		t.Errorf("usage %v", u)
	}
	results := adapterstest.Results(&gatewayv0.ToolResult{CallId: "call_a", Content: "hi"}, &gatewayv0.ToolResult{CallId: "call_b", Content: "no such file", IsError: true})
	results.Parts = append(results.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_Text{Text: "[Operator note] hurry"}})
	b, err := h.Infer(ctx, &gatewayv0.InferenceRequest{Model: "large", Messages: []*gatewayv0.Message{user, adapterstest.Assistant(a), results}, Tools: []*gatewayv0.ToolSpec{readTool}})
	if err != nil || b.Text != "Both say hi." || b.Done.GetStopReason() != gatewayv0.StopReason_STOP_REASON_END_TURN || len(b.Done.GetContinuation()) != 0 {
		t.Fatalf("second answer %+v, %v", b, err)
	}
	reqs := m.Requests()
	if r := reqs[0]; r.Model != "gpt-test" || r.MaxCompletionTokens != 1000 || r.MaxTokens != 0 || len(r.Tools) != 1 || string(r.Tools[0].Function.Parameters) != readTool.GetInputSchema() {
		t.Errorf("first request %+v", r)
	}
	got, _ := json.Marshal(reqs[1].Messages)
	want := `[{"role":"user","content":"Read a.txt and b.txt."},` +
		`{"role":"assistant","content":"Reading both.","tool_calls":[{"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}},{"id":"call_b","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"b.txt\"}"}}]},` +
		`{"role":"tool","content":"hi","tool_call_id":"call_a"},{"role":"tool","content":"Error: no such file","tool_call_id":"call_b"},` +
		`{"role":"user","content":"[Operator note] hurry"}]`
	if string(got) != want {
		t.Errorf("second request's messages:\n%s\nwant\n%s", got, want)
	}
}

// Errors map to what the worker retries, without the key; a quota that
// ran out isn't retried.
func TestErrors(t *testing.T) {
	m, h := setup(t, openai.Config{}, func(context.Context, *gatewayv0.InferenceRequest) (*mock.Answer, error) {
		return &mock.Answer{Text: "all good"}, nil
	})
	call := func() error {
		_, err := h.Infer(t.Context(), &gatewayv0.InferenceRequest{Model: "large", Messages: []*gatewayv0.Message{adapterstest.User("hi")}})
		return err
	}
	for _, tc := range []struct {
		status             int
		errType, code, msg string
		want               codes.Code
	}{
		{429, "requests", "rate_limit_exceeded", "Rate limit reached", codes.ResourceExhausted},
		{429, "insufficient_quota", "insufficient_quota", "You exceeded your current quota", codes.FailedPrecondition},
		{503, "server_error", "", "The engine is currently overloaded", codes.Unavailable},
		{400, "invalid_request_error", "context_length_exceeded", "too long", codes.InvalidArgument},
		{401, "invalid_request_error", "invalid_api_key", "Incorrect API key provided: " + key, codes.Unauthenticated},
	} {
		m.FailNext(tc.status, tc.errType, tc.code, tc.msg)
		err := call()
		if status.Code(err) != tc.want || strings.Contains(err.Error(), key) {
			t.Errorf("HTTP %d %s: %v", tc.status, tc.code, err)
		}
	}
	m.FailMidStream()
	if err := call(); status.Code(err) != codes.Unavailable {
		t.Errorf("mid-stream: %v", err)
	}
	if err := call(); err != nil {
		t.Errorf("after the failures: %v", err)
	}
}

// A refusal, and a cut-off answer.
func TestStops(t *testing.T) {
	answer := &mock.Answer{Refusal: "I can't help with that."}
	_, h := setup(t, openai.Config{}, func(context.Context, *gatewayv0.InferenceRequest) (*mock.Answer, error) { return answer, nil })
	a, err := h.Infer(t.Context(), &gatewayv0.InferenceRequest{Model: "large", Messages: []*gatewayv0.Message{adapterstest.User("hi")}})
	if err != nil || a.Done.GetStopReason() != gatewayv0.StopReason_STOP_REASON_REFUSAL || a.Text != "I can't help with that." {
		t.Errorf("refusal %+v, %v", a, err)
	}
	answer = &mock.Answer{Text: "cut", Finish: "length"}
	if a, err := h.Infer(t.Context(), &gatewayv0.InferenceRequest{Model: "large", Messages: []*gatewayv0.Message{adapterstest.User("hi")}}); err != nil || a.Done.GetStopReason() != gatewayv0.StopReason_STOP_REASON_MAX_TOKENS {
		t.Errorf("length %+v, %v", a, err)
	}
}

// A call repeated with its intent ID reaches the provider once.
func TestIdempotent(t *testing.T) {
	m, h := setup(t, openai.Config{}, func(context.Context, *gatewayv0.InferenceRequest) (*mock.Answer, error) {
		return &mock.Answer{Text: "once"}, nil
	})
	for range 3 {
		if _, err := h.Infer(t.Context(), &gatewayv0.InferenceRequest{IntentId: "intent-1", Model: "large", Messages: []*gatewayv0.Message{adapterstest.User("hi")}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(m.Requests()); n != 1 {
		t.Errorf("%d upstream calls", n)
	}
}

// An OpenAI-compatible endpoint, such as nexos.ai's, by base URL, with
// max_tokens.
func TestCompatible(t *testing.T) {
	m := mock.New(key, func(context.Context, *gatewayv0.InferenceRequest) (*mock.Answer, error) {
		return &mock.Answer{Text: "hi"}, nil
	})
	t.Cleanup(m.Close)
	c := nexos.New(openai.Config{BaseURL: m.BaseURL(), APIKey: key})
	h := adapterstest.Serve(t, sdk.Options{OnInference: c.OnInference("gpt-4.1-nexos", adapters.Params{MaxTokens: 500})})
	if a, err := h.Infer(t.Context(), &gatewayv0.InferenceRequest{Model: "large", Messages: []*gatewayv0.Message{adapterstest.User("hi")}}); err != nil || a.Text != "hi" {
		t.Fatalf("%+v, %v", a, err)
	}
	if r := m.Requests()[0]; r.MaxTokens != 500 || r.MaxCompletionTokens != 0 || r.Model != "gpt-4.1-nexos" {
		t.Errorf("request %+v", r)
	}
	m.FailNext(401, "invalid_request_error", "invalid_api_key", "bad key "+key)
	if _, err := h.Infer(t.Context(), &gatewayv0.InferenceRequest{Model: "large", Messages: []*gatewayv0.Message{adapterstest.User("hi")}}); !strings.Contains(err.Error(), "nexos: HTTP 401") || strings.Contains(err.Error(), key) {
		t.Errorf("errors name nexos: %v", err)
	}
}
