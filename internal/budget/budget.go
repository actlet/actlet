// Package budget tracks a session's budgets: wall-clock time since the
// session first started, the cost the gateway reports, model tokens and
// the session folder's size, over all the session's runs. When a budget reaches its alert threshold, the
// agent is told once; when it is used up, the session is stopped, pauses,
// or idles until the budget is raised or reset, as its profile says.
//
// Budget events are recorded in the session journal before the tracker
// acts on them, and a run restores what earlier runs recorded.
package budget

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/actlet/actlet/internal/session/sessionpb"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Kind is what a budget limits.
type Kind = gatewayv0.BudgetKind

// The budget kinds.
const (
	Time   = gatewayv0.BudgetKind_BUDGET_KIND_TIME
	Money  = gatewayv0.BudgetKind_BUDGET_KIND_MONEY
	Tokens = gatewayv0.BudgetKind_BUDGET_KIND_TOKENS
	Disk   = gatewayv0.BudgetKind_BUDGET_KIND_DISK
)

// How often the tracker looks at the time and the session folder's size,
// and how soon after a tool call it looks at the folder again.
const (
	tick         = time.Second
	diskEvery    = 15 * time.Second
	diskAfterUse = 2 * time.Second
)

// Name is a budget kind in lower case, such as "money".
func Name(k Kind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "BUDGET_KIND_"))
}

// Hooks are how the tracker records, reports and acts. Each may be nil.
type Hooks struct {
	// Record journals a budget event before the tracker acts on it. When
	// it fails, the tracker doesn't act.
	Record func(*sessionpb.BudgetEvent) error
	// Emit publishes a budget status.
	Emit func(*gatewayv0.BudgetStatus)
	// Notify gives the agent a notice, once per ID.
	Notify func(id, text string)
	// Stop stops the session; Pause pauses it; Idle says new work waits.
	Stop, Pause, Idle func(reason string)
	// Disk returns the session folder's size in bytes.
	Disk func() int64
	// Now returns the time; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Restored is what earlier runs recorded.
type Restored struct {
	// FirstStart is when the session first started; zero for a session
	// starting now.
	FirstStart time.Time
	// Offsets are the usage totals budgets were reset at.
	Offsets map[Kind]float64
	// Alerted are the limits at which each budget's alert was given.
	Alerted map[Kind]float64
}

// Tracker tracks a session's budgets in a run.
type Tracker struct {
	h Hooks

	mu       sync.Mutex
	budgets  map[Kind]*gatewayv0.Budget
	order    []Kind
	first    time.Time // the session's first start
	tokens   float64
	cost     float64
	currency string
	disk     float64
	diskAt   time.Time
	offsets  map[Kind]float64
	alerted  map[Kind]float64
	// exhausted are the limits each budget was used up at and acted on.
	exhausted map[Kind]float64
	waiting   map[Kind]bool
	wake      chan struct{} // closed and replaced when waiting may end
	warned    bool          // about a cost in another currency
}

// New returns a tracker for the budgets, of a session that first started
// at r.FirstStart, or now.
func New(budgets []*gatewayv0.Budget, r Restored, h Hooks) *Tracker {
	if h.Now == nil {
		h.Now = time.Now
	}
	if h.Logger == nil {
		h.Logger = slog.Default()
	}
	t := &Tracker{
		h: h, first: r.FirstStart,
		offsets: map[Kind]float64{}, alerted: map[Kind]float64{},
		exhausted: map[Kind]float64{}, waiting: map[Kind]bool{}, wake: make(chan struct{}),
	}
	if t.first.IsZero() {
		t.first = h.Now()
	}
	for k, v := range r.Offsets {
		t.offsets[k] = v
	}
	for k, v := range r.Alerted {
		t.alerted[k] = v
	}
	t.setBudgets(budgets)
	return t
}

func (t *Tracker) setBudgets(budgets []*gatewayv0.Budget) {
	t.budgets, t.order = map[Kind]*gatewayv0.Budget{}, nil
	for _, b := range budgets {
		if b.GetLimit() > 0 && b.GetKind() != gatewayv0.BudgetKind_BUDGET_KIND_UNSPECIFIED {
			t.budgets[b.GetKind()] = b
			t.order = append(t.order, b.GetKind())
		}
	}
}

// Empty reports whether there are no budgets to track.
func (t *Tracker) Empty() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.budgets) == 0
}

// Run reports the budgets, then watches time and the session folder's
// size until ctx ends.
func (t *Tracker) Run(ctx context.Context) {
	t.sampleDisk(true)
	t.check("start")
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sampleDisk(false)
			t.check("")
		}
	}
}

// Spent sets what the agent's model and bridged tool calls have used in
// all: their tokens, and their cost in a currency.
func (t *Tracker) Spent(tokens uint64, cost float64, currency string) {
	t.mu.Lock()
	t.tokens = float64(tokens)
	if b := t.budgets[Money]; b != nil && cost > 0 && currency != b.GetCurrency() {
		if !t.warned {
			t.warned = true
			t.h.Logger.Warn("budget: costs are reported in another currency than the money budget's; they don't count",
				"currency", currency, "budget", b.GetCurrency())
		}
	} else {
		t.cost, t.currency = cost, currency
	}
	t.mu.Unlock()
	t.check("")
}

// ToolDone notes that a tool ran, which may have changed the session
// folder's size.
func (t *Tracker) ToolDone() {
	t.sampleDisk(false)
	t.check("")
}

func (t *Tracker) sampleDisk(force bool) {
	t.mu.Lock()
	b := t.budgets[Disk]
	due := b != nil && t.h.Disk != nil && (force || t.h.Now().Sub(t.diskAt) >= diskAfterUse)
	if due {
		t.diskAt = t.h.Now()
	}
	t.mu.Unlock()
	if due {
		size := float64(t.h.Disk())
		t.mu.Lock()
		t.disk = size
		t.mu.Unlock()
	}
}

// usedLocked returns a budget's usage since its last reset.
func (t *Tracker) usedLocked(k Kind) float64 {
	var total float64
	switch k {
	case Time:
		total = t.h.Now().Sub(t.first).Seconds()
	case Money:
		total = t.cost
	case Tokens:
		total = t.tokens
	case Disk:
		total = t.disk
	}
	if k == Disk {
		return total // the folder's size now; a reset doesn't apply
	}
	return math.Max(total-t.offsets[k], 0)
}

// totalLocked returns a budget's usage over all runs, ignoring resets.
func (t *Tracker) totalLocked(k Kind) float64 {
	return t.usedLocked(k) + t.offsets[k]
}

// action is what the tracker does once it has recorded an event.
type action struct {
	ev     *sessionpb.BudgetEvent
	notice string
	do     func(string)
	reason string
}

// check compares usage with the limits, and records and acts on what
// crossed a threshold. A reason reports the status even if nothing did.
func (t *Tracker) check(reason string) {
	t.mu.Lock()
	var acts []action
	for _, k := range t.order {
		b := t.budgets[k]
		used, limit := t.usedLocked(k), b.GetLimit()
		if used >= limit {
			if t.exhausted[k] == limit {
				continue
			}
			t.exhausted[k] = limit
			a := action{ev: &sessionpb.BudgetEvent{Kind: k, Event: "exhausted", Used: used, Limit: limit, Action: actionName(b)}}
			a.reason = fmt.Sprintf("%s budget used up (%s of %s)", Name(k), amount(b, used), amount(b, limit))
			switch a.ev.Action {
			case "kill":
				a.do = t.h.Stop
			case "pause":
				a.do = t.h.Pause
				a.reason += "; pausing until it is raised"
			case "idle":
				t.waiting[k] = true
				a.do = t.h.Idle
				a.reason += "; new work waits until it is raised"
			}
			acts = append(acts, a)
			continue
		}
		if p := b.GetAlertAtPercent(); p > 0 && used*100 >= float64(p)*limit && t.alerted[k] != limit {
			t.alerted[k] = limit
			ev := &sessionpb.BudgetEvent{Kind: k, Event: "alert", Used: used, Limit: limit, Action: actionName(b)}
			acts = append(acts, action{ev: ev, notice: Notice(ev, b.GetCurrency())})
		}
	}
	status := t.statusLocked(reason)
	t.mu.Unlock()

	for _, a := range acts {
		if t.h.Record != nil {
			if err := t.h.Record(a.ev); err != nil {
				t.h.Logger.Error("budget: recording an event", "err", err)
				return
			}
		}
		if a.notice != "" && t.h.Notify != nil {
			t.h.Notify(NoticeID(a.ev), a.notice)
		}
		if a.do != nil {
			a.do(a.reason)
		}
		status.Reason = a.ev.GetEvent()
	}
	if (len(acts) > 0 || reason != "") && t.h.Emit != nil && len(status.GetBudgets()) > 0 {
		t.h.Emit(status)
	}
}

func actionName(b *gatewayv0.Budget) string {
	if b.GetOnExhaust() == gatewayv0.BudgetAction_BUDGET_ACTION_WAIT {
		if b.GetWaitMode() == gatewayv0.BudgetWaitMode_BUDGET_WAIT_MODE_IDLE {
			return "idle"
		}
		return "pause"
	}
	return "kill"
}

// NoticeID identifies a budget alert's notice to the agent: one per
// budget and limit.
func NoticeID(ev *sessionpb.BudgetEvent) string {
	return fmt.Sprintf("budget-%s-%g", Name(ev.GetKind()), ev.GetLimit())
}

// Notice is what the agent is told when a budget reaches its alert
// threshold.
func Notice(ev *sessionpb.BudgetEvent, currency string) string {
	b := &gatewayv0.Budget{Kind: ev.GetKind(), Currency: currency}
	then := "the session will be stopped"
	switch ev.GetAction() {
	case "pause":
		then = "the session will pause until it is raised"
	case "idle":
		then = "your work will wait until it is raised"
	}
	return fmt.Sprintf("You have used %.0f%% of the session's %s budget (%s of %s). When it runs out, %s. Plan to finish your task within it.",
		ev.GetUsed()*100/ev.GetLimit(), Name(ev.GetKind()), amount(b, ev.GetUsed()), amount(b, ev.GetLimit()), then)
}

// amount writes a quantity in its budget's unit.
func amount(b *gatewayv0.Budget, v float64) string {
	switch b.GetKind() {
	case Time:
		return (time.Duration(v) * time.Second).Round(time.Second).String()
	case Money:
		return strings.TrimSpace(fmt.Sprintf("%.2f %s", v, b.GetCurrency()))
	case Tokens:
		return fmt.Sprintf("%.0f tokens", v)
	case Disk:
		return fmt.Sprintf("%.1f MiB", v/(1<<20))
	}
	return fmt.Sprintf("%g", v)
}

func (t *Tracker) statusLocked(reason string) *gatewayv0.BudgetStatus {
	st := &gatewayv0.BudgetStatus{Reason: reason}
	for _, k := range t.order {
		b := t.budgets[k]
		used := t.usedLocked(k)
		state := "ok"
		switch {
		case t.waiting[k]:
			state = "waiting"
		case used >= b.GetLimit():
			state = "exhausted"
		case b.GetAlertAtPercent() > 0 && used*100 >= float64(b.GetAlertAtPercent())*b.GetLimit():
			state = "alerted"
		}
		st.Budgets = append(st.Budgets, &gatewayv0.BudgetUsage{Kind: k, Limit: b.GetLimit(), Used: used, Currency: b.GetCurrency(), State: state})
	}
	return st
}

// Status returns the budgets and their usage now.
func (t *Tracker) Status() *gatewayv0.BudgetStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.statusLocked("")
}

// Summary describes the budgets for the agent.
func (t *Tracker) Summary() string {
	st := t.Status()
	if len(st.GetBudgets()) == 0 {
		return "This session has no budgets."
	}
	var lines []string
	for _, u := range st.GetBudgets() {
		b := &gatewayv0.Budget{Kind: u.GetKind(), Currency: u.GetCurrency()}
		lines = append(lines, fmt.Sprintf("%s: %s of %s used (%.0f%%), %s", Name(u.GetKind()), amount(b, u.GetUsed()),
			amount(b, u.GetLimit()), u.GetUsed()*100/u.GetLimit(), u.GetState()))
	}
	return strings.Join(lines, "\n")
}

// Wait returns when budget-consuming work may start: at once, unless a
// budget is used up and the session idles until it is raised. It returns
// ctx's error if ctx ends first.
func (t *Tracker) Wait(ctx context.Context) error {
	for {
		t.mu.Lock()
		waiting := len(t.waiting) > 0
		wake := t.wake
		t.mu.Unlock()
		if !waiting {
			return nil
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Update sets new budgets, such as from a profile update. A used-up
// budget whose limit is raised above its usage no longer holds work back,
// and an alert is given again at the new limit.
func (t *Tracker) Update(budgets []*gatewayv0.Budget) {
	t.mu.Lock()
	t.setBudgets(budgets)
	t.refreshLocked()
	t.mu.Unlock()
	t.check("update")
}

// Reset starts the budgets' usage again from zero. It returns the events
// to record, which Restore reads back; the caller records them before the
// reset takes effect, with Apply.
func (t *Tracker) Reset(kinds []Kind) []*sessionpb.BudgetEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*sessionpb.BudgetEvent
	for _, k := range kinds {
		if k == Disk {
			continue // the folder's size can't be reset
		}
		out = append(out, &sessionpb.BudgetEvent{Kind: k, Event: "reset", Offset: t.totalLocked(k)})
	}
	return out
}

// Apply makes recorded resets take effect.
func (t *Tracker) Apply(resets []*sessionpb.BudgetEvent) {
	t.mu.Lock()
	for _, ev := range resets {
		t.offsets[ev.GetKind()] = ev.GetOffset()
		delete(t.alerted, ev.GetKind())
	}
	t.refreshLocked()
	t.mu.Unlock()
	t.check("reset")
}

// refreshLocked lets budgets that are no longer used up hold no work
// back.
func (t *Tracker) refreshLocked() {
	woke := false
	for k := range t.exhausted {
		b := t.budgets[k]
		if b == nil || t.usedLocked(k) < b.GetLimit() {
			delete(t.exhausted, k)
			if t.waiting[k] {
				delete(t.waiting, k)
				woke = true
			}
		}
	}
	if woke {
		close(t.wake)
		t.wake = make(chan struct{})
	}
}
