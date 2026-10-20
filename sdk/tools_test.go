package sdk

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

func toolReq(tool, intent, input string) *gatewayv0.ToolCallRequest {
	return &gatewayv0.ToolCallRequest{SessionId: "s1", Epoch: 1, IntentId: intent, Worker: "root", Tool: tool, Input: input}
}

// decide calls go to OnDecide once per intent ID: a repeated call gets the
// same answer; a failed one runs again.
func TestCallToolDecide(t *testing.T) {
	var asked atomic.Int32
	fail := atomic.Bool{}
	h := newHarness(t, Options{OnDecide: func(_ context.Context, d *DecideRequest) (*Decision, error) {
		asked.Add(1)
		if fail.Load() {
			return nil, errors.New("the decider is out")
		}
		if d.Question != "Ship it?" || len(d.Options) != 2 || d.Worker != "root" || d.Session.ID() != "s1" {
			return nil, status.Errorf(codes.InvalidArgument, "request %+v", d)
		}
		return &Decision{Decision: "yes", Reason: "green", DecidedBy: "alice", Usage: &gatewayv0.Usage{Cost: 0.1, Currency: "EUR"}}, nil
	}})
	h.registerWith(t, "s1")
	ctx := authed(t.Context())
	in := `{"question":"Ship it?","options":["yes","no"]}`

	resp, err := h.client.CallTool(ctx, toolReq("decide", "i1", in))
	if err != nil || resp.GetOutput() != `{"decision":"yes","reason":"green","decided_by":"alice"}` || resp.GetUsage().GetCost() != 0.1 || resp.GetReplayed() {
		t.Fatalf("decide: %v, %v", resp, err)
	}
	again, err := h.client.CallTool(ctx, toolReq("decide", "i1", in))
	if err != nil || !again.GetReplayed() || again.GetOutput() != resp.GetOutput() || asked.Load() != 1 {
		t.Errorf("asked again: %v, %v, decided %d times", again, err, asked.Load())
	}
	fail.Store(true)
	if _, err := h.client.CallTool(ctx, toolReq("decide", "i2", in)); status.Code(err) != codes.Unknown || !strings.Contains(err.Error(), "the decider is out") {
		t.Errorf("failing decider: %v", err)
	}
	fail.Store(false)
	if resp, err := h.client.CallTool(ctx, toolReq("decide", "i2", in)); err != nil || resp.GetReplayed() || asked.Load() != 3 {
		t.Errorf("after a failure: %v, %v, decided %d times", resp, err, asked.Load())
	}

	for _, tc := range []struct {
		req  *gatewayv0.ToolCallRequest
		code codes.Code
	}{
		{toolReq("lookup", "i3", `{}`), codes.Unimplemented},
		{toolReq("decide", "", in), codes.InvalidArgument},
		{&gatewayv0.ToolCallRequest{SessionId: "nope", Epoch: 1, IntentId: "i4", Tool: "decide"}, codes.NotFound},
	} {
		if _, err := h.client.CallTool(ctx, tc.req); status.Code(err) != tc.code {
			t.Errorf("%v: %v", tc.req, err)
		}
	}
	if _, err := h.client.CallTool(t.Context(), toolReq("decide", "i5", in)); status.Code(err) != codes.Unauthenticated {
		t.Errorf("without a token: %v", err)
	}
}

// Other bridged tools go to OnTool.
func TestCallToolGeneric(t *testing.T) {
	h := newHarness(t, Options{OnTool: func(_ context.Context, c *ToolCall) (*gatewayv0.ToolCallResponse, error) {
		return &gatewayv0.ToolCallResponse{Output: `{"found":"` + c.Request.GetTool() + `"}`}, nil
	}})
	h.registerWith(t, "s1")
	resp, err := h.client.CallTool(authed(t.Context()), toolReq("lookup", "i1", `{}`))
	if err != nil || resp.GetOutput() != `{"found":"lookup"}` {
		t.Errorf("lookup: %v, %v", resp, err)
	}
	// Without OnDecide, decide is OnTool's too.
	if resp, err := h.client.CallTool(authed(t.Context()), toolReq("decide", "i2", `{}`)); err != nil || resp.GetOutput() != `{"found":"decide"}` {
		t.Errorf("decide: %v, %v", resp, err)
	}
}
