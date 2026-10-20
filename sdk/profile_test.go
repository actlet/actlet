package sdk

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

func TestLoadProfileYAMLAndJSONAgree(t *testing.T) {
	fromYAML, err := LoadProfile("testdata/profile.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The same profile as JSON loads to the same thing.
	data, err := json.Marshal(fromYAML)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fromJSON, err := LoadProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(fromYAML)
	b, _ := json.Marshal(fromJSON)
	if string(a) != string(b) {
		t.Fatalf("YAML and JSON differ:\n%s\n%s", a, b)
	}

	pb, err := fromYAML.Proto()
	if err != nil {
		t.Fatal(err)
	}
	if pb.GetId() != "coding-default" || pb.GetStopGrace().AsDuration() != 45*time.Second ||
		pb.GetDefaultPauseMode() != gatewayv0.PauseMode_PAUSE_MODE_INTERRUPT || len(pb.GetRequiredFeatures()) != 3 {
		t.Errorf("profile %v", pb)
	}
	if b := pb.GetBudgets(); len(b) != 2 || b[0].GetWaitMode() != gatewayv0.BudgetWaitMode_BUDGET_WAIT_MODE_PAUSE ||
		b[1].GetKind() != gatewayv0.BudgetKind_BUDGET_KIND_MONEY || b[1].GetCurrency() != "EUR" {
		t.Errorf("budgets %v", b)
	}
	if l := pb.GetLimits(); l.GetMaxSubagentDepth() != 1 || l.GetMaxSubagents() != 3 {
		t.Errorf("limits %v", l)
	}
	if m := pb.GetModels(); m.GetRoles()["main"] != "large" || len(m.GetAliases()) != 2 || len(m.GetSelectable()) != 2 {
		t.Errorf("models %v", m)
	}
	if tools := pb.GetTools(); len(tools) != 2 || tools[1].GetSettings()["route"] != "humans" {
		t.Errorf("tools %v", tools)
	}
	if pb.GetInstructions() != "Write the report in British English.\n" || pb.GetToolsManifest() == "" || pb.GetCheckpointEveryTurns() != 2 {
		t.Errorf("agent fields %q %q %d", pb.GetInstructions(), pb.GetToolsManifest(), pb.GetCheckpointEveryTurns())
	}
}

func TestProfileFromGo(t *testing.T) {
	p := &Profile{Schema: ProfileSchema, ID: "minimal"}
	pb, err := p.Proto()
	if err != nil {
		t.Fatal(err)
	}
	if pb.GetDefaultPauseMode() != gatewayv0.PauseMode_PAUSE_MODE_DRAIN || pb.GetStopGrace() != nil {
		t.Errorf("defaults: %v", pb)
	}
}

func TestProfileStrictParsing(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown field":   "schema: v0\nid: p\ncolour: blue\n",
		"unknown nested":  "schema: v0\nid: p\nlimits: {max_workers: 1, maximum: 2}\n",
		"duplicate key":   "schema: v0\nid: p\nid: q\n",
		"wrong type":      "schema: v0\nid: p\nlimits: {max_workers: lots}\n",
		"duration number": "schema: v0\nid: p\nstop_grace: 30\n",
		"bad duration":    "schema: v0\nid: p\nstop_grace: soon\n",
		"not a mapping":   "- schema\n",
	} {
		if _, err := ParseProfileYAML([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseProfileJSON([]byte(`{"schema":"v0","id":"p"} {"schema":"v0","id":"q"}`)); err == nil {
		t.Error("accepted two JSON documents")
	}
	if _, err := LoadProfile(filepath.Join(t.TempDir(), "p.toml")); err == nil {
		t.Error("accepted an unknown extension")
	}
}

func TestProfileValidation(t *testing.T) {
	base := func() *Profile { return &Profile{Schema: ProfileSchema, ID: "p"} }
	cases := map[string]func(*Profile){
		"wrong schema":        func(p *Profile) { p.Schema = "v1" },
		"missing id":          func(p *Profile) { p.ID = "" },
		"bad id":              func(p *Profile) { p.ID = "has space" },
		"bad pause mode":      func(p *Profile) { p.DefaultPauseMode = "sleep" },
		"negative grace":      func(p *Profile) { p.StopGrace = Duration(-time.Second) },
		"duplicate feature":   func(p *Profile) { p.RequiredFeatures = []string{"a", "a"} },
		"empty feature":       func(p *Profile) { p.RequiredFeatures = []string{""} },
		"unknown budget kind": func(p *Profile) { p.Budgets = []Budget{{Kind: "cpu", Limit: 1, OnExhaust: "kill"}} },
		"zero limit":          func(p *Profile) { p.Budgets = []Budget{{Kind: "time", OnExhaust: "kill"}} },
		"money without code":  func(p *Profile) { p.Budgets = []Budget{{Kind: "money", Limit: 1, OnExhaust: "kill"}} },
		"currency on time":    func(p *Profile) { p.Budgets = []Budget{{Kind: "time", Limit: 1, Currency: "EUR", OnExhaust: "kill"}} },
		"alert over 100": func(p *Profile) {
			p.Budgets = []Budget{{Kind: "time", Limit: 1, AlertAtPercent: 101, OnExhaust: "kill"}}
		},
		"wait without mode":   func(p *Profile) { p.Budgets = []Budget{{Kind: "time", Limit: 1, OnExhaust: "wait"}} },
		"kill with wait mode": func(p *Profile) { p.Budgets = []Budget{{Kind: "time", Limit: 1, OnExhaust: "kill", WaitMode: "idle"}} },
		"unknown action":      func(p *Profile) { p.Budgets = []Budget{{Kind: "time", Limit: 1, OnExhaust: "shrug"}} },
		"two time budgets": func(p *Profile) {
			p.Budgets = []Budget{{Kind: "time", Limit: 1, OnExhaust: "kill"}, {Kind: "time", Limit: 2, OnExhaust: "kill"}}
		},
		"role to unknown alias": func(p *Profile) {
			p.Models = &Models{Aliases: []ModelAlias{{Alias: "a"}}, Roles: map[string]string{"main": "b"}}
		},
		"selectable unknown alias": func(p *Profile) {
			p.Models = &Models{Aliases: []ModelAlias{{Alias: "a"}}, Selectable: []string{"b"}}
		},
		"duplicate alias": func(p *Profile) { p.Models = &Models{Aliases: []ModelAlias{{Alias: "a"}, {Alias: "a"}}} },
		"empty alias":     func(p *Profile) { p.Models = &Models{Aliases: []ModelAlias{{}}} },
		"duplicate tool":  func(p *Profile) { p.Tools = []ToolGrant{{Name: "shell"}, {Name: "shell"}} },
		"empty tool":      func(p *Profile) { p.Tools = []ToolGrant{{}} },
	}
	for name, mutate := range cases {
		p := base()
		mutate(p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := p.Proto(); err == nil {
			t.Errorf("%s: Proto accepted", name)
		}
	}
	// Every problem is reported at once.
	p := base()
	p.Schema, p.DefaultPauseMode = "v9", "sleep"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "schema") || !strings.Contains(err.Error(), "default_pause_mode") {
		t.Errorf("err = %v, want both problems", err)
	}
}

func TestParseProfileUpdate(t *testing.T) {
	u, err := ParseProfileUpdate([]byte(`
limits: {max_workers: 3}
budgets:
  - {kind: money, limit: 4, currency: EUR, on_exhaust: wait, wait_mode: idle}
reset_budgets: [money, tokens]
`))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(u.GetFields()) != "[budgets limits.max_workers]" || len(u.GetResetBudgets()) != 2 ||
		u.GetProfile().GetLimits().GetMaxWorkers() != 3 || u.GetProfile().GetBudgets()[0].GetLimit() != 4 {
		t.Errorf("update %v", u)
	}
	// A field a worker doesn't let change is passed on, for it to refuse.
	if u, err := ParseProfileUpdate([]byte(`instructions: be evil`)); err != nil || fmt.Sprint(u.GetFields()) != "[instructions]" {
		t.Errorf("instructions: %v, %v", u, err)
	}
	for _, bad := range []string{`{}`, `reset_budgets: [fun]`, `budgets: [{kind: money, limit: 1, on_exhaust: kill}]`, `nonsense: 1`} {
		if _, err := ParseProfileUpdate([]byte(bad)); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}
