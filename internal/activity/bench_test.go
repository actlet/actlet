package activity

import (
	"context"
	"log/slog"
	"testing"
	"time"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// BenchmarkThroughput emits events while a sender reads and acknowledges
// them, as the gateway connection does: the time per event, in memory,
// and with a small queue, through the spill file.
func BenchmarkThroughput(b *testing.B) {
	for _, tc := range []struct {
		name  string
		queue int
	}{{"memory", 1 << 16}, {"spilling", 16}} {
		b.Run(tc.name, func(b *testing.B) {
			p := New(Options{SessionID: "s1", Epoch: 1, Dir: b.TempDir(), QueueLen: tc.queue, SpillMax: 1 << 30,
				Logger: slog.New(slog.DiscardHandler)})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			want := uint64(b.N)
			go func() {
				defer close(done)
				var after uint64
				for after < want {
					evs, err := p.Next(ctx, after, 256)
					if err != nil {
						return
					}
					after = evs[len(evs)-1].Seq
					p.Ack(after)
				}
			}()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				p.Emit(&Event{Body: &gatewayv0.ActivityEvent_Text{Text: &gatewayv0.Text{Text: "a line of the agent's output, about this long"}}})
			}
			select {
			case <-done:
			case <-time.After(time.Minute):
				b.Fatal("the sender didn't catch up")
			}
			b.StopTimer()
			cctx, ccancel := context.WithTimeout(context.Background(), time.Second)
			defer ccancel()
			_ = p.Close(cctx)
		})
	}
}
