package sdk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"sigs.k8s.io/yaml"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// ProfileSchema is the version of the profile document this package reads
// and writes.
const ProfileSchema = "v0"

// Profile is an execution profile: what the orchestrator lets a session do.
// Build one in Go, or load it from a JSON or YAML file with LoadProfile; both
// describe the same schema. Proto converts it to the wire form for
// Assign.
type Profile struct {
	// Schema must be ProfileSchema.
	Schema  string `json:"schema"`
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	// RequiredFeatures lists worker capabilities the profile needs. A worker
	// without all of them is refused at registration.
	RequiredFeatures []string `json:"required_features,omitempty"`
	Limits           Limits   `json:"limits,omitempty"`
	// DefaultPauseMode is "drain" (the default) or "interrupt": how the
	// worker pauses on an OS signal.
	DefaultPauseMode string `json:"default_pause_mode,omitempty"`
	// StopGrace is the grace period of stop commands that give none.
	StopGrace Duration    `json:"stop_grace,omitempty"`
	Budgets   []Budget    `json:"budgets,omitempty"`
	Models    *Models     `json:"models,omitempty"`
	Tools     []ToolGrant `json:"tools,omitempty"`
	// Instructions are the implementor's instructions to the agent, added
	// to Actlet's base prompt.
	Instructions string `json:"instructions,omitempty"`
	// BasePrompt replaces Actlet's base prompt.
	BasePrompt string `json:"base_prompt,omitempty"`
	// ToolsManifest describes what is installed in the container, for the
	// agent's prompt.
	ToolsManifest string `json:"tools_manifest,omitempty"`
	// CheckpointEveryTurns is the number of model turns between execution
	// checkpoints; 0 means every turn.
	CheckpointEveryTurns uint32 `json:"checkpoint_every_turns,omitempty"`
	// LocalAttach controls viewers attached to the box itself.
	LocalAttach *LocalAttach `json:"local_attach,omitempty"`
	// Compaction is when and how workers compact their context. Unset
	// compacts by default.
	Compaction *Compaction `json:"compaction,omitempty"`
}

// Compaction is when and how workers compact their context: when it
// nears the model's context window, a model summarises the older turns.
type Compaction struct {
	// Strategy is "summarize" (the default) or "off".
	Strategy string `json:"strategy,omitempty"`
	// ThresholdPercent of the alias's context window starts a
	// compaction; 0 means 80.
	ThresholdPercent uint32 `json:"threshold_percent,omitempty"`
	// Model is the alias that writes the summary; empty means the
	// "compaction" role, or else the worker's own model.
	Model string `json:"model,omitempty"`
	// KeepTurns are the most recent model turns kept as they are; 0
	// means 4.
	KeepTurns uint32 `json:"keep_turns,omitempty"`
}

// LocalAttach controls viewers attached to the box itself, such as the
// local TUI.
type LocalAttach struct {
	// Commands they may send: "steer", "pause", "stop". Empty allows steer
	// notes only; ["none"] allows nothing.
	Commands []string `json:"commands,omitempty"`
	// Disabled turns local attach off.
	Disabled bool `json:"disabled,omitempty"`
}

// Limits caps concurrency. Zero means the worker's deployment ceiling.
type Limits struct {
	// MaxWorkers bounds the workers (the agent and its subagents) running
	// at once.
	MaxWorkers uint32 `json:"max_workers,omitempty"`
	// MaxToolConcurrency bounds the tool calls of one model turn running
	// at once.
	MaxToolConcurrency uint32 `json:"max_tool_concurrency,omitempty"`
	// MaxSubagentDepth bounds how deep subagents nest, and MaxSubagents
	// how many a session creates. Zero means the worker's default.
	MaxSubagentDepth uint32 `json:"max_subagent_depth,omitempty"`
	MaxSubagents     uint32 `json:"max_subagents,omitempty"`
}

// Budget limits one resource.
type Budget struct {
	// Kind is "time", "money", "tokens" or "disk".
	Kind string `json:"kind"`
	// Limit is in seconds, currency units, tokens or bytes.
	Limit float64 `json:"limit"`
	// Currency is the ISO 4217 code of a money budget.
	Currency string `json:"currency,omitempty"`
	// AlertAtPercent reports when this much of the limit is used; 0 never.
	AlertAtPercent uint32 `json:"alert_at_percent,omitempty"`
	// OnExhaust is "kill" or "wait".
	OnExhaust string `json:"on_exhaust"`
	// WaitMode is "pause" or "idle"; required when OnExhaust is "wait".
	WaitMode string `json:"wait_mode,omitempty"`
}

// Models names the models a session may use by alias.
type Models struct {
	Aliases []ModelAlias `json:"aliases"`
	// Roles maps a role, such as "main", "subagent" or "llm", to an alias.
	Roles map[string]string `json:"roles,omitempty"`
	// Selectable are the aliases the agent may choose itself, such as for
	// a subagent.
	Selectable []string `json:"selectable,omitempty"`
}

// ModelAlias is a model as the orchestrator presents it: a real model name
// or a tier such as "large" or "fast".
type ModelAlias struct {
	Alias        string   `json:"alias"`
	Description  string   `json:"description,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// ContextWindow is the model's context window in tokens, if known.
	ContextWindow uint64 `json:"context_window,omitempty"`
}

// ToolGrant allows a tool.
type ToolGrant struct {
	Name     string            `json:"name"`
	Settings map[string]string `json:"settings,omitempty"`
}

// Duration is a time.Duration written as a string such as "30s" in JSON and
// YAML.
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`a duration must be a string such as "30s"`)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

var (
	idPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

// Validate checks the profile against the schema.
func (p *Profile) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if p.Schema != ProfileSchema {
		bad("schema is %q, want %q", p.Schema, ProfileSchema)
	}
	if !idPattern.MatchString(p.ID) {
		bad("id %q: use 1 to 128 letters, digits and . _ : / -", p.ID)
	}
	checkUnique("required_features", p.RequiredFeatures, bad)
	switch p.DefaultPauseMode {
	case "", "drain", "interrupt":
	default:
		bad("default_pause_mode %q: use drain or interrupt", p.DefaultPauseMode)
	}
	if p.StopGrace < 0 {
		bad("stop_grace is negative")
	}
	kinds := map[string]bool{}
	for i, b := range p.Budgets {
		at := fmt.Sprintf("budgets[%d]", i)
		switch b.Kind {
		case "time", "money", "tokens", "disk":
			if kinds[b.Kind] {
				bad("%s: a second %s budget", at, b.Kind)
			}
			kinds[b.Kind] = true
		default:
			bad("%s: kind %q: use time, money, tokens or disk", at, b.Kind)
		}
		if b.Limit <= 0 {
			bad("%s: limit must be positive", at)
		}
		if b.Kind == "money" && !currencyPattern.MatchString(b.Currency) {
			bad("%s: a money budget needs a currency code such as EUR", at)
		}
		if b.Kind != "money" && b.Currency != "" {
			bad("%s: currency only applies to money budgets", at)
		}
		if b.AlertAtPercent > 100 {
			bad("%s: alert_at_percent above 100", at)
		}
		switch {
		case b.OnExhaust == "kill" && b.WaitMode == "":
		case b.OnExhaust == "kill":
			bad("%s: wait_mode only applies when on_exhaust is wait", at)
		case b.OnExhaust == "wait" && (b.WaitMode == "pause" || b.WaitMode == "idle"):
		case b.OnExhaust == "wait":
			bad("%s: wait_mode %q: use pause or idle", at, b.WaitMode)
		default:
			bad("%s: on_exhaust %q: use kill or wait", at, b.OnExhaust)
		}
	}
	if m := p.Models; m != nil {
		aliases := map[string]bool{}
		for i, a := range m.Aliases {
			if a.Alias == "" {
				bad("models.aliases[%d]: empty alias", i)
			} else if aliases[a.Alias] {
				bad("models.aliases[%d]: duplicate alias %q", i, a.Alias)
			}
			aliases[a.Alias] = true
		}
		for _, alias := range m.Selectable {
			if !aliases[alias] {
				bad("models.selectable: unknown alias %q", alias)
			}
		}
		for role, alias := range m.Roles {
			if role == "" {
				bad("models.roles: empty role")
			}
			if !aliases[alias] {
				bad("models.roles.%s: unknown alias %q", role, alias)
			}
		}
	}
	if la := p.LocalAttach; la != nil {
		for _, c := range la.Commands {
			switch c {
			case "steer", "pause", "stop":
			case "none":
				if len(la.Commands) > 1 {
					bad("local_attach.commands: \"none\" can't be combined with others")
				}
			default:
				bad("local_attach.commands: unknown command %q; use steer, pause, stop or none", c)
			}
		}
	}
	if c := p.Compaction; c != nil {
		switch c.Strategy {
		case "", "summarize", "off":
		default:
			bad("compaction.strategy %q: use summarize or off", c.Strategy)
		}
		if c.ThresholdPercent > 100 {
			bad("compaction.threshold_percent above 100")
		}
		if c.Model != "" && p.Models != nil && !slices.ContainsFunc(p.Models.Aliases, func(a ModelAlias) bool { return a.Alias == c.Model }) {
			bad("compaction.model: unknown alias %q", c.Model)
		}
	}
	tools := map[string]bool{}
	for i, t := range p.Tools {
		if t.Name == "" {
			bad("tools[%d]: empty name", i)
		} else if tools[t.Name] {
			bad("tools[%d]: duplicate tool %q", i, t.Name)
		}
		tools[t.Name] = true
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid profile: %w", errors.Join(errs...))
	}
	return nil
}

func checkUnique(field string, values []string, bad func(string, ...any)) {
	seen := map[string]bool{}
	for _, v := range values {
		if v == "" {
			bad("%s: empty entry", field)
		} else if seen[v] {
			bad("%s: duplicate %q", field, v)
		}
		seen[v] = true
	}
}

// Proto validates the profile and returns its wire form.
func (p *Profile) Proto() (*gatewayv0.Profile, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	out := &gatewayv0.Profile{
		Id:               p.ID,
		Version:          p.Version,
		RequiredFeatures: p.RequiredFeatures,
		Limits: &gatewayv0.Limits{
			MaxWorkers: p.Limits.MaxWorkers, MaxToolConcurrency: p.Limits.MaxToolConcurrency,
			MaxSubagentDepth: p.Limits.MaxSubagentDepth, MaxSubagents: p.Limits.MaxSubagents,
		},
		DefaultPauseMode: gatewayv0.PauseMode_PAUSE_MODE_DRAIN,
		OnDisconnect:     gatewayv0.DisconnectBehavior_DISCONNECT_BEHAVIOR_CONTINUE_LOCAL,
		Instructions:     p.Instructions,
		BasePrompt:       p.BasePrompt,
		ToolsManifest:    p.ToolsManifest,

		CheckpointEveryTurns: p.CheckpointEveryTurns,
	}
	if la := p.LocalAttach; la != nil {
		out.LocalAttach = &gatewayv0.LocalAttach{Commands: la.Commands, Disabled: la.Disabled}
	}
	if c := p.Compaction; c != nil {
		out.Compaction = &gatewayv0.CompactionPolicy{Strategy: c.Strategy, ThresholdPercent: c.ThresholdPercent, Model: c.Model, KeepTurns: c.KeepTurns}
	}
	if p.DefaultPauseMode == "interrupt" {
		out.DefaultPauseMode = gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT
	}
	if p.StopGrace > 0 {
		out.StopGrace = durationpb.New(time.Duration(p.StopGrace))
	}
	for _, b := range p.Budgets {
		out.Budgets = append(out.Budgets, &gatewayv0.Budget{
			Kind:           budgetKinds[b.Kind],
			Limit:          b.Limit,
			Currency:       b.Currency,
			AlertAtPercent: b.AlertAtPercent,
			OnExhaust:      budgetActions[b.OnExhaust],
			WaitMode:       budgetWaitModes[b.WaitMode],
		})
	}
	if m := p.Models; m != nil {
		out.Models = &gatewayv0.ModelCatalog{Roles: m.Roles, Selectable: m.Selectable}
		for _, a := range m.Aliases {
			out.Models.Aliases = append(out.Models.Aliases, &gatewayv0.ModelAlias{
				Alias: a.Alias, Description: a.Description, Capabilities: a.Capabilities, ContextWindow: a.ContextWindow,
			})
		}
	}
	for _, t := range p.Tools {
		out.Tools = append(out.Tools, &gatewayv0.ToolGrant{Name: t.Name, Settings: t.Settings})
	}
	return out, nil
}

var (
	budgetKinds = map[string]gatewayv0.BudgetKind{
		"time": gatewayv0.BudgetKind_BUDGET_KIND_TIME, "money": gatewayv0.BudgetKind_BUDGET_KIND_MONEY,
		"tokens": gatewayv0.BudgetKind_BUDGET_KIND_TOKENS, "disk": gatewayv0.BudgetKind_BUDGET_KIND_DISK,
	}
	budgetActions = map[string]gatewayv0.BudgetAction{
		"kill": gatewayv0.BudgetAction_BUDGET_ACTION_KILL, "wait": gatewayv0.BudgetAction_BUDGET_ACTION_WAIT,
	}
	budgetWaitModes = map[string]gatewayv0.BudgetWaitMode{
		"pause": gatewayv0.BudgetWaitMode_BUDGET_WAIT_MODE_PAUSE, "idle": gatewayv0.BudgetWaitMode_BUDGET_WAIT_MODE_IDLE,
	}
)

// ParseProfileJSON reads a profile from JSON. Unknown fields are rejected
// and the result is validated.
func ParseProfileJSON(data []byte) (*Profile, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	if dec.More() {
		return nil, errors.New("profile: more than one JSON document")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// ParseProfileYAML reads a profile from YAML with the same fields and rules
// as JSON. Duplicate keys and unknown fields are rejected.
func ParseProfileYAML(data []byte) (*Profile, error) {
	j, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	return ParseProfileJSON(j)
}

// LoadProfile reads a profile file: JSON for .json, YAML for .yaml or .yml.
func LoadProfile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p *Profile
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".json":
		p, err = ParseProfileJSON(data)
	case ".yaml", ".yml":
		p, err = ParseProfileYAML(data)
	default:
		return nil, fmt.Errorf("profile %s: unknown format %q: use .json, .yaml or .yml", path, ext)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// ParseProfileUpdate reads a profile update, in YAML or JSON: a partial
// profile document with the fields to change, and optionally
// reset_budgets, the budget kinds to reset. Each field present is
// changed, and nested limits are changed one by one, such as
// "limits.max_workers". A field a worker doesn't let change is passed
// on, for the worker to refuse the update.
func ParseProfileUpdate(data []byte) (*gatewayv0.ProfileUpdate, error) {
	j, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return nil, fmt.Errorf("profile update: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(j, &raw); err != nil {
		return nil, fmt.Errorf("profile update: %w", err)
	}
	u := &gatewayv0.ProfileUpdate{}
	if r, ok := raw["reset_budgets"]; ok {
		var kinds []string
		if err := json.Unmarshal(r, &kinds); err != nil {
			return nil, fmt.Errorf("profile update: reset_budgets: %w", err)
		}
		for _, k := range kinds {
			kind, ok := budgetKinds[k]
			if !ok {
				return nil, fmt.Errorf("profile update: reset_budgets: kind %q: use time, money, tokens or disk", k)
			}
			u.ResetBudgets = append(u.ResetBudgets, kind)
		}
		delete(raw, "reset_budgets")
	}
	for key, value := range raw {
		if key == "limits" {
			var limits map[string]json.RawMessage
			if err := json.Unmarshal(value, &limits); err != nil {
				return nil, fmt.Errorf("profile update: limits: %w", err)
			}
			for k := range limits {
				u.Fields = append(u.Fields, "limits."+k)
			}
			continue
		}
		u.Fields = append(u.Fields, key)
	}
	slices.Sort(u.Fields)
	// The rest is a profile, given a schema and an ID to validate.
	raw["schema"], raw["id"] = json.RawMessage(`"`+ProfileSchema+`"`), json.RawMessage(`"update"`)
	doc, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	p, err := ParseProfileJSON(doc)
	if err != nil {
		return nil, fmt.Errorf("profile update: %w", err)
	}
	if u.Profile, err = p.Proto(); err != nil {
		return nil, fmt.Errorf("profile update: %w", err)
	}
	if len(u.Fields) == 0 && len(u.ResetBudgets) == 0 {
		return nil, errors.New("profile update: it changes nothing")
	}
	return u, nil
}

// LoadProfileUpdate reads a profile update file, YAML or JSON.
func LoadProfileUpdate(path string) (*gatewayv0.ProfileUpdate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	u, err := ParseProfileUpdate(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return u, nil
}
