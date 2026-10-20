package session

import (
	"slices"
	"testing"

	"github.com/actlet/actlet/internal/agent"
	"github.com/actlet/actlet/internal/journal"
)

// Recovery keeps the agent's records from its latest checkpoint on, with
// the parts of a packed one, which come just before it.
func TestRecoverKeepsCheckpointParts(t *testing.T) {
	for _, tc := range []struct {
		kinds, want []string
	}{
		{[]string{agent.KindCheckpoint, agent.KindInferenceOutcome, agent.KindCheckpointPart, agent.KindCheckpointPart, agent.KindCheckpoint, agent.KindToolIntent},
			[]string{agent.KindCheckpointPart, agent.KindCheckpointPart, agent.KindCheckpoint, agent.KindToolIntent}},
		// Parts of a checkpoint a crash cut short stay until the next one.
		{[]string{agent.KindCheckpoint, agent.KindCheckpointPart, agent.KindInferenceIntent, agent.KindCheckpoint},
			[]string{agent.KindCheckpoint}},
		{[]string{agent.KindCheckpoint, agent.KindInferenceOutcome, agent.KindCheckpointPart},
			[]string{agent.KindCheckpoint, agent.KindInferenceOutcome, agent.KindCheckpointPart}},
	} {
		r := &Recovered{}
		for i, k := range tc.kinds {
			if err := r.apply(journal.Record{Seq: uint64(i + 1), Kind: k}); err != nil {
				t.Fatal(err)
			}
		}
		var got []string
		for _, jr := range r.Agent {
			got = append(got, jr.Kind)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("from %v kept %v, want %v", tc.kinds, got, tc.want)
		}
	}
}
