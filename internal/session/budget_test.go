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

func updateCmd(id string, u *gatewayv0.ProfileUpdate) *gatewayv0.Command {
	return &gatewayv0.Command{Id: id, Origin: gatewayv0.Origin_ORIGIN_ORCHESTRATOR, Action: &gatewayv0.Command_ProfileUpdate{ProfileUpdate: u}}
}

// A profile update changes only allowlisted fields, is recorded before it
// takes effect, and changes the limits, budgets and viewers' commands.
func TestProfileUpdate(t *testing.T) {
	f := folder(t)
	cfg := withBudget(&gatewayv0.Budget{Kind: budget.Time, Limit: 600, OnExhaust: gatewayv0.BudgetAction_BUDGET_ACTION_KILL})
	cfg.MaxWorkers, cfg.MaxToolConcurrency, cfg.MaxSubagents = 2, 2, 3
	var told []*sessionpb.Config
	r := start(t, f, `{"steps":20,"step":"200ms"}`, cfg)
	r.s.p.OnConfig = func(c *sessionpb.Config) { told = append(told, c) }
	r.s.p.MaxWorkers = 4

	for _, tc := range []struct {
		u    *gatewayv0.ProfileUpdate
		want string
	}{
		{&gatewayv0.ProfileUpdate{Fields: []string{"instructions"}, Profile: &gatewayv0.Profile{Instructions: "be evil"}}, "these fields can't change during a session: instructions"},
		{&gatewayv0.ProfileUpdate{Fields: []string{"limits.max_subagent_depth", "budgets"}}, "limits.max_subagent_depth"},
		{&gatewayv0.ProfileUpdate{}, "changes nothing"},
		{&gatewayv0.ProfileUpdate{Fields: []string{"budgets"}, Profile: &gatewayv0.Profile{Budgets: []*gatewayv0.Budget{{Kind: budget.Money, Limit: 1}}}}, "needs a currency"},
		{&gatewayv0.ProfileUpdate{ResetBudgets: []budget.Kind{budget.Disk}}, "can't be reset"},
	} {
		ack := r.apply(t, updateCmd("bad-"+tc.want, tc.u))
		if ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_REJECTED || !strings.Contains(ack.GetMessage(), tc.want) {
			t.Errorf("%v: %v", tc.u, ack)
		}
	}

	ack := r.apply(t, updateCmd("u1", &gatewayv0.ProfileUpdate{
		Fields: []string{"limits.max_workers", "limits.max_subagents", "budgets", "local_attach"},
		Profile: &gatewayv0.Profile{
			Limits:      &gatewayv0.Limits{MaxWorkers: 9, MaxSubagents: 5},
			Budgets:     []*gatewayv0.Budget{{Kind: budget.Time, Limit: 1200, OnExhaust: gatewayv0.BudgetAction_BUDGET_ACTION_KILL}},
			LocalAttach: &gatewayv0.LocalAttach{Commands: []string{"steer", "pause"}},
		},
		ResetBudgets: []budget.Kind{budget.Time},
	}))
	if ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED || !strings.Contains(ack.GetMessage(), "reset time") {
		t.Fatalf("update: %v", ack)
	}
	if w, tools, subs := r.s.limits.Get(); w != 4 || tools != 2 || subs != 5 {
		t.Errorf("limits %d %d %d: the worker ceiling is 4", w, tools, subs)
	}
	if st := r.s.budget.Status().GetBudgets(); len(st) != 1 || st[0].GetLimit() != 1200 || st[0].GetUsed() > 1 {
		t.Errorf("budgets after the update and reset: %v", st)
	}
	if len(told) != 1 || strings.Join(told[0].GetLocalCommands(), ",") != "steer,pause" {
		t.Errorf("told %v", told)
	}
	if got := budgetReasons(r.rec); !strings.Contains(got, "update") || !strings.Contains(got, "reset") {
		t.Errorf("budget statuses %q", got)
	}
	r.apply(t, stopCmd("stop", time.Second, true))
	outcome(t, r)

	rec, err := Recover(f)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Config.GetMaxWorkers() != 4 || rec.Budget.Offsets[budget.Time] <= 0 || rec.Commands["bad-changes nothing"].GetAccepted() {
		t.Errorf("recovered config %v, offsets %v", rec.Config, rec.Budget.Offsets)
	}
}
