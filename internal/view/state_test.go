package view

import (
	"encoding/json"
	"strings"
	"testing"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// The view state names what the web viewer names, and writes empty lists
// as lists and what isn't known yet as null.
func TestViewState(t *testing.T) {
	data, err := json.Marshal(New().ViewState())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"version":1`, `"first":null`, `"info":null`, `"progress":null`, `"budgets":null`,
		`"items":[]`, `"workers":[]`, `"files":[]`, `"models":[]`, `"log":[]`, `"feedback":[]`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("an empty view lacks %s: %s", want, data)
		}
	}

	evs := subagentRun()
	var e events
	e.seq = uint64(len(evs))
	e.add("root", &gatewayv0.ToolFinished{CallId: "z", Tool: "shell", OutcomeUnknown: true, IsError: true})
	st := fold(append(evs, e.out...)).ViewState()
	if st.State != "SESSION_STATE_COMPLETED" || st.Profile != "coding" || *st.First != "2023-11-14T22:13:21Z" ||
		st.Info == nil || string(st.Info.Tools[0]) != `{"name":"shell","policy":"never"}` || len(st.Info.Budgets) != 0 {
		t.Fatalf("state %+v, info %+v", st, st.Info)
	}
	if len(st.Workers) != 2 || st.Workers[1].ID != "root.1" || st.Workers[1].Status != "finished" || st.Workers[1].CallID != "s1" {
		t.Errorf("workers %+v", st.Workers)
	}
	if len(st.Files) != 1 || st.Files[0].Path != "workspace/a" || strings.Join(st.Models, ",") != "fast,large" {
		t.Errorf("files %+v, models %v", st.Files, st.Models)
	}
	sub := st.Workers[1].Items[0].Turn
	if c := sub.Calls[0]; c.Status != "ok" || c.TookNS != 1e6 || len(c.Diffs) != 1 || !c.Diffs[0].Created {
		t.Errorf("subagent's call %+v", c)
	}
	root := st.Workers[0].Items
	last := root[len(root)-1].Turn.Calls
	if root[0].Kind != "turn" || last[len(last)-1].Status != "unknown" {
		t.Errorf("root's items %+v", root)
	}
}
