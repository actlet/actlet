package view

import (
	"bytes"
	"encoding/json"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// StateVersion is the version of the view state's JSON form.
const StateVersion = 1

// State is a session's view as JSON, in the form the web viewer's
// toViewState writes (version 1, snake_case), so that the Go and
// TypeScript folds of the same events can be compared.
type State struct {
	Version         int             `json:"version"`
	ID              string          `json:"id"`
	Epoch           uint64          `json:"epoch"`
	State           string          `json:"state"`
	Reason          string          `json:"reason"`
	Resumed         bool            `json:"resumed"`
	Profile         string          `json:"profile"`
	Events          uint64          `json:"events"`
	Skipped         uint64          `json:"skipped"`
	First           *string         `json:"first"`
	Last            *string         `json:"last"`
	Info            *InfoState      `json:"info"`
	Usage           UsageState      `json:"usage"`
	Progress        *ProgressState  `json:"progress"`
	Feedback        []FeedbackState `json:"feedback"`
	FeedbackDropped int             `json:"feedback_dropped"`
	Budgets         []BudgetJSON    `json:"budgets"`
	Items           []ItemState     `json:"items"`
	Dropped         int             `json:"dropped"`
	Workers         []WorkerState   `json:"workers"`
	Files           []FileState     `json:"files"`
	Models          []string        `json:"models"`
	Log             []LogLineState  `json:"log"`
	LogDropped      int             `json:"log_dropped"`
}

// InfoState is the run's description.
type InfoState struct {
	TaskID        string            `json:"task_id"`
	TaskKind      string            `json:"task_kind"`
	Task          string            `json:"task"`
	Config        string            `json:"config"`
	Budgets       []json.RawMessage `json:"budgets"`
	Models        json.RawMessage   `json:"models"`
	Tools         []json.RawMessage `json:"tools"`
	Grants        []json.RawMessage `json:"grants"`
	Manifest      string            `json:"manifest"`
	WorkerID      string            `json:"worker_id"`
	Version       string            `json:"version"`
	LocalCommands []string          `json:"local_commands"`
	Skills        []json.RawMessage `json:"skills"`
}

// UsageState is tokens and cost.
type UsageState struct {
	Input      uint64  `json:"input"`
	Output     uint64  `json:"output"`
	CacheRead  uint64  `json:"cache_read"`
	CacheWrite uint64  `json:"cache_write"`
	Cost       float64 `json:"cost"`
	Currency   string  `json:"currency"`
}

// ProgressState is a progress report.
type ProgressState struct {
	Worker  string `json:"worker"`
	Message string `json:"message"`
	Percent int    `json:"percent"`
	Time    string `json:"time"`
}

// FeedbackState is feedback the agent sent.
type FeedbackState struct {
	Worker  string `json:"worker"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Time    string `json:"time"`
}

// BudgetJSON is a budget as the latest status reported it.
type BudgetJSON struct {
	Kind     string  `json:"kind"`
	Limit    float64 `json:"limit"`
	Used     float64 `json:"used"`
	Currency string  `json:"currency"`
	State    string  `json:"state"`
}

// ItemState is a timeline item.
type ItemState struct {
	Kind  string     `json:"kind"`
	Epoch uint64     `json:"epoch"`
	Seq   uint64     `json:"seq"`
	Time  string     `json:"time"`
	Turn  *TurnState `json:"turn"`
	Text  string     `json:"text"`
	From  string     `json:"from"`
	Warn  bool       `json:"warn"`
}

// TurnState is a model turn and its tool calls.
type TurnState struct {
	Number uint64      `json:"number"`
	Model  string      `json:"model"`
	Text   string      `json:"text"`
	Usage  UsageState  `json:"usage"`
	Stop   string      `json:"stop"`
	Unseen bool        `json:"unseen"`
	Calls  []CallState `json:"calls"`
}

// CallState is a tool call.
type CallState struct {
	CallID string      `json:"call_id"`
	Name   string      `json:"name"`
	Input  string      `json:"input"`
	Status string      `json:"status"`
	Output string      `json:"output"`
	TookNS int64       `json:"took_ns"`
	Diffs  []DiffState `json:"diffs"`
	Child  string      `json:"child"`
	Epoch  uint64      `json:"epoch"`
	Seq    uint64      `json:"seq"`
}

// DiffState is a file change.
type DiffState struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
	Diff    string `json:"diff"`
}

// WorkerState is a worker.
type WorkerState struct {
	ID      string      `json:"id"`
	Parent  string      `json:"parent"`
	CallID  string      `json:"call_id"`
	Model   string      `json:"model"`
	Task    string      `json:"task"`
	Status  string      `json:"status"`
	Result  string      `json:"result"`
	Turns   uint64      `json:"turns"`
	Context uint64      `json:"context"`
	Usage   UsageState  `json:"usage"`
	Items   []ItemState `json:"items"`
	Dropped int         `json:"dropped"`
}

// FileState is a changed file.
type FileState struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Changes int    `json:"changes"`
}

// LogLineState is a line of the event log.
type LogLineState struct {
	Epoch  uint64 `json:"epoch"`
	Seq    uint64 `json:"seq"`
	Time   string `json:"time"`
	Worker string `json:"worker"`
	Text   string `json:"text"`
}

// stateTime writes a time as the web viewer does: RFC 3339 in UTC with
// nanoseconds, trailing zeros trimmed.
func stateTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// protoJSON writes a message as protobuf JSON with its fields' proto
// names, compacted: protojson varies its spacing from build to build.
func protoJSON(m proto.Message) json.RawMessage {
	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	var out bytes.Buffer
	if err != nil || json.Compact(&out, data) != nil {
		return json.RawMessage("null")
	}
	return out.Bytes()
}

func usageState(u Usage) UsageState {
	return UsageState(u)
}

func itemKind(k ItemKind) string {
	switch k {
	case ItemTurn:
		return "turn"
	case ItemNote:
		return "note"
	}
	return "system"
}

func callStatus(s ToolStatus) string {
	switch s {
	case ToolOK:
		return "ok"
	case ToolError:
		return "error"
	case ToolUnknown:
		return "unknown"
	}
	return "running"
}

func itemStates(items []*Item) []ItemState {
	out := make([]ItemState, 0, len(items))
	for _, it := range items {
		is := ItemState{Kind: itemKind(it.Kind), Epoch: it.Epoch, Seq: it.Seq, Time: stateTime(it.Time), Text: it.Text, From: it.From, Warn: it.Warn}
		if t := it.Turn; t != nil && it.Kind == ItemTurn {
			ts := &TurnState{Number: t.Number, Model: t.Model, Text: t.Text, Usage: usageState(t.Usage), Stop: t.Stop, Unseen: t.Unseen, Calls: []CallState{}}
			for _, c := range t.Calls {
				cs := CallState{CallID: c.CallID, Name: c.Name, Input: c.Input, Status: callStatus(c.Status), Output: c.Output,
					TookNS: int64(c.Took), Diffs: []DiffState{}, Child: c.Child, Epoch: c.Epoch, Seq: c.Seq}
				for _, d := range c.Diffs {
					cs.Diffs = append(cs.Diffs, DiffState(d))
				}
				ts.Calls = append(ts.Calls, cs)
			}
			is.Turn = ts
		}
		out = append(out, is)
	}
	return out
}

// ViewState returns the view in the web viewer's JSON form.
func (s *Session) ViewState() *State {
	st := &State{
		Version: StateVersion, ID: s.ID, Epoch: s.Epoch, State: s.State.String(), Reason: s.Reason, Resumed: s.Resumed,
		Profile: s.Profile, Events: s.Events, Skipped: s.Skipped, Usage: usageState(s.Usage),
		Feedback: []FeedbackState{}, FeedbackDropped: s.FeedbackDropped, Items: itemStates(s.Items), Dropped: s.Dropped,
		Workers: []WorkerState{}, Files: []FileState{}, Models: s.Models(), Log: []LogLineState{}, LogDropped: s.LogDropped,
	}
	if st.Models == nil {
		st.Models = []string{}
	}
	if !s.First.IsZero() {
		first, last := stateTime(s.First), stateTime(s.Last)
		st.First, st.Last = &first, &last
	}
	if i := s.Info; i != nil {
		is := &InfoState{TaskID: i.TaskID, TaskKind: i.TaskKind, Task: i.Task, Config: i.Config, Manifest: i.Manifest,
			WorkerID: i.WorkerID, Version: i.Version, LocalCommands: i.LocalCommands,
			Budgets: []json.RawMessage{}, Models: json.RawMessage("null"), Tools: []json.RawMessage{}, Grants: []json.RawMessage{},
			Skills: []json.RawMessage{}}
		if is.LocalCommands == nil {
			is.LocalCommands = []string{}
		}
		for _, b := range i.Budgets {
			is.Budgets = append(is.Budgets, protoJSON(b))
		}
		if i.Models != nil {
			is.Models = protoJSON(i.Models)
		}
		for _, t := range i.Tools {
			is.Tools = append(is.Tools, protoJSON(t))
		}
		for _, g := range i.Grants {
			is.Grants = append(is.Grants, protoJSON(g))
		}
		for _, sk := range i.Skills {
			is.Skills = append(is.Skills, protoJSON(sk))
		}
		st.Info = is
	}
	if p := s.Progress; p != nil {
		st.Progress = &ProgressState{Worker: p.Worker, Message: p.Message, Percent: p.Percent, Time: stateTime(p.Time)}
	}
	for _, f := range s.Feedback {
		st.Feedback = append(st.Feedback, FeedbackState{Worker: f.Worker, Kind: f.Kind, Message: f.Message, Time: stateTime(f.Time)})
	}
	if s.Budgets != nil {
		st.Budgets = []BudgetJSON{}
		for _, b := range s.Budgets {
			st.Budgets = append(st.Budgets, BudgetJSON{Kind: b.Kind.String(), Limit: b.Limit, Used: b.Used, Currency: b.Currency, State: b.State})
		}
	}
	for _, w := range s.Workers() {
		st.Workers = append(st.Workers, WorkerState{ID: w.ID, Parent: w.Parent, CallID: w.CallID, Model: w.Model, Task: w.Task,
			Status: w.Status.String(), Result: w.Result, Turns: w.Turns, Context: w.Context, Usage: usageState(w.Usage),
			Items: itemStates(w.Items), Dropped: w.Dropped})
	}
	for _, f := range s.Files() {
		st.Files = append(st.Files, FileState(f))
	}
	for _, l := range s.Log {
		st.Log = append(st.Log, LogLineState{Epoch: l.Epoch, Seq: l.Seq, Time: stateTime(l.Time), Worker: l.Worker, Text: l.Text})
	}
	return st
}
