// Package adapterstest runs model adapters behind a real SDK server, for
// testing them: calls go over gRPC on an in-memory connection, as a
// worker's would, and answers are collected.
package adapterstest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

// Harness is an SDK server with a registered session.
type Harness struct {
	Client  gatewayv0.GatewayClient
	Session string
}

// Serve starts an SDK server with the options, which must set the hooks
// under test, and registers a session.
func Serve(t testing.TB, opts sdk.Options) *Harness {
	t.Helper()
	if opts.Assign == nil {
		opts.Assign = func(context.Context, *gatewayv0.RegisterRequest) (*gatewayv0.RegisterResponse, error) {
			return &gatewayv0.RegisterResponse{Profile: &gatewayv0.Profile{Id: "test"}}, nil
		}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	srv, err := sdk.NewServer(opts)
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
	resp, err := client.Register(context.Background(), &gatewayv0.RegisterRequest{ProtocolVersion: sdk.ProtocolVersion, WorkerId: "w1", Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	return &Harness{Client: client, Session: resp.GetSessionId()}
}

// Answer is a model call's answer as a worker gets it.
type Answer struct {
	// Deltas are the text deltas, in order; Text joins them.
	Deltas    []string
	Text      string
	ToolCalls []*gatewayv0.ToolCall
	Done      *gatewayv0.InferenceDone
}

// Infer makes a model call for the session, filling in its session,
// epoch and, if missing, intent ID.
func (h *Harness) Infer(ctx context.Context, req *gatewayv0.InferenceRequest) (*Answer, error) {
	req.SessionId, req.Epoch = h.Session, 1
	if req.IntentId == "" {
		req.IntentId = sdk.NewID()
	}
	stream, err := h.Client.Infer(ctx, req)
	if err != nil {
		return nil, err
	}
	a := &Answer{}
	var text strings.Builder
	for {
		r, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return a, errors.New("the answer ended without being done")
		}
		if err != nil {
			return a, err
		}
		switch m := r.Msg.(type) {
		case *gatewayv0.InferenceResponse_TextDelta:
			a.Deltas = append(a.Deltas, m.TextDelta)
			text.WriteString(m.TextDelta)
			a.Text = text.String()
		case *gatewayv0.InferenceResponse_ToolCall:
			a.ToolCalls = append(a.ToolCalls, m.ToolCall)
		case *gatewayv0.InferenceResponse_Done:
			a.Done = m.Done
			return a, nil
		}
	}
}

// CallTool calls a bridged tool for the session.
func (h *Harness) CallTool(ctx context.Context, req *gatewayv0.ToolCallRequest) (*gatewayv0.ToolCallResponse, error) {
	req.SessionId, req.Epoch = h.Session, 1
	if req.IntentId == "" {
		req.IntentId = sdk.NewID()
	}
	return h.Client.CallTool(ctx, req)
}

// User is a user message of text.
func User(text string) *gatewayv0.Message {
	return &gatewayv0.Message{Role: gatewayv0.Role_ROLE_USER, Parts: []*gatewayv0.Part{{Part: &gatewayv0.Part_Text{Text: text}}}}
}

// Assistant is the assistant message an answer makes.
func Assistant(a *Answer) *gatewayv0.Message {
	m := &gatewayv0.Message{Role: gatewayv0.Role_ROLE_ASSISTANT}
	if a.Text != "" {
		m.Parts = append(m.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_Text{Text: a.Text}})
	}
	for _, c := range a.ToolCalls {
		m.Parts = append(m.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_ToolCall{ToolCall: c}})
	}
	return m
}

// Results is a user message of tool results.
func Results(results ...*gatewayv0.ToolResult) *gatewayv0.Message {
	m := &gatewayv0.Message{Role: gatewayv0.Role_ROLE_USER}
	for _, r := range results {
		m.Parts = append(m.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_ToolResult{ToolResult: r}})
	}
	return m
}
