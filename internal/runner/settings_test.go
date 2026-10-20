package runner

import (
	"strings"
	"testing"

	"github.com/actlet/actlet/internal/agent"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// The compaction policy gets its defaults, the compaction role's model,
// and the aliases' context windows; a strategy Actlet doesn't know is
// refused.
func TestCompactionSettings(t *testing.T) {
	models := &gatewayv0.ModelCatalog{
		Aliases: []*gatewayv0.ModelAlias{{Alias: "large", ContextWindow: 200000}, {Alias: "fast"}},
		Roles:   map[string]string{"main": "large", "compaction": "fast"},
	}
	for _, tc := range []struct {
		policy *gatewayv0.CompactionPolicy
		want   agent.CompactionPolicy
		err    string
	}{
		{nil, agent.CompactionPolicy{ThresholdPercent: 80, Model: "fast", KeepTurns: 4}, ""},
		{&gatewayv0.CompactionPolicy{ThresholdPercent: 60, Model: "large", KeepTurns: 2},
			agent.CompactionPolicy{ThresholdPercent: 60, Model: "large", KeepTurns: 2}, ""},
		{&gatewayv0.CompactionPolicy{Strategy: "off"}, agent.CompactionPolicy{Off: true, ThresholdPercent: 80, Model: "fast", KeepTurns: 4}, ""},
		{&gatewayv0.CompactionPolicy{Strategy: "truncate"}, agent.CompactionPolicy{}, `strategy "truncate"`},
		{&gatewayv0.CompactionPolicy{ThresholdPercent: 120}, agent.CompactionPolicy{}, "over 100%"},
	} {
		p := &gatewayv0.Profile{Models: models, Compaction: tc.policy}
		effective := effectiveConfig(Config{}, p)
		s, err := agentSettings(Config{}, p, effective)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%v: %v, want %q", tc.policy, err, tc.err)
			}
			continue
		}
		if err != nil || s.Compaction != tc.want || len(s.ContextWindows) != 1 || s.ContextWindows["large"] != 200000 {
			t.Errorf("%v: %+v, windows %v, %v", tc.policy, s.Compaction, s.ContextWindows, err)
		}
		if effective.GetCompaction().GetStrategy() == "" {
			t.Errorf("%v: effective %v", tc.policy, effective.GetCompaction())
		}
	}
}
