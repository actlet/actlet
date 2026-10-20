package sdk

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// infer makes a call and collects the answer.
func (h *harness) infer(t *testing.T, req *gatewayv0.InferenceRequest) ([]*gatewayv0.InferenceResponse, error) {
	t.Helper()
	stream, err := h.client.Infer(authed(t.Context()), req)
	if err != nil {
		return nil, err
	}
	var out []*gatewayv0.InferenceResponse
	for {
		r, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
}

func TestInferenceDeduplicated(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	h := newHarness(t, Options{InferenceCache: 2, OnInference: func(ctx context.Context, c *InferenceCall) error {
		calls.Add(1)
		if c.Request.GetModel() == "slow" {
			<-release
		}
		if err := c.Text("hi " + c.Request.GetIntentId()); err != nil {
			return err
		}
		return c.Done(&gatewayv0.InferenceDone{StopReason: gatewayv0.StopReason_STOP_REASON_END_TURN, Continuation: []byte("c")})
	}})
	id := h.register(t, "")
	req := func(intent, model string) *gatewayv0.InferenceRequest {
		return &gatewayv0.InferenceRequest{SessionId: id, Epoch: 1, IntentId: intent, Model: model}
	}

	first, err := h.infer(t, req("a", "m"))
	if err != nil || len(first) != 2 || first[0].GetTextDelta() != "hi a" || first[1].GetDone().GetReplayed() {
		t.Fatalf("first call: %v, %v", first, err)
	}
	again, err := h.infer(t, req("a", "m"))
	if err != nil || len(again) != 2 || again[0].GetTextDelta() != "hi a" || !again[1].GetDone().GetReplayed() ||
		string(again[1].GetDone().GetContinuation()) != "c" {
		t.Fatalf("repeated call: %v, %v", again, err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("OnInference ran %d times, want once", n)
	}

	// A repeat of a running call waits for it.
	var wg sync.WaitGroup
	results := make([][]*gatewayv0.InferenceResponse, 2)
	for i := range results {
		wg.Go(func() { results[i], _ = h.infer(t, req("b", "slow")) })
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 2 {
		t.Fatalf("OnInference ran %d times, want 2", n)
	}
	if results[0][0].GetTextDelta() != "hi b" || results[1][0].GetTextDelta() != "hi b" {
		t.Fatalf("results %v", results)
	}

	// The cache keeps two answered calls: "a" has gone.
	if _, err := h.infer(t, req("c", "m")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.infer(t, req("a", "m")); err != nil || calls.Load() != 4 {
		t.Fatalf("evicted call: %v, %d calls", err, calls.Load())
	}
}

func TestInferenceFailures(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, Options{OnInference: func(ctx context.Context, c *InferenceCall) error {
		switch calls.Add(1) {
		case 1:
			return status.Error(codes.Unavailable, "provider down")
		case 2:
			return nil // no answer
		}
		return c.Done(&gatewayv0.InferenceDone{})
	}})
	id := h.register(t, "")
	req := &gatewayv0.InferenceRequest{SessionId: id, Epoch: 1, IntentId: "x"}
	if _, err := h.infer(t, req); status.Code(err) != codes.Unavailable {
		t.Fatalf("first: %v", err)
	}
	// A failed call isn't remembered: the retry runs again.
	if _, err := h.infer(t, req); status.Code(err) != codes.Internal {
		t.Fatalf("second: %v", err)
	}
	if out, err := h.infer(t, req); err != nil || out[0].GetDone() == nil || out[0].GetDone().GetReplayed() {
		t.Fatalf("third: %v, %v", out, err)
	}

	bad := []struct {
		req  *gatewayv0.InferenceRequest
		code codes.Code
	}{
		{&gatewayv0.InferenceRequest{SessionId: id, Epoch: 1}, codes.InvalidArgument},
		{&gatewayv0.InferenceRequest{SessionId: id, IntentId: "y"}, codes.InvalidArgument},
		{&gatewayv0.InferenceRequest{SessionId: "nobody", Epoch: 1, IntentId: "y"}, codes.NotFound},
	}
	for _, b := range bad {
		if _, err := h.infer(t, b.req); status.Code(err) != b.code {
			t.Errorf("%v: %v, want %v", b.req, err, b.code)
		}
	}

	none := newHarness(t, Options{})
	id = none.register(t, "")
	if _, err := none.infer(t, &gatewayv0.InferenceRequest{SessionId: id, Epoch: 1, IntentId: "z"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("no OnInference: %v", err)
	}
}
