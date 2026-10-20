package agent

import (
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/agent/agentpb"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// noise returns n bytes of text that compresses poorly, the same each
// time for a seed.
func noise(seed uint64, n int) string {
	r := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n*3/4+3)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return base64.StdEncoding.EncodeToString(b)[:n]
}

func TestPackCheckpoint(t *testing.T) {
	small := &agentpb.Checkpoint{Spawned: 1, Workers: []*agentpb.WorkerState{{WorkerId: RootWorker}}}
	recs, err := pack(small)
	if err != nil || len(recs) != 1 || !proto.Equal(recs[0], small) {
		t.Fatalf("a small checkpoint: %v, %v", recs, err)
	}

	big := &agentpb.Checkpoint{Spawned: 2, SteersDelivered: []string{"c1"}, Workers: []*agentpb.WorkerState{{WorkerId: RootWorker}}}
	for i := range 5 {
		big.Workers[0].Messages = append(big.Workers[0].Messages, userText(noise(uint64(i), 2<<20)))
	}
	recs, err = pack(big)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) < 3 {
		t.Fatalf("%d records for %d bytes", len(recs), proto.Size(big))
	}
	head := recs[len(recs)-1].(*agentpb.Checkpoint)
	if head.GetPacked().GetParts() != uint32(len(recs)-1) || len(head.GetWorkers()) != 0 {
		t.Fatalf("head %v", head.GetPacked())
	}
	for _, m := range recs {
		if n := proto.Size(m); n > maxCheckpointRecord+1024 {
			t.Errorf("a %s record of %d bytes", kindOf(m), n)
		}
	}
	replay := func(recs ...proto.Message) (*stateSet, error) {
		s := newStateSet()
		for _, m := range recs {
			if err := s.apply(m); err != nil {
				return s, err
			}
		}
		return s, nil
	}
	s, err := replay(recs...)
	if err != nil {
		t.Fatal(err)
	}
	want := newStateSet()
	want.restore(big)
	if !proto.Equal(s.checkpoint(), want.checkpoint()) {
		t.Error("the unpacked checkpoint differs")
	}

	// Parts of a checkpoint that was never finished don't matter.
	orphans := []proto.Message{recs[0], recs[1]}
	other := &agentpb.CheckpointPart{Sha256: []byte("other"), Index: 3, Data: []byte("x")}
	if _, err := replay(append(append(orphans, other), recs...)...); err != nil {
		t.Errorf("after orphaned parts: %v", err)
	}
	// A packed checkpoint without all its parts fails recovery.
	for _, tc := range [][]proto.Message{
		recs[1:],
		append(append([]proto.Message{}, recs[:1]...), recs[2:]...),
		{head},
	} {
		if _, err := replay(tc...); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Errorf("%d records: %v", len(tc), err)
		}
	}
	bad := proto.Clone(recs[1]).(*agentpb.CheckpointPart)
	bad.Data = append([]byte(nil), bad.Data...)
	bad.Data[0] ^= 1
	if _, err := replay(append([]proto.Message{recs[0], bad}, recs[2:]...)...); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("a damaged part: %v", err)
	}

	defer func(n int) { maxCheckpoint = n }(maxCheckpoint)
	maxCheckpoint = 8 << 20
	if _, err := pack(big); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("over the limit: %v", err)
	}
}

// A conversation of over 10 MiB checkpoints in parts and recovers from
// them, and a crash while the parts are written recovers from the
// checkpoint before.
func TestLargeConversationRecovers(t *testing.T) {
	const turns, size = 5, 2 << 20
	script := func() []turn {
		var ts []turn
		for i := range turns {
			ts = append(ts, turn{text: noise(uint64(i), size), calls: [][2]string{{"glob", `{"pattern":"*.none"}`}}})
		}
		return append(ts, turn{text: "done", check: func(req *gatewayv0.InferenceRequest) error {
			var n, total int
			for _, m := range req.GetMessages() {
				for _, p := range m.GetParts() {
					total += len(p.GetText())
				}
				if m.GetRole() == gatewayv0.Role_ROLE_ASSISTANT {
					if txt := messageText(m); txt != noise(uint64(n), size) {
						return fmt.Errorf("answer %d came back changed: %d bytes", n+1, len(txt))
					}
					n++
				}
			}
			if n != turns || total < 10<<20 {
				return fmt.Errorf("%d answers, %d bytes of text", n, total)
			}
			return nil
		}})
	}
	for _, tc := range []struct {
		name  string
		crash func(s *sandbox) func(string, proto.Message) bool
	}{
		{"after the checkpoint", func(s *sandbox) func(string, proto.Message) bool {
			return func(kind string, _ proto.Message) bool {
				return kind == KindInferenceIntent && len(s.model.callsOf("root")) == turns
			}
		}},
		{"between its parts", func(s *sandbox) func(string, proto.Message) bool {
			return func(kind string, m proto.Message) bool {
				return kind == KindCheckpointPart && m.(*agentpb.CheckpointPart).GetIndex() == 1 && len(s.model.callsOf("root")) == turns
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSandbox(t, script()...)
			s.crashBefore = tc.crash(s)
			s.mustCrash()
			since := s.sinceCheckpoint()
			if since[0].Kind != KindCheckpointPart {
				t.Fatalf("recovery starts at a %s record", since[0].Kind)
			}
			s.mustFinish()
			if n := s.count(KindCheckpointPart); n < 3 {
				t.Errorf("%d checkpoint parts", n)
			}
		})
	}
}
