package scripted

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

const script = `
turns:
  - text: Reading the input.
    tool_calls:
      - name: read_file
        input: {path: ../inputs/a.txt}
  - expect:
      - contains: "42"
        is_error: false
    text: Done.
`

func gateway(t *testing.T, m *Model) (gatewayv0.GatewayClient, string) {
	t.Helper()
	srv, err := sdk.NewServer(sdk.Options{
		OnInference: m.Serve,
		Assign: func(context.Context, *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
			return &gatewayv0.RegisterResponse{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	srv.RegisterService(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	client := gatewayv0.NewGatewayClient(cc)
	resp, err := client.Register(t.Context(), &gatewayv0.RegisterRequest{ProtocolVersion: sdk.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	return client, resp.GetSessionId()
}

// answer collects one call's answer.
func answer(t *testing.T, c gatewayv0.GatewayClient, req *gatewayv0.InferenceRequest) (string, []*gatewayv0.ToolCall, *gatewayv0.InferenceDone, error) {
	t.Helper()
	stream, err := c.Infer(t.Context(), req)
	if err != nil {
		return "", nil, nil, err
	}
	var text strings.Builder
	var calls []*gatewayv0.ToolCall
	for {
		r, err := stream.Recv()
		if err != nil {
			return text.String(), calls, nil, err
		}
		text.WriteString(r.GetTextDelta())
		if tc := r.GetToolCall(); tc != nil {
			calls = append(calls, tc)
		}
		if d := r.GetDone(); d != nil {
			return text.String(), calls, d, nil
		}
	}
}

func TestScriptedModel(t *testing.T) {
	m, err := Parse([]byte(script))
	if err != nil {
		t.Fatal(err)
	}
	c, id := gateway(t, m)
	user := &gatewayv0.Message{Role: gatewayv0.Role_ROLE_USER, Parts: []*gatewayv0.Part{{Part: &gatewayv0.Part_Text{Text: "task"}}}}

	text, calls, done, err := answer(t, c, &gatewayv0.InferenceRequest{SessionId: id, Epoch: 1, IntentId: "i1", Messages: []*gatewayv0.Message{user}})
	if err != nil || text != "Reading the input." || len(calls) != 1 || calls[0].GetName() != "read_file" ||
		calls[0].GetInput() != `{"path":"../inputs/a.txt"}` || done.GetStopReason() != gatewayv0.StopReason_STOP_REASON_TOOL_USE {
		t.Fatalf("turn 1: %q %v %v %v", text, calls, done, err)
	}
	assistant := &gatewayv0.Message{Role: gatewayv0.Role_ROLE_ASSISTANT, Parts: []*gatewayv0.Part{{Part: &gatewayv0.Part_ToolCall{ToolCall: calls[0]}}}}
	result := func(content string) *gatewayv0.Message {
		return &gatewayv0.Message{Role: gatewayv0.Role_ROLE_USER, Parts: []*gatewayv0.Part{{Part: &gatewayv0.Part_ToolResult{ToolResult: &gatewayv0.ToolResult{CallId: calls[0].GetId(), Content: content}}}}}
	}
	turn2 := func(intent, content string, cont []byte) (string, *gatewayv0.InferenceDone, error) {
		text, _, done, err := answer(t, c, &gatewayv0.InferenceRequest{SessionId: id, Epoch: 1, IntentId: intent,
			Messages: []*gatewayv0.Message{user, assistant, result(content)}, Continuation: cont})
		return text, done, err
	}

	if _, _, err := turn2("i2", "41", done.GetContinuation()); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), `doesn't contain "42"`) {
		t.Errorf("unexpected tool result: %v", err)
	}
	if _, _, err := turn2("i3", "42", []byte("bogus")); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "continuation") {
		t.Errorf("wrong continuation: %v", err)
	}
	text, done2, err := turn2("i4", "the answer is 42", done.GetContinuation())
	if err != nil || text != "Done." || done2.GetStopReason() != gatewayv0.StopReason_STOP_REASON_END_TURN {
		t.Fatalf("turn 2: %q %v %v", text, done2, err)
	}
	if got := len(m.Calls()); got != 4 {
		t.Errorf("%d calls recorded", got)
	}
}

func TestSubagentScripts(t *testing.T) {
	m, err := Parse([]byte(`
turns:
  - text: main
subagents:
  - match: count words
    turns:
      - text: sub
`))
	if err != nil {
		t.Fatal(err)
	}
	c, id := gateway(t, m)
	for _, tt := range []struct{ first, want, script string }{
		{"# Task\n\nbig task", "main", ""},
		{"# Subtask from root\n\nplease count words in a.txt", "sub", "count words"},
	} {
		msg := &gatewayv0.Message{Role: gatewayv0.Role_ROLE_USER, Parts: []*gatewayv0.Part{{Part: &gatewayv0.Part_Text{Text: tt.first}}}}
		text, _, _, err := answer(t, c, &gatewayv0.InferenceRequest{SessionId: id, Epoch: 1, IntentId: tt.first, Messages: []*gatewayv0.Message{msg}})
		if err != nil || text != tt.want {
			t.Errorf("%q: %q, %v", tt.first, text, err)
		}
	}
	if calls := m.Calls(); len(calls) != 2 || calls[0].Script != "" || calls[1].Script != "count words" {
		t.Errorf("calls %+v", calls)
	}
}

func TestExpectOnce(t *testing.T) {
	m, err := Parse([]byte("turns:\n  - expect_once: [metric]\n    text: ok\n"))
	if err != nil {
		t.Fatal(err)
	}
	c, id := gateway(t, m)
	for _, tt := range []struct {
		text string
		ok   bool
	}{{"use metric units", true}, {"nothing", false}, {"metric, metric", false}} {
		msg := &gatewayv0.Message{Role: gatewayv0.Role_ROLE_USER, Parts: []*gatewayv0.Part{{Part: &gatewayv0.Part_Text{Text: tt.text}}}}
		_, _, _, err := answer(t, c, &gatewayv0.InferenceRequest{SessionId: id, Epoch: 1, IntentId: tt.text, Messages: []*gatewayv0.Message{msg}})
		if (err == nil) != tt.ok {
			t.Errorf("%q: %v", tt.text, err)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{
		"turns: []",
		"turns:\n  - text: hi\n    colour: red",
		"turns:\n  - tool_calls:\n      - input: {}",
		"turns:\n  - delay: soon",
		"turns:\n  - text: a\nsubagents:\n  - turns:\n      - text: b",
		"turns:\n  - text: a\nsubagents:\n  - match: x\n    turns: []",
	} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

// Decisions, llm answers and per-call costs.
func TestDecisionsAndLLM(t *testing.T) {
	m, err := Parse([]byte(`
price: {currency: EUR, input: 1, output: 2}
turns:
  - text: hi
    cost: 0.25
decisions:
  - match: Ship
    decision: ship
    reason: green
llm:
  - match: summarise
    text: short
    usage: {input: 10, output: 2}
    cost: 0.01
`))
	if err != nil {
		t.Fatal(err)
	}
	c, id := gateway(t, m)
	user := func(s string) []*gatewayv0.Message {
		return []*gatewayv0.Message{{Role: gatewayv0.Role_ROLE_USER, Parts: []*gatewayv0.Part{{Part: &gatewayv0.Part_Text{Text: s}}}}}
	}
	_, _, done, err := answer(t, c, &gatewayv0.InferenceRequest{SessionId: id, Epoch: 1, IntentId: "t1", Messages: user("task")})
	if err != nil || done.GetUsage().GetCost() != 0.25 || done.GetUsage().GetCurrency() != "EUR" {
		t.Errorf("turn cost: %v, %v", done, err)
	}
	text, _, done, err := answer(t, c, &gatewayv0.InferenceRequest{SessionId: id, Epoch: 1, IntentId: "l1", Purpose: "llm", Messages: user("please summarise this")})
	if err != nil || text != "short" || done.GetUsage().GetInputTokens() != 10 || done.GetUsage().GetCost() != 0.01 {
		t.Errorf("llm: %q %v %v", text, done, err)
	}
	if _, _, _, err := answer(t, c, &gatewayv0.InferenceRequest{SessionId: id, Epoch: 1, IntentId: "l2", Purpose: "llm", Messages: user("translate")}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("unscripted llm call: %v", err)
	}
	d, err := m.Decide(t.Context(), &sdk.DecideRequest{IntentID: "d1", Question: "Ship it?"})
	if err != nil || d.Decision != "ship" || d.Reason != "green" || fmt.Sprint(m.Decides()) != "[d1]" {
		t.Errorf("decide: %+v, %v", d, err)
	}
	if _, err := m.Decide(t.Context(), &sdk.DecideRequest{Question: "Lunch?"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("unscripted decision: %v", err)
	}
	if _, err := Parse([]byte("turns:\n  - text: a\n    cost: 1")); err == nil {
		t.Error("a cost without a price was accepted")
	}
}

// Compaction calls get the summary, and a compacted conversation, with
// fewer answers than turns played, plays on from its continuation.
func TestCompaction(t *testing.T) {
	m, err := Parse([]byte(`
turns:
  - text: one
  - text: two
  - text: three
summary:
  text: Said one and two.
  usage: {input: 50, output: 5}
`))
	if err != nil {
		t.Fatal(err)
	}
	c, id := gateway(t, m)
	msg := func(role gatewayv0.Role, s string) *gatewayv0.Message {
		return &gatewayv0.Message{Role: role, Parts: []*gatewayv0.Part{{Part: &gatewayv0.Part_Text{Text: s}}}}
	}
	text, _, done, err := answer(t, c, &gatewayv0.InferenceRequest{SessionId: id, Epoch: 1, IntentId: "c1", Purpose: "compaction",
		Messages: []*gatewayv0.Message{msg(gatewayv0.Role_ROLE_USER, "the transcript")}})
	if err != nil || text != "Said one and two." || done.GetUsage().GetInputTokens() != 50 {
		t.Fatalf("summary: %q %v %v", text, done, err)
	}
	text, _, _, err = answer(t, c, &gatewayv0.InferenceRequest{SessionId: id, Epoch: 1, IntentId: "t3", Continuation: continuation(2),
		Messages: []*gatewayv0.Message{msg(gatewayv0.Role_ROLE_USER, "task and summary"), msg(gatewayv0.Role_ROLE_ASSISTANT, "two"), msg(gatewayv0.Role_ROLE_USER, "go on")}})
	if err != nil || text != "three" {
		t.Errorf("after the compaction: %q %v", text, err)
	}
}
