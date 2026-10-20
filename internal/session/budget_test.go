package session

import (
	"strings"
	"testing"
	"time"

	"github.com/actlet/actlet/internal/budget"
	"github.com/actlet/actlet/internal/session/sessionpb"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

func withBudget(b *gatewayv0.Budget) *sessionpb.Config {
	cfg := testConfig()
	cfg.Budgets = []*gatewayv0.Budget{b}
	return cfg
}

func budgetReasons(r *recorder) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, ev := range r.evs {
		if st := ev.GetBudgetStatus(); st != nil {
			out = append(out, st.GetReason())
		}
	}
	return strings.Join(out, " ")
}

// A time budget alerts at its threshold and stops the session at its
// limit; both are journaled.
func TestTimeBudgetStops(t *testing.T) {
	f := folder(t)
	r := start(t, f, `{"steps":20,"step":"500ms"}`, withBudget(&gatewayv0.Budget{
		Kind: budget.Time, Limit: 2, AlertAtPercent: 50, OnExhaust: gatewayv0.BudgetAction_BUDGET_ACTION_KILL,
	}))
	o := outcome(t, r)
	if o.State != gatewayv0.SessionState_SESSION_STATE_STOPPED || !strings.Contains(o.Reason, "time budget used up") {
		t.Fatalf("outcome %+v", o)
	}
	if got := budgetReasons(r.rec); got != "start alert exhausted" {
		t.Errorf("budget statuses %q", got)
	}
	rec, err := Recover(f)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Budget.Alerted[budget.Time] != 2 || rec.Budget.ActiveTime < 2*time.Second || len(rec.Steers) != 1 || !rec.Steers[0].System {
		t.Errorf("recovered %+v, steers %+v", rec.Budget, rec.Steers)
	}
}

// A budget that waits by pausing pauses the session; a run with the same
// budget pauses again at once, and one with a raised budget completes,
// without alerting again.
func TestBudgetPausesUntilRaised(t *testing.T) {
	f := folder(t)
	b := &gatewayv0.Budget{Kind: budget.Time, Limit: 3, AlertAtPercent: 50,
		OnExhaust: gatewayv0.BudgetAction_BUDGET_ACTION_WAIT, WaitMode: gatewayv0.BudgetWaitMode_BUDGET_WAIT_MODE_PAUSE}
	r := start(t, f, `{"steps":20,"step":"300ms"}`, withBudget(b))
	if o := outcome(t, r); o.State != gatewayv0.SessionState_SESSION_STATE_PAUSED || !strings.Contains(o.Reason, "pausing until it is raised") {
		t.Fatalf("first run: %+v", o)
	}
	r = start(t, f, `{"steps":20,"step":"300ms"}`, withBudget(b))
	if o := outcome(t, r); o.State != gatewayv0.SessionState_SESSION_STATE_PAUSED {
		t.Fatalf("second run: %+v", o)
	}
	b.Limit = 60
	r = start(t, f, `{"steps":20,"step":"300ms"}`, withBudget(b))
	if o := outcome(t, r); o.State != gatewayv0.SessionState_SESSION_STATE_COMPLETED {
		t.Fatalf("third run: %+v", o)
	}
	// The alert at the first limit came once; at 50% of 60s, none came.
	rec, _ := Recover(f)
	notices := 0
	for _, s := range rec.Steers {
		if s.CommandID == "budget-time-3" {
			notices++
		}
	}
	if notices != 1 || len(rec.Steers) != 1 {
		t.Errorf("%d notices at the first limit, %d in all", notices, len(rec.Steers))
	}
}
