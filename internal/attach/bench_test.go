package attach

import (
	"fmt"
	"sync"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// BenchmarkHubFanout publishes events to viewers that read as fast as
// they can: the time per event published, with its fan-out to every
// viewer.
func BenchmarkHubFanout(b *testing.B) {
	for _, viewers := range []int{1, 8, 32} {
		b.Run(fmt.Sprintf("viewers=%d", viewers), func(b *testing.B) {
			h := NewHub(HubOptions{MaxClients: viewers})
			var wg sync.WaitGroup
			var skipped uint64
			var mu sync.Mutex
			for range viewers {
				sub, err := h.Subscribe(0)
				if err != nil {
					b.Fatal(err)
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					for m := range sub.C() {
						if s := m.GetSkipped(); s != nil {
							mu.Lock()
							skipped += s.GetCount()
							mu.Unlock()
						}
					}
				}()
			}
			ev := &gatewayv0.ActivityEvent{Id: "1.1", Epoch: 1, Seq: 1, Time: timestamppb.Now(), Worker: "root",
				Body: &gatewayv0.ActivityEvent_Text{Text: &gatewayv0.Text{Text: "a line of the agent's output, about this long"}}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				ev.Seq = uint64(i + 1)
				h.Publish(ev)
			}
			b.StopTimer()
			h.Close()
			wg.Wait()
			b.ReportMetric(float64(skipped)/float64(b.N*viewers), "skipped/delivery")
		})
	}
}
