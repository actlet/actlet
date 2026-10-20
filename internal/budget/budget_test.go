package budget

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/session/sessionpb"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// probe records what a tracker does, on a clock the test moves.
type probe struct {
	mu       sync.Mutex
	now      time.Time
	disk     int64
	recorded []*sessionpb.BudgetEvent
	status   []*gatewayv0.BudgetStatus
	notices  map[string]string
	acted    []string
}

func newProbe() *probe {
	return &probe{now: time.Unix(1700000000, 0), notices: map[string]string{}}
}

func (p *probe) hooks() Hooks {
	act := func(what string) func(string) {
		return func(reason string) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.acted = append(p.acted, what+": "+reason)
		}
	}
	return Hooks{
		Record: func(ev *sessionpb.BudgetEvent) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.recorded = append(p.recorded, ev)
			return nil
		},
		Emit: func(st *gatewayv0.BudgetStatus) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.status = append(p.status, st)
		},
		Notify: func(id, text string) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.notices[id] = text
		},
		Stop: act("stop"), Pause: act("pause"), Idle: act("idle"),
		Disk: func() int64 {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.disk
		},
		Now: func() time.Time {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.now
		},
		Logger: slog.New(slog.DiscardHandler),
	}
}

func (p *probe) advance(d time.Duration) {
	p.mu.Lock()
	p.now = p.now.Add(d)
	p.mu.Unlock()
}

func (p *probe) events() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, ev := range p.recorded {
		out = append(out, Name(ev.GetKind())+" "+ev.GetEvent())
	}
	return strings.Join(out, ", ")
}

func (p *probe) actions() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.acted, "; ")
}

func (p *probe) lastStatus() *gatewayv0.BudgetStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.status) == 0 {
		return nil
	}
	return p.status[len(p.status)-1]
}

// Active time counts earlier runs; the alert is given once, and the limit
// stops the session once.
func TestTimeAlertAndKill(t *testing.T) {
	p := newProbe()
	tr := New([]*gatewayv0.Budget{{Kind: Time, Limit: 10, AlertAtPercent: 50, OnExhaust: gatewayv0.BudgetAction_BUDGET_ACTION_KILL}},
		Restored{ActiveTime: 3 * time.Second}, p.hooks())
	tr.check("start")
	if p.events() != "" || p.lastStatus().GetReason() != "start" || p.lastStatus().GetBudgets()[0].GetUsed() != 3 {
		t.Fatalf("start: events %q, status %v", p.events(), p.lastStatus())
	}
	p.advance(2 * time.Second)
	tr.check("")
	tr.check("")
	if p.events() != "time alert" || len(p.notices) != 1 || !strings.Contains(p.notices["budget-time-10"], "50% of the session's time budget (5s of 10s)") {
		t.Fatalf("alert: events %q, notices %v", p.events(), p.notices)
	}
	if !strings.Contains(p.notices["budget-time-10"], "the session will be stopped") || p.lastStatus().GetBudgets()[0].GetState() != "alerted" {
		t.Errorf("notice %q, status %v", p.notices["budget-time-10"], p.lastStatus())
	}
	p.advance(5 * time.Second)
	tr.check("")
	tr.check("")
	if p.events() != "time alert, time exhausted" || p.actions() != "stop: time budget used up (10s of 10s)" {
		t.Errorf("exhausted: events %q, actions %q", p.events(), p.actions())
	}
	if st := p.lastStatus(); st.GetReason() != "exhausted" || st.GetBudgets()[0].GetState() != "exhausted" {
		t.Errorf("status %v", st)
	}
}

// An idling budget holds work back until it is raised; the alert is given
// again at the new limit.
func TestIdleUntilRaised(t *testing.T) {
	p := newProbe()
	money := &gatewayv0.Budget{Kind: Money, Limit: 1, Currency: "EUR", AlertAtPercent: 80,
		OnExhaust: gatewayv0.BudgetAction_BUDGET_ACTION_WAIT, WaitMode: gatewayv0.BudgetWaitMode_BUDGET_WAIT_MODE_IDLE}
	tr := New([]*gatewayv0.Budget{money}, Restored{}, p.hooks())
	tr.Spent(1000, 0.5, "EUR")
	if err := tr.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	tr.Spent(2000, 1.2, "EUR")
	if p.events() != "money exhausted" || !strings.Contains(p.actions(), "idle: money budget used up (1.20 EUR of 1.00 EUR)") {
		t.Fatalf("events %q, actions %q", p.events(), p.actions())
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := tr.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waited: %v", err)
	}
	if st := tr.Status(); st.GetBudgets()[0].GetState() != "waiting" {
		t.Errorf("status %v", st)
	}
	released := make(chan error, 1)
	go func() { released <- tr.Wait(t.Context()) }()
	raised := proto.Clone(money).(*gatewayv0.Budget)
	raised.Limit = 1.4
	tr.Update([]*gatewayv0.Budget{raised})
	select {
	case err := <-released:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("raising the budget didn't release the work")
	}
	if p.events() != "money exhausted, money alert" || p.notices["budget-money-1.4"] == "" {
		t.Errorf("after the update: events %q, notices %v", p.events(), p.notices)
	}
}

// A reset counts again from zero; a cost in another currency doesn't
// count; a token budget that runs out pauses.
func TestResetCurrencyAndPause(t *testing.T) {
	p := newProbe()
	tr := New([]*gatewayv0.Budget{
		{Kind: Money, Limit: 1, Currency: "EUR", OnExhaust: gatewayv0.BudgetAction_BUDGET_ACTION_WAIT, WaitMode: gatewayv0.BudgetWaitMode_BUDGET_WAIT_MODE_IDLE},
		{Kind: Tokens, Limit: 5000, OnExhaust: gatewayv0.BudgetAction_BUDGET_ACTION_WAIT, WaitMode: gatewayv0.BudgetWaitMode_BUDGET_WAIT_MODE_PAUSE},
	}, Restored{}, p.hooks())
	tr.Spent(100, 3, "USD")
	if p.events() != "" {
		t.Fatalf("a USD cost counted against a EUR budget: %q", p.events())
	}
	tr.Spent(200, 1, "EUR")
	resets := tr.Reset([]Kind{Money})
	if len(resets) != 1 || resets[0].GetOffset() != 1 {
		t.Fatalf("resets %v", resets)
	}
	tr.Apply(resets)
	if err := tr.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := tr.Status().GetBudgets()[0]; u.GetUsed() != 0 || u.GetState() != "ok" {
		t.Errorf("after the reset: %v", u)
	}
	tr.Spent(6000, 1.5, "EUR")
	// After the reset at 1 EUR, 1.5 EUR in all is 0.5 EUR of the budget.
	if p.events() != "money exhausted, tokens exhausted" || !strings.Contains(p.actions(), "pause: tokens budget used up") {
		t.Errorf("events %q, actions %q", p.events(), p.actions())
	}
}

// The disk budget samples the folder after tool calls, at most every
// couple of seconds.
func TestDisk(t *testing.T) {
	p := newProbe()
	tr := New([]*gatewayv0.Budget{{Kind: Disk, Limit: 1 << 20, OnExhaust: gatewayv0.BudgetAction_BUDGET_ACTION_KILL}}, Restored{}, p.hooks())
	tr.sampleDisk(true)
	p.mu.Lock()
	p.disk = 2 << 20
	p.mu.Unlock()
	tr.ToolDone()
	if p.events() != "" {
		t.Fatal("sampled again at once")
	}
	p.advance(3 * time.Second)
	tr.ToolDone()
	if p.events() != "disk exhausted" || !strings.Contains(p.actions(), "disk budget used up (2.0 MiB of 1.0 MiB)") {
		t.Errorf("events %q, actions %q", p.events(), p.actions())
	}
}

// Alerts given in earlier runs aren't given again.
func TestRestoredAlert(t *testing.T) {
	p := newProbe()
	tr := New([]*gatewayv0.Budget{{Kind: Tokens, Limit: 100, AlertAtPercent: 50, OnExhaust: gatewayv0.BudgetAction_BUDGET_ACTION_KILL}},
		Restored{Alerted: map[Kind]float64{Tokens: 100}}, p.hooks())
	tr.Spent(60, 0, "")
	if p.events() != "" || len(p.notices) != 0 {
		t.Errorf("events %q, notices %v", p.events(), p.notices)
	}
	if got := tr.Summary(); got != "tokens: 60 tokens of 100 tokens used (60%), alerted" {
		t.Errorf("summary %q", got)
	}
	if got := fmt.Sprint(New(nil, Restored{}, p.hooks()).Summary()); got != "This session has no budgets." {
		t.Errorf("no budgets: %q", got)
	}
}
