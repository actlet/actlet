package sdk

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// sessionTokens gives each box its own token, bound to its session, as a
// box controller does.
var sessionTokens = map[string]string{"tok-a": "sess-a", "tok-b": "sess-b"}

func as(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func knownToken(_ context.Context, token string) error {
	if _, ok := sessionTokens[token]; !ok {
		return errors.New("unknown token")
	}
	return nil
}

// tokenSession is the session the caller's token is bound to.
func tokenSession(ctx context.Context) string {
	token, _ := TokenFromContext(ctx)
	return sessionTokens[token]
}

func TestTokenFromContext(t *testing.T) {
	for _, tt := range []struct {
		md     metadata.MD
		want   string
		wantOK bool
	}{
		{metadata.Pairs("authorization", "Bearer abc"), "abc", true},
		{metadata.Pairs("authorization", "abc"), "abc", true},
		{metadata.Pairs("other", "x"), "", false},
		{nil, "", false},
	} {
		ctx := t.Context()
		if tt.md != nil {
			ctx = metadata.NewIncomingContext(ctx, tt.md)
		}
		if got, ok := TokenFromContext(ctx); got != tt.want || ok != tt.wantOK {
			t.Errorf("%v: %q, %v", tt.md, got, ok)
		}
	}
}

// Assign can bind a worker's token to its session: a worker that asks
// for another session is refused, and one that asks for none gets its own.
func TestAssignBindsTokenToSession(t *testing.T) {
	h := newHarness(t, Options{
		Authenticate: knownToken,
		Assign: func(ctx context.Context, req *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
			own := tokenSession(ctx)
			if req.GetSessionId() != "" && req.GetSessionId() != own {
				return nil, status.Errorf(codes.PermissionDenied, "this token is for another session")
			}
			return &gatewayv0.RegisterResponse{SessionId: own, Profile: &gatewayv0.Profile{Id: "test"}}, nil
		},
	})
	register := func(token, session string) (string, error) {
		resp, err := h.client.Register(as(t.Context(), token), &gatewayv0.RegisterRequest{
			ProtocolVersion: ProtocolVersion, WorkerId: "w", SessionId: session, Epoch: 1})
		return resp.GetSessionId(), err
	}
	if id, err := register("tok-a", ""); err != nil || id != "sess-a" {
		t.Fatalf("a new box: %q, %v", id, err)
	}
	if id, err := register("tok-a", "sess-a"); err != nil || id != "sess-a" {
		t.Fatalf("resuming its own session: %q, %v", id, err)
	}
	if _, err := register("tok-a", "sess-b"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("another session: %v, want PermissionDenied", err)
	}
}

// With AuthorizeSession, a token bound to session A is refused for
// session B wherever a worker names a session, also where B has an answer
// cached for the intent ID; A keeps working.
func TestAuthorizeSession(t *testing.T) {
	var mu sync.Mutex
	var asked []string // the sessions OnInference and OnTool ran for
	ask := func(id string) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, id)
	}
	h := newHarness(t, Options{
		Authenticate: knownToken,
		AuthorizeSession: func(ctx context.Context, id string) error {
			if tokenSession(ctx) != id {
				return errors.New("this token is for another session")
			}
			return nil
		},
		OnInference: func(_ context.Context, call *InferenceCall) error {
			ask(call.Session.ID())
			if err := call.Text("for " + call.Session.ID()); err != nil {
				return err
			}
			return call.Done(&gatewayv0.InferenceDone{StopReason: gatewayv0.StopReason_STOP_REASON_END_TURN})
		},
		OnTool: func(_ context.Context, call *ToolCall) (*gatewayv0.ToolCallResponse, error) {
			ask(call.Session.ID())
			return &gatewayv0.ToolCallResponse{Output: "for " + call.Session.ID()}, nil
		},
	})
	ctxA, ctxB := as(t.Context(), "tok-a"), as(t.Context(), "tok-b")
	denied := func(what string, err error) {
		t.Helper()
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: %v, want PermissionDenied", what, err)
		}
	}
	register := func(ctx context.Context, session string) error {
		_, err := h.client.Register(ctx, &gatewayv0.RegisterRequest{ProtocolVersion: ProtocolVersion, WorkerId: "w", SessionId: session, Epoch: 1})
		return err
	}
	infer := func(ctx context.Context, session string) (string, error) {
		stream, err := h.client.Infer(ctx, &gatewayv0.InferenceRequest{SessionId: session, Epoch: 1, IntentId: "i1", Model: "m"})
		if err != nil {
			return "", err
		}
		var text string
		for {
			r, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return text, nil
			}
			if err != nil {
				return "", err
			}
			text += r.GetTextDelta()
		}
	}
	callTool := func(ctx context.Context, session string) (string, error) {
		resp, err := h.client.CallTool(ctx, &gatewayv0.ToolCallRequest{SessionId: session, Epoch: 1, IntentId: "t1", Worker: "root", Tool: "lookup", Input: "{}"})
		return resp.GetOutput(), err
	}

	// Register, and an answer cached for each of B's intent IDs.
	for ctx, id := range map[context.Context]string{ctxA: "sess-a", ctxB: "sess-b"} {
		if err := register(ctx, id); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	if out, err := infer(ctxB, "sess-b"); err != nil || out != "for sess-b" {
		t.Fatalf("B's model call: %q, %v", out, err)
	}
	if out, err := callTool(ctxB, "sess-b"); err != nil || out != "for sess-b" {
		t.Fatalf("B's tool call: %q, %v", out, err)
	}

	// A's token, for session B.
	denied("Register", register(ctxA, "sess-b"))
	control, err := h.client.Control(ctxA)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.Send(&gatewayv0.ControlUp{Msg: &gatewayv0.ControlUp_Hello{Hello: &gatewayv0.ControlHello{SessionId: "sess-b", Epoch: 1}}}); err != nil {
		t.Fatal(err)
	}
	_, err = control.Recv()
	denied("Control", err)
	activity, err := h.client.PublishActivity(ctxA)
	if err != nil {
		t.Fatal(err)
	}
	if err := activity.Send(&gatewayv0.ActivityUp{Msg: &gatewayv0.ActivityUp_Hello{Hello: &gatewayv0.ActivityHello{SessionId: "sess-b", Epoch: 1}}}); err != nil {
		t.Fatal(err)
	}
	_, err = activity.Recv()
	denied("PublishActivity", err)
	out, err := infer(ctxA, "sess-b")
	denied("Infer, with B's answer cached", err)
	if out != "" {
		t.Errorf("Infer gave A B's answer %q", out)
	}
	out, err = callTool(ctxA, "sess-b")
	denied("CallTool, with B's answer cached", err)
	if out != "" {
		t.Errorf("CallTool gave A B's answer %q", out)
	}
	mu.Lock()
	if len(asked) != 2 {
		t.Errorf("the hooks ran for %v", asked)
	}
	mu.Unlock()

	// A, for its own session.
	w := h.connectControlAs(t, ctxA, "sess-a")
	go func() { _, _ = h.srv.Session("sess-a").Steer(t.Context(), "carry on") }()
	if cmd := w.nextCommand(t); cmd.GetSteer().GetNote() != "carry on" {
		t.Errorf("A's control stream got %v", cmd)
	}
	activity, err = h.client.PublishActivity(ctxA)
	if err != nil {
		t.Fatal(err)
	}
	if err := activity.Send(&gatewayv0.ActivityUp{Msg: &gatewayv0.ActivityUp_Hello{Hello: &gatewayv0.ActivityHello{SessionId: "sess-a", Epoch: 1}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := activity.Recv(); err != nil {
		t.Errorf("A's activity stream: %v", err)
	}
	if out, err := infer(ctxA, "sess-a"); err != nil || out != "for sess-a" {
		t.Errorf("A's model call: %q, %v", out, err)
	}
	if out, err := callTool(ctxA, "sess-a"); err != nil || out != "for sess-a" {
		t.Errorf("A's tool call: %q, %v", out, err)
	}
}

func (h *harness) connectControlAs(t *testing.T, ctx context.Context, sessionID string) *worker {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	stream, err := h.client.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = stream.Send(&gatewayv0.ControlUp{Msg: &gatewayv0.ControlUp_Hello{Hello: &gatewayv0.ControlHello{
		SessionId: sessionID, Epoch: 1, State: gatewayv0.SessionState_SESSION_STATE_RUNNING,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return &worker{stream: stream, cancel: cancel}
}
