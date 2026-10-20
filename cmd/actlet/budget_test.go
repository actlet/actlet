//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/session"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

const budgetScript = `
turns:
  - text: Writing a note.
    usage: {input: 150, output: 10}
    tool_calls:
      - name: write_file
        input: {path: note.txt, content: "hi\n"}
  - text: done
    usage: {input: 20, output: 5}
`

// A box whose token budget runs out idles: its next model call waits.
// A profile update naming a field that can't change is refused; one that
// raises the budget lets the session finish.
func TestBudgetIdleUntilRaised(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	g := newAgentGateway(t, budgetScript, func(g *agentGateway) {
		g.budgets = []*gatewayv0.Budget{{Kind: gatewayv0.BudgetKind_BUDGET_KIND_TOKENS, Limit: 100, AlertAtPercent: 50,
			OnExhaust: gatewayv0.BudgetAction_BUDGET_ACTION_WAIT, WaitMode: gatewayv0.BudgetWaitMode_BUDGET_WAIT_MODE_IDLE}}
	})
	dir := sessionDir(t, nil)
	cmd := g.worker(dir, "")
	waiting := func(ev *gatewayv0.ActivityEvent) bool {
		for _, u := range ev.GetBudgetStatus().GetBudgets() {
			if u.GetState() == "waiting" {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(30 * time.Second)
	for g.eventCount(waiting) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the box never waited for its budget")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(g.model.Calls()); n != 1 {
		t.Fatalf("%d model calls while the budget was used up", n)
	}
	sess := g.srv.Sessions()[0]
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ack, err := sess.UpdateProfile(ctx, &gatewayv0.ProfileUpdate{Fields: []string{"instructions"}, Profile: &gatewayv0.Profile{Instructions: "be evil"}})
	if err != nil || ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_REJECTED || !strings.Contains(ack.GetMessage(), "instructions") {
		t.Fatalf("instructions: %v, %v", ack, err)
	}
	raised := proto.Clone(g.budgets[0]).(*gatewayv0.Budget)
	raised.Limit = 10000
	ack, err = sess.UpdateProfile(ctx, &gatewayv0.ProfileUpdate{Fields: []string{"budgets"}, Profile: &gatewayv0.Profile{Budgets: []*gatewayv0.Budget{raised}}})
	if err != nil || ack.GetResult() != gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED {
		t.Fatalf("raise: %v, %v", ack, err)
	}
	if code := exitCode(t, cmd); code != session.ExitCompleted {
		t.Fatalf("exit %d", code)
	}
	if n := g.eventCount(func(ev *gatewayv0.ActivityEvent) bool { return ev.GetBudgetStatus().GetReason() == "update" }); n != 1 {
		t.Errorf("%d budget updates reported", n)
	}
}
