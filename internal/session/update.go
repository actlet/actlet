package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/actlet/actlet/internal/attach"
	"github.com/actlet/actlet/internal/budget"
	"github.com/actlet/actlet/internal/session/sessionpb"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// UpdatableFields are the profile fields a profile update may change.
var UpdatableFields = []string{
	"limits.max_workers", "limits.max_tool_concurrency", "limits.max_subagents", "budgets", "local_attach",
}

// changed names what an update changes, for the record.
func changed(u *gatewayv0.ProfileUpdate) []string {
	out := slices.Clone(u.GetFields())
	for _, k := range u.GetResetBudgets() {
		out = append(out, "reset "+budget.Name(k))
	}
	return out
}

// planUpdate returns the configuration a profile update makes, or why it
// is refused: it names a field that may not change, or a value isn't
// valid.
func (s *Session) planUpdate(u *gatewayv0.ProfileUpdate) (*sessionpb.Config, error) {
	if len(u.GetFields()) == 0 && len(u.GetResetBudgets()) == 0 {
		return nil, errors.New("the profile update changes nothing")
	}
	var refused []string
	for _, f := range u.GetFields() {
		if !slices.Contains(UpdatableFields, f) {
			refused = append(refused, f)
		}
	}
	if len(refused) > 0 {
		return nil, fmt.Errorf("these fields can't change during a session: %s; they may: %s",
			strings.Join(refused, ", "), strings.Join(UpdatableFields, ", "))
	}
	s.mu.Lock()
	cfg := proto.Clone(s.config).(*sessionpb.Config)
	s.mu.Unlock()
	p := u.GetProfile()
	capped := func(v, ceiling uint32) uint32 {
		if v == 0 || (ceiling > 0 && v > ceiling) {
			return ceiling
		}
		return v
	}
	for _, f := range u.GetFields() {
		switch f {
		case "limits.max_workers":
			cfg.MaxWorkers = capped(p.GetLimits().GetMaxWorkers(), s.p.MaxWorkers)
		case "limits.max_tool_concurrency":
			cfg.MaxToolConcurrency = capped(p.GetLimits().GetMaxToolConcurrency(), s.p.MaxToolConcurrency)
		case "limits.max_subagents":
			cfg.MaxSubagents = p.GetLimits().GetMaxSubagents()
		case "budgets":
			if err := checkBudgets(p.GetBudgets()); err != nil {
				return nil, err
			}
			cfg.Budgets = p.GetBudgets()
		case "local_attach":
			for _, c := range p.GetLocalAttach().GetCommands() {
				if !slices.Contains([]string{attach.CommandSteer, attach.CommandPause, attach.CommandStop, "none"}, c) {
					return nil, fmt.Errorf("local_attach: unknown command %q", c)
				}
			}
			cfg.LocalCommands = attach.AllowedCommands(p.GetLocalAttach())
		}
	}
	for _, k := range u.GetResetBudgets() {
		if k == budget.Disk || k == gatewayv0.BudgetKind_BUDGET_KIND_UNSPECIFIED {
			return nil, fmt.Errorf("the %s budget can't be reset", budget.Name(k))
		}
	}
	return cfg, nil
}

// checkBudgets checks budgets as the profile schema does.
func checkBudgets(budgets []*gatewayv0.Budget) error {
	seen := map[gatewayv0.BudgetKind]bool{}
	for _, b := range budgets {
		name := budget.Name(b.GetKind())
		switch {
		case b.GetKind() == gatewayv0.BudgetKind_BUDGET_KIND_UNSPECIFIED:
			return errors.New("a budget has no kind")
		case seen[b.GetKind()]:
			return fmt.Errorf("a second %s budget", name)
		case b.GetLimit() <= 0:
			return fmt.Errorf("the %s budget's limit must be positive", name)
		case b.GetKind() == budget.Money && b.GetCurrency() == "":
			return errors.New("the money budget needs a currency")
		case b.GetAlertAtPercent() > 100:
			return fmt.Errorf("the %s budget's alert is above 100%%", name)
		case b.GetOnExhaust() == gatewayv0.BudgetAction_BUDGET_ACTION_UNSPECIFIED:
			return fmt.Errorf("the %s budget says nothing about what happens when it runs out", name)
		case b.GetOnExhaust() == gatewayv0.BudgetAction_BUDGET_ACTION_WAIT && b.GetWaitMode() == gatewayv0.BudgetWaitMode_BUDGET_WAIT_MODE_UNSPECIFIED:
			return fmt.Errorf("the %s budget waits, but doesn't say how: pause or idle", name)
		}
		seen[b.GetKind()] = true
	}
	return nil
}

// updateProfile carries out an accepted profile update: it records the
// new configuration and any budget resets, then makes them take effect.
// An error means the change couldn't be recorded and nothing changed.
func (s *Session) updateProfile(ctx context.Context, commandID string, u *gatewayv0.ProfileUpdate) error {
	cfg, err := s.planUpdate(u)
	if err != nil {
		return err // checked before; can't happen
	}
	if err := s.record(ctx, kindRevision, &sessionpb.ConfigRevision{CommandId: commandID, Config: cfg, Fields: u.GetFields()}); err != nil {
		return fmt.Errorf("record the configuration: %w", err)
	}
	resets := s.budget.Reset(u.GetResetBudgets())
	for _, ev := range resets {
		if err := s.record(ctx, kindBudget, ev); err != nil {
			return fmt.Errorf("record a budget reset: %w", err)
		}
	}
	s.mu.Lock()
	s.config = cfg
	s.mu.Unlock()
	s.limits.Set(int(cfg.GetMaxWorkers()), int(cfg.GetMaxToolConcurrency()), int(cfg.GetMaxSubagents()))
	if slices.Contains(u.GetFields(), "budgets") {
		s.budget.Update(cfg.GetBudgets())
	}
	if len(resets) > 0 {
		s.budget.Apply(resets)
	}
	if s.p.OnConfig != nil {
		s.p.OnConfig(cfg)
	}
	return nil
}
