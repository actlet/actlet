package sdk

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// DecideRequest is a decide call from a worker's agent: a question for a
// person or a system that decides, for Options.OnDecide to answer.
type DecideRequest struct {
	Session *Session
	// IntentID identifies the call. A worker that asks again, such as
	// after a pause or a crash, sends the same ID; answer it the same
	// way. The server does this for calls it still remembers.
	IntentID string
	// Worker is the worker asking: "root", "root.1", and so on.
	Worker   string
	Question string
	Options  []string
	Context  string
}

// Decision answers a decide call.
type Decision struct {
	Decision string `json:"decision"`
	// Reason explains it, for the agent.
	Reason string `json:"reason,omitempty"`
	// DecidedBy names who or what decided.
	DecidedBy string `json:"decided_by,omitempty"`
	// Usage is what deciding used, such as a model's tokens and cost,
	// which count towards the session's budgets.
	Usage *gatewayv0.Usage `json:"-"`
}

// ToolCall is a call of a bridged tool from a worker's agent, for
// Options.OnTool to answer.
type ToolCall struct {
	Session *Session
	Request *gatewayv0.ToolCallRequest
}

type toolKey struct {
	session, intent string
}

// toolResult is a bridged tool call by intent ID: running, or answered.
type toolResult struct {
	done chan struct{}
	resp *gatewayv0.ToolCallResponse
}

// CallTool implements the gateway's CallTool call. A call with an intent
// ID the server has answered gets the same answer again, marked as
// replayed; one with the ID of a call still running waits for it.
func (s *Server) CallTool(ctx context.Context, req *gatewayv0.ToolCallRequest) (*gatewayv0.ToolCallResponse, error) {
	if err := s.authenticate(ctx); err != nil {
		return nil, err
	}
	if s.draining() {
		return nil, errDraining
	}
	switch {
	case req.GetIntentId() == "" || len(req.GetIntentId()) > 128:
		return nil, status.Error(codes.InvalidArgument, "an intent ID of 1 to 128 bytes is required")
	case req.GetEpoch() == 0:
		return nil, status.Error(codes.InvalidArgument, "epoch must be at least 1")
	case req.GetTool() == "":
		return nil, status.Error(codes.InvalidArgument, "no tool")
	}
	sess := s.Session(req.GetSessionId())
	if sess == nil {
		return nil, status.Errorf(codes.NotFound, "unknown session %q: register first", req.GetSessionId())
	}
	if req.GetTool() != "decide" || s.opts.OnDecide == nil {
		if s.opts.OnTool == nil {
			return nil, status.Errorf(codes.Unimplemented, "this gateway provides no %s tool", req.GetTool())
		}
	}
	s.streams.Add(1)
	defer s.streams.Done()

	k := toolKey{req.GetSessionId(), req.GetIntentId()}
	for {
		r, run := s.claimTool(k)
		if run {
			resp, err := s.runTool(ctx, sess, req)
			s.finishTool(k, r, resp)
			return resp, err
		}
		select {
		case <-r.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if r.resp != nil {
			s.log.Info("tool: answering a repeated call", "session", k.session, "tool", req.GetTool(), "intent", k.intent)
			resp := proto.Clone(r.resp).(*gatewayv0.ToolCallResponse)
			resp.Replayed = true
			return resp, nil
		}
		// The earlier call failed: make this one.
	}
}

func (s *Server) runTool(ctx context.Context, sess *Session, req *gatewayv0.ToolCallRequest) (*gatewayv0.ToolCallResponse, error) {
	if req.GetTool() == "decide" && s.opts.OnDecide != nil {
		var in struct {
			Question string   `json:"question"`
			Options  []string `json:"options"`
			Context  string   `json:"context"`
		}
		if err := json.Unmarshal([]byte(req.GetInput()), &in); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "decide input: %v", err)
		}
		d, err := s.opts.OnDecide(ctx, &DecideRequest{
			Session: sess, IntentID: req.GetIntentId(), Worker: req.GetWorker(),
			Question: in.Question, Options: in.Options, Context: in.Context,
		})
		if err != nil {
			return nil, toolError(ctx, err)
		}
		if d == nil {
			return nil, status.Error(codes.Internal, "OnDecide returned no decision")
		}
		out, err := json.Marshal(d)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "decision: %v", err)
		}
		return &gatewayv0.ToolCallResponse{Output: string(out), Usage: d.Usage}, nil
	}
	resp, err := s.opts.OnTool(ctx, &ToolCall{Session: sess, Request: req})
	if err != nil {
		return nil, toolError(ctx, err)
	}
	if resp == nil {
		return nil, status.Error(codes.Internal, "OnTool returned no response")
	}
	return resp, nil
}

// toolError returns a hook's error as a gRPC status.
func toolError(ctx context.Context, err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.Unknown, err.Error())
}

// claimTool returns the call with the key. If there is none, it creates
// it and reports that the caller must run it.
func (s *Server) claimTool(k toolKey) (*toolResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.toolCalls[k]; ok {
		return r, false
	}
	r := &toolResult{done: make(chan struct{})}
	s.toolCalls[k] = r
	return r, true
}

// finishTool records how a call ended. A failed call is forgotten, so
// that a retry runs it again; answered calls are kept, up to the cache
// size.
func (s *Server) finishTool(k toolKey, r *toolResult, resp *gatewayv0.ToolCallResponse) {
	s.mu.Lock()
	r.resp = resp
	if resp == nil {
		delete(s.toolCalls, k)
	} else {
		s.toolsAnswered = append(s.toolsAnswered, k)
		for len(s.toolsAnswered) > s.opts.InferenceCache {
			delete(s.toolCalls, s.toolsAnswered[0])
			s.toolsAnswered = s.toolsAnswered[1:]
		}
	}
	s.mu.Unlock()
	close(r.done)
}
