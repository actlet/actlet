package sdk

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// FuzzWorkerMessages sends what a worker might, garbled: each part of the
// input, between separators, is a message on one of the gateway's calls,
// chosen by its first byte. The gateway must not crash, must keep serving,
// and must store a session's activity in order, without gaps or
// duplicates, whatever it is sent.
func FuzzWorkerMessages(f *testing.F) {
	seed := func(msgs ...[]byte) []byte { return bytes.Join(msgs, []byte(sep)) }
	part := func(kind byte, m proto.Message) []byte {
		b, _ := proto.Marshal(m)
		return append([]byte{kind}, b...)
	}
	f.Add(seed(
		part(0, &gatewayv0.ControlUp{Msg: &gatewayv0.ControlUp_Ack{Ack: &gatewayv0.CommandAck{CommandId: "x"}}}),
		part(1, &gatewayv0.ActivityUp{Msg: &gatewayv0.ActivityUp_Batch{Batch: &gatewayv0.ActivityBatch{Events: []*gatewayv0.ActivityEvent{
			{Id: "1.1", Epoch: 1, Seq: 1}, {Id: "1.3", Epoch: 1, Seq: 3}, {Id: "1.2", Epoch: 1, Seq: 2},
		}}}}),
		part(2, &gatewayv0.InferenceRequest{IntentId: "i", Model: "m", Messages: []*gatewayv0.Message{{Role: gatewayv0.Role_ROLE_USER}}}),
		part(3, &gatewayv0.ToolCallRequest{Tool: "decide", Input: "{"}),
		part(4, &gatewayv0.RegisterRequest{ProtocolVersion: "v9", SessionId: "../x"}),
	))
	f.Add(seed(part(1, &gatewayv0.ActivityUp{Msg: &gatewayv0.ActivityUp_Hello{Hello: &gatewayv0.ActivityHello{Epoch: 7}}})))
	f.Add([]byte{0, 0xff, 0x01})

	store := NewMemoryStore()
	h := newHarness(f, Options{
		Store: store,
		OnInference: func(_ context.Context, call *InferenceCall) error {
			if err := call.Text("ok"); err != nil {
				return err
			}
			return call.Done(&gatewayv0.InferenceDone{StopReason: gatewayv0.StopReason_STOP_REASON_END_TURN})
		},
	})
	n := 0
	f.Fuzz(func(t *testing.T, data []byte) {
		n++
		ctx, cancel := context.WithTimeout(authed(context.Background()), 2*time.Second)
		defer cancel()
		resp, err := h.client.Register(ctx, &gatewayv0.RegisterRequest{ProtocolVersion: ProtocolVersion, WorkerId: "fuzz", Epoch: 1})
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		sid := resp.GetSessionId()
		control, err := h.client.Control(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_ = control.Send(&gatewayv0.ControlUp{Msg: &gatewayv0.ControlUp_Hello{Hello: &gatewayv0.ControlHello{SessionId: sid, Epoch: 1}}})
		activity, err := h.client.PublishActivity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_ = activity.Send(&gatewayv0.ActivityUp{Msg: &gatewayv0.ActivityUp_Hello{Hello: &gatewayv0.ActivityHello{SessionId: sid, Epoch: 1}}})
		go func() {
			for {
				if _, err := activity.Recv(); err != nil {
					return
				}
			}
		}()
		for _, p := range bytes.Split(data, []byte(sep)) {
			if len(p) == 0 {
				continue
			}
			kind, body := p[0]%5, p[1:]
			switch kind {
			case 0:
				m := new(gatewayv0.ControlUp)
				if proto.Unmarshal(body, m) == nil {
					_ = control.Send(m)
				}
			case 1:
				m := new(gatewayv0.ActivityUp)
				if proto.Unmarshal(body, m) == nil {
					_ = activity.Send(m)
				}
			case 2:
				m := new(gatewayv0.InferenceRequest)
				if proto.Unmarshal(body, m) == nil {
					m.SessionId = sid
					if s, err := h.client.Infer(ctx, m); err == nil {
						for {
							if _, err := s.Recv(); err != nil {
								break
							}
						}
					}
				}
			case 3:
				m := new(gatewayv0.ToolCallRequest)
				if proto.Unmarshal(body, m) == nil {
					m.SessionId = sid
					_, _ = h.client.CallTool(ctx, m)
				}
			case 4:
				m := new(gatewayv0.RegisterRequest)
				if proto.Unmarshal(body, m) == nil {
					_, _ = h.client.Register(ctx, m)
				}
			}
		}
		_ = control.CloseSend()
		_ = activity.CloseSend()
		cancel()

		for i, ev := range store.Events(sid) {
			if want := EventID(ev.GetEpoch(), ev.GetSeq()); ev.GetEpoch() != 1 || ev.GetSeq() != uint64(i+1) || ev.GetId() != want {
				t.Fatalf("stored event %d is %s (epoch %d, seq %d)", i, ev.GetId(), ev.GetEpoch(), ev.GetSeq())
			}
		}
		// Still serving.
		if _, err := h.client.Register(authed(context.Background()), &gatewayv0.RegisterRequest{ProtocolVersion: ProtocolVersion, WorkerId: fmt.Sprint(n), Epoch: 1}); err != nil {
			t.Fatalf("Register after the messages: %v", err)
		}
	})
}

// sep separates the messages of a fuzz input.
const sep = "\x00SEP\x00"
