package scripted

import (
	"context"
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

func TestParseErrors(t *testing.T) {
	for _, s := range []string{
		"turns: []",
		"turns:\n  - text: hi\n    colour: red",
		"turns:\n  - tool_calls:\n      - input: {}",
		"turns:\n  - delay: soon",
	} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}
