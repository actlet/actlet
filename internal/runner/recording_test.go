package runner

import (
	"errors"
	"io"
	"os"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/journal"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// readRecording reads the events of a gateway recording.
func readRecording(t *testing.T, path string) []*gatewayv0.ActivityEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	r, err := journal.NewReader(f, st.Size(), journal.ReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var out []*gatewayv0.ActivityEvent
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range b.Records {
			ev := new(gatewayv0.ActivityEvent)
			if err := proto.Unmarshal(rec.Data, ev); err != nil {
				t.Fatal(err)
			}
			out = append(out, ev)
		}
	}
}
