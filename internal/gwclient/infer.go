package gwclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Inference is the answer to a model call.
type Inference struct {
	// Message is the model's answer: its text and tool calls, in the order
	// the model produced them.
	Message *gatewayv0.Message
	Done    *gatewayv0.InferenceDone
	// Attempts is how many times the call was made.
	Attempts int
}

var (
	errIncomplete = errors.New("the answer ended without being done")
	errIdle       = errors.New("no answer in time")
)

// retryable reports failures of a model call worth retrying.
func retryable(err error) bool {
	if errors.Is(err, errIncomplete) || errors.Is(err, errIdle) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.ResourceExhausted, codes.Aborted, codes.DeadlineExceeded, codes.NotFound:
		return true
	}
	return false
}

// Infer makes a model call. It retries transient failures with backoff,
// sending the same request, with the same intent ID, and discarding what
// an interrupted attempt received: a call is restarted, never continued.
// An unknown session is registered again first.
func (c *Client) Infer(ctx context.Context, req *gatewayv0.InferenceRequest, reregister Reregister) (*Inference, error) {
	b := backoff{min: c.cfg.BackoffMin, max: c.cfg.BackoffMax}
	for attempt := 1; ; attempt++ {
		msg, done, err := c.inferOnce(ctx, req)
		if err == nil {
			return &Inference{Message: msg, Done: done, Attempts: attempt}, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retryable(err) {
			return nil, fmt.Errorf("model call: %w", err)
		}
		if status.Code(err) == codes.NotFound && reregister != nil {
			if rerr := reregister(ctx); rerr != nil {
				c.log.Warn("gateway: registering again failed", "err", rerr)
			}
		}
		d := b.next()
		c.log.Warn("gateway: model call failed, retrying", "intent", req.GetIntentId(), "attempt", attempt, "err", err, "in", d.Round(time.Millisecond))
		if err := sleep(ctx, d); err != nil {
			return nil, err
		}
	}
}

func (c *Client) inferOnce(ctx context.Context, req *gatewayv0.InferenceRequest) (*gatewayv0.Message, *gatewayv0.InferenceDone, error) {
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	idle := time.AfterFunc(c.cfg.InferenceIdle, func() { cancel(errIdle) })
	defer idle.Stop()
	stream, err := c.gw.Infer(actx, req, grpc.WaitForReady(true))
	if err != nil {
		return nil, nil, cause(actx, err)
	}
	msg := &gatewayv0.Message{Role: gatewayv0.Role_ROLE_ASSISTANT}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil, nil, errIncomplete
		}
		if err != nil {
			return nil, nil, cause(actx, err)
		}
		idle.Reset(c.cfg.InferenceIdle)
		switch m := resp.Msg.(type) {
		case *gatewayv0.InferenceResponse_TextDelta:
			// Consecutive text deltas make one text part.
			if n := len(msg.Parts); n > 0 && msg.Parts[n-1].GetToolCall() == nil {
				msg.Parts[n-1].Part = &gatewayv0.Part_Text{Text: msg.Parts[n-1].GetText() + m.TextDelta}
			} else {
				msg.Parts = append(msg.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_Text{Text: m.TextDelta}})
			}
		case *gatewayv0.InferenceResponse_ToolCall:
			msg.Parts = append(msg.Parts, &gatewayv0.Part{Part: &gatewayv0.Part_ToolCall{ToolCall: m.ToolCall}})
		case *gatewayv0.InferenceResponse_Done:
			return msg, m.Done, nil
		}
	}
}

// cause prefers the reason the attempt was cancelled, such as being idle.
func cause(ctx context.Context, err error) error {
	if c := context.Cause(ctx); c != nil && !errors.Is(c, context.Canceled) {
		return c
	}
	return err
}
