package sdk

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// DefaultInferenceCache is the default number of answered model calls a
// server keeps to answer repeated intent IDs.
const DefaultInferenceCache = 256

// InferenceCall is one model call from a worker's agent, for
// Options.OnInference to answer: stream the answer with Text and ToolCall,
// in the order the model produces it, and end it with Done.
type InferenceCall struct {
	// Session is the session making the call.
	Session *Session
	// Request is the call. The model is an alias from the profile's
	// catalogue; resolve it to a provider and model of your choosing.
	Request *gatewayv0.InferenceRequest

	send func(*gatewayv0.InferenceResponse) error
	done bool
}

// Text streams a piece of the model's text.
func (c *InferenceCall) Text(delta string) error {
	return c.send(&gatewayv0.InferenceResponse{Msg: &gatewayv0.InferenceResponse_TextDelta{TextDelta: delta}})
}

// ToolCall streams a complete tool call. Its ID must be unique within the
// conversation and its input a JSON object.
func (c *InferenceCall) ToolCall(id, name, input string) error {
	return c.send(&gatewayv0.InferenceResponse{Msg: &gatewayv0.InferenceResponse_ToolCall{ToolCall: &gatewayv0.ToolCall{Id: id, Name: name, Input: input}}})
}

// Done ends the answer: why the model stopped, what it used, and the
// continuation the worker passes back with its next call.
func (c *InferenceCall) Done(d *gatewayv0.InferenceDone) error {
	if err := c.send(&gatewayv0.InferenceResponse{Msg: &gatewayv0.InferenceResponse_Done{Done: d}}); err != nil {
		return err
	}
	c.done = true
	return nil
}

type inferenceKey struct {
	session, intent string
}

// inference is a model call by intent ID: running, or answered.
type inference struct {
	done      chan struct{}
	ok        bool
	responses []*gatewayv0.InferenceResponse
}

// claimInference returns the call with the key. If there is none, it
// creates it and reports that the caller must run it.
func (s *Server) claimInference(k inferenceKey) (*inference, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inf, ok := s.inferences[k]; ok {
		return inf, false
	}
	inf := &inference{done: make(chan struct{})}
	s.inferences[k] = inf
	return inf, true
}

// finishInference records how a call ended. A failed call is forgotten,
// so that a retry runs it again; answered calls are kept, up to the cache
// size.
func (s *Server) finishInference(k inferenceKey, inf *inference, ok bool) {
	s.mu.Lock()
	inf.ok = ok
	if !ok {
		delete(s.inferences, k)
	} else {
		s.answered = append(s.answered, k)
		for len(s.answered) > s.opts.InferenceCache {
			delete(s.inferences, s.answered[0])
			s.answered = s.answered[1:]
		}
	}
	s.mu.Unlock()
	close(inf.done)
}

// Infer implements the gateway's Infer call. A call with an intent ID the
// server has answered gets the same answer again, marked as replayed; one
// with the ID of a call still running waits for it. Draining lets running
// calls finish.
func (s *Server) Infer(req *gatewayv0.InferenceRequest, stream gatewayv0.Gateway_InferServer) error {
	ctx := stream.Context()
	if err := s.authenticate(ctx); err != nil {
		return err
	}
	if s.draining() {
		return errDraining
	}
	if s.opts.OnInference == nil {
		return status.Error(codes.Unimplemented, "this gateway serves no models")
	}
	switch {
	case req.GetIntentId() == "" || len(req.GetIntentId()) > 128:
		return status.Error(codes.InvalidArgument, "an intent ID of 1 to 128 bytes is required")
	case req.GetEpoch() == 0:
		return status.Error(codes.InvalidArgument, "epoch must be at least 1")
	}
	sess := s.Session(req.GetSessionId())
	if sess == nil {
		return status.Errorf(codes.NotFound, "unknown session %q: register first", req.GetSessionId())
	}
	s.streams.Add(1)
	defer s.streams.Done()

	k := inferenceKey{req.GetSessionId(), req.GetIntentId()}
	for {
		inf, run := s.claimInference(k)
		if run {
			return s.runInference(ctx, k, inf, sess, req, stream)
		}
		select {
		case <-inf.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if inf.ok {
			s.log.Info("inference: answering a repeated call", "session", k.session, "intent", k.intent)
			return replayInference(inf.responses, stream)
		}
		// The earlier call failed: make this one.
	}
}

func (s *Server) runInference(ctx context.Context, k inferenceKey, inf *inference, sess *Session, req *gatewayv0.InferenceRequest, stream gatewayv0.Gateway_InferServer) (err error) {
	call := &InferenceCall{Session: sess, Request: req}
	call.send = func(r *gatewayv0.InferenceResponse) error {
		if call.done {
			return errors.New("sdk: the inference call is already done")
		}
		inf.responses = append(inf.responses, r)
		return stream.Send(r)
	}
	defer func() { s.finishInference(k, inf, err == nil) }()
	if err := s.opts.OnInference(ctx, call); err != nil {
		if _, ok := status.FromError(err); !ok && ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if !call.done {
		return status.Error(codes.Internal, "the gateway ended the call without an answer")
	}
	return nil
}

func replayInference(responses []*gatewayv0.InferenceResponse, stream gatewayv0.Gateway_InferServer) error {
	for _, r := range responses {
		if d := r.GetDone(); d != nil {
			d = proto.Clone(d).(*gatewayv0.InferenceDone)
			d.Replayed = true
			r = &gatewayv0.InferenceResponse{Msg: &gatewayv0.InferenceResponse_Done{Done: d}}
		}
		if err := stream.Send(r); err != nil {
			return err
		}
	}
	return nil
}
