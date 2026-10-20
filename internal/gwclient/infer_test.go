package gwclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

func text(s string) *gatewayv0.InferenceResponse {
	return &gatewayv0.InferenceResponse{Msg: &gatewayv0.InferenceResponse_TextDelta{TextDelta: s}}
}

func toolCall(id string) *gatewayv0.InferenceResponse {
	return &gatewayv0.InferenceResponse{Msg: &gatewayv0.InferenceResponse_ToolCall{ToolCall: &gatewayv0.ToolCall{Id: id, Name: "shell", Input: "{}"}}}
}

func done() *gatewayv0.InferenceResponse {
	return &gatewayv0.InferenceResponse{Msg: &gatewayv0.InferenceResponse_Done{Done: &gatewayv0.InferenceDone{
		StopReason: gatewayv0.StopReason_STOP_REASON_TOOL_USE, Continuation: []byte("next"),
	}}}
}

// attempts answers each attempt of a call with its own script; an error
// ends the attempt.
type attempts struct {
	mu      sync.Mutex
	scripts [][]any // *InferenceResponse, error or time.Duration (a pause)
	intents []string
}

func (a *attempts) serve(req *gatewayv0.InferenceRequest, s gatewayv0.Gateway_InferServer) error {
	a.mu.Lock()
	n := len(a.intents)
	a.intents = append(a.intents, req.GetIntentId())
	a.mu.Unlock()
	for _, step := range a.scripts[min(n, len(a.scripts)-1)] {
		switch v := step.(type) {
		case *gatewayv0.InferenceResponse:
			if err := s.Send(v); err != nil {
				return err
			}
		case error:
			return v
		case time.Duration:
			select {
			case <-time.After(v):
			case <-s.Context().Done():
				return s.Context().Err()
			}
		}
	}
	return nil
}

func TestInfer(t *testing.T) {
	req := &gatewayv0.InferenceRequest{SessionId: "s", Epoch: 1, IntentId: "intent-1"}
	want := &gatewayv0.Message{Role: gatewayv0.Role_ROLE_ASSISTANT, Parts: []*gatewayv0.Part{
		{Part: &gatewayv0.Part_Text{Text: "Let me look."}},
		{Part: &gatewayv0.Part_ToolCall{ToolCall: &gatewayv0.ToolCall{Id: "a", Name: "shell", Input: "{}"}}},
		{Part: &gatewayv0.Part_Text{Text: "And this."}},
		{Part: &gatewayv0.Part_ToolCall{ToolCall: &gatewayv0.ToolCall{Id: "b", Name: "shell", Input: "{}"}}},
	}}
	answer := []any{text("Let "), text("me look."), toolCall("a"), text("And this."), toolCall("b"), done()}
	tests := []struct {
		name     string
		attempts [][]any
		attempt  int
		err      codes.Code
	}{
		{"first time", [][]any{answer}, 1, codes.OK},
		{"retried after a failure mid-answer", [][]any{{text("Partial "), status.Error(codes.Unavailable, "gone")}, answer}, 2, codes.OK},
		{"retried when the answer ends early", [][]any{{text("Partial")}, answer}, 2, codes.OK},
		{"retried when rate limited", [][]any{{status.Error(codes.ResourceExhausted, "slow down")}, answer}, 2, codes.OK},
		{"retried when idle", [][]any{{text("Partial"), time.Second}, answer}, 2, codes.OK},
		{"permanent failure", [][]any{{status.Error(codes.FailedPrecondition, "no such model")}}, 1, codes.FailedPrecondition},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &attempts{scripts: tt.attempts}
			c := serve(t, &fakeGateway{infer: a.serve}, Config{InferenceIdle: 200 * time.Millisecond})
			got, err := c.Infer(ctxT(t, 5*time.Second), req, nil)
			if status.Code(err) != tt.err {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if len(a.intents) != tt.attempt {
				t.Fatalf("%d attempts, want %d", len(a.intents), tt.attempt)
			}
			for _, id := range a.intents {
				if id != "intent-1" {
					t.Errorf("attempt with intent %q", id)
				}
			}
			if err != nil {
				return
			}
			if !proto.Equal(got.Message, want) {
				t.Errorf("message %v, want %v", got.Message, want)
			}
			if string(got.Done.GetContinuation()) != "next" || got.Attempts != tt.attempt {
				t.Errorf("done %v after %d attempts", got.Done, got.Attempts)
			}
		})
	}
}

func TestInferReregisters(t *testing.T) {
	a := &attempts{scripts: [][]any{{status.Error(codes.NotFound, "unknown session")}, {done()}}}
	c := serve(t, &fakeGateway{infer: a.serve}, Config{})
	registered := 0
	_, err := c.Infer(ctxT(t, 5*time.Second), &gatewayv0.InferenceRequest{IntentId: "i"}, func(context.Context) error {
		registered++
		return nil
	})
	if err != nil || registered != 1 {
		t.Fatalf("err %v, registered %d times", err, registered)
	}
}

func TestInferCancelled(t *testing.T) {
	a := &attempts{scripts: [][]any{{time.Minute}}}
	c := serve(t, &fakeGateway{infer: a.serve}, Config{})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Infer(ctx, &gatewayv0.InferenceRequest{IntentId: "i"}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

// A bridged tool call is retried with the same intent ID while the
// gateway is unavailable, and not when it refuses the call.
func TestCallToolRetries(t *testing.T) {
	var intents []string
	var mu sync.Mutex
	c := serve(t, &fakeGateway{callTool: func(_ context.Context, req *gatewayv0.ToolCallRequest) (*gatewayv0.ToolCallResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		intents = append(intents, req.GetIntentId())
		switch {
		case req.GetTool() == "missing":
			return nil, status.Error(codes.Unimplemented, "no such tool")
		case len(intents) < 3:
			return nil, status.Error(codes.Unavailable, "restarting")
		}
		return &gatewayv0.ToolCallResponse{Output: `{"decision":"yes"}`}, nil
	}}, Config{})
	resp, err := c.CallTool(t.Context(), &gatewayv0.ToolCallRequest{IntentId: "i1", Tool: "decide"}, nil)
	if err != nil || resp.GetOutput() != `{"decision":"yes"}` || fmt.Sprint(intents) != "[i1 i1 i1]" {
		t.Fatalf("%v, %v after %v", resp, err, intents)
	}
	if _, err := c.CallTool(t.Context(), &gatewayv0.ToolCallRequest{IntentId: "i2", Tool: "missing"}, nil); status.Code(err) != codes.Unimplemented || len(intents) != 4 {
		t.Errorf("refused call: %v after %d attempts", err, len(intents))
	}
}

// Rate limits are retried; a call larger than the gateway accepts is not,
// since it stays too large.
func TestRetryable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{status.Error(codes.ResourceExhausted, "rate limited"), true},
		{status.Error(codes.ResourceExhausted, "grpc: received message larger than max (5242880 vs. 4194304)"), false},
		{status.Error(codes.Unavailable, "down"), true},
		{status.Error(codes.InvalidArgument, "bad"), false},
		{errIdle, true},
	} {
		if got := retryable(tc.err); got != tc.want {
			t.Errorf("%v: retryable %v", tc.err, got)
		}
	}
}
