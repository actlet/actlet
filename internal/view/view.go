// Package view folds a session's activity events into what a viewer shows:
// the session's state, its workers, and each worker's conversation as
// model turns, each with the assistant's text and its tool calls and their
// results. It is pure and deterministic, so a live viewer and a replay of
// the same events arrive at the same state.
//
// Text from events is untrusted (it comes from models, tools and files):
// terminal control sequences are removed as events are folded.
package view

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Bounds of what a view keeps.
const (
	// MaxItems bounds a timeline; older items are dropped and counted.
	MaxItems = 2000
	// MaxLog bounds the raw event log.
	MaxLog = 5000
	// MaxFeedback bounds the feedback kept.
	MaxFeedback = 200
)

// Session is the presentation state of a session.
type Session struct {
	ID      string
	Epoch   uint64
	State   gatewayv0.SessionState
	Reason  string
	Resumed bool
	// Profile is the profile ID from the latest start.
	Profile string
	// Events counts the events folded; Skipped the events known to be
	// missing (gaps, or events a viewer missed).
	Events, Skipped uint64
	// First and Last are the times of the first and latest events.
	First, Last time.Time
	// Info describes the run, from its session_info event; nil before it.
	Info *Info
	// Usage totals the model calls' tokens and cost.
	Usage Usage
	// Progress is the agent's latest progress report; nil before one.
	Progress *Progress
	// Feedback is what the agent sent, oldest first.
	Feedback        []Feedback
	FeedbackDropped int
	// Budgets are the budgets as the latest budget status reported them;
	// nil before one.
	Budgets []BudgetState
	// Items is the session's own timeline: its start and state changes,
	// commands, operator notes and messages.
	Items   []*Item
	Dropped int
	// Log is every event as one line, newest last.
	Log        []LogLine
	LogDropped int

	workers map[string]*Worker
	order   []string
}

// Worker is one worker of the session's agent: "root", or a subagent
// such as "root.1".
type Worker struct {
	ID     string
	Parent string
	// CallID is the parent's spawn_subagent call waiting for it.
	CallID string
	Model  string
	// Task is a subagent's subtask.
	Task   string
	Status WorkerStatus
	// Result is a finished subagent's final answer.
	Result string
	Turns  uint64
	// Context is the input tokens of the worker's latest model call: how
	// full its context is.
	Context uint64
	Usage   Usage
	// Items is the worker's timeline: its model turns and messages.
	Items   []*Item
	Dropped int

	last  *Turn
	calls map[string]*ToolCall
}

// Progress is a progress report of the agent's.
type Progress struct {
	Worker  string
	Message string
	// Percent is how far along it is, or -1 when it didn't say.
	Percent int
	Time    time.Time
}

// BudgetState is a budget as a budget status reported it.
type BudgetState struct {
	Kind        gatewayv0.BudgetKind
	Limit, Used float64
	Currency    string
	// State is "ok", "alerted", "exhausted" or "waiting".
	State string
}

// Feedback is feedback the agent sent.
type Feedback struct {
	Worker  string
	Kind    string
	Message string
	Time    time.Time
}

// WorkerStatus is how far a worker is.
type WorkerStatus int

// Worker statuses.
const (
	WorkerRunning WorkerStatus = iota
	WorkerFinished
	WorkerFailed
)

func (s WorkerStatus) String() string {
	switch s {
	case WorkerFinished:
		return "finished"
	case WorkerFailed:
		return "failed"
	}
	return "running"
}

// ItemKind is what a timeline item is.
type ItemKind int

// Item kinds.
const (
	ItemTurn   ItemKind = iota // a model turn and its tool calls
	ItemNote                   // an operator's note
	ItemSystem                 // a message from Actlet or about the session
)

// Item is one entry of a timeline.
type Item struct {
	Kind       ItemKind
	Epoch, Seq uint64
	Time       time.Time
	Turn       *Turn
	Text       string
	From       string // who sent a note
	Warn       bool   // a system item worth attention, such as a gap
}

// Turn is one model call: the assistant's answer and the tool calls it
// made.
type Turn struct {
	Number uint64
	Model  string
	Text   string
	Usage  Usage
	Stop   string
	Calls  []*ToolCall
	// Unseen marks calls whose model turn this viewer never saw.
	Unseen bool
}

// ToolCall is a tool call and its result.
type ToolCall struct {
	CallID string
	Name   string
	Input  string
	Status ToolStatus
	Output string
	Took   time.Duration
	Diffs  []Diff
	// Child is the subagent a spawn_subagent call started.
	Child      string
	Epoch, Seq uint64
}

// ToolStatus is how a tool call went.
type ToolStatus int

// Tool call statuses.
const (
	ToolRunning ToolStatus = iota
	ToolOK
	ToolError
	ToolUnknown // the worker stopped while it ran
)

func (s ToolStatus) String() string {
	switch s {
	case ToolOK:
		return "ok"
	case ToolError:
		return "error"
	case ToolUnknown:
		return "outcome unknown"
	}
	return "running"
}

// Diff is a file a tool call changed.
type Diff struct {
	Path    string
	Created bool
	Diff    string
}

// Stat counts a diff's added and removed lines.
func (d Diff) Stat() (added, removed int) {
	for _, l := range strings.Split(d.Diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			added++
		case strings.HasPrefix(l, "-"):
			removed++
		}
	}
	return added, removed
}

// Args decodes a tool call's JSON input; nil if it isn't an object.
func (c *ToolCall) Args() map[string]any {
	var m map[string]any
	if json.Unmarshal([]byte(c.Input), &m) != nil {
		return nil
	}
	return m
}

// LogLine is one event, described in a line.
type LogLine struct {
	Epoch, Seq uint64
	Time       time.Time
	Worker     string
	Text       string
}

// Info describes a run: its task, configuration and tools. Text is
// sanitized.
type Info struct {
	TaskID, TaskKind string
	// Task is task.json; Config the effective configuration as JSON.
	Task, Config  string
	Budgets       []*gatewayv0.Budget
	Models        *gatewayv0.ModelCatalog
	Tools         []*gatewayv0.ToolInfo
	Grants        []*gatewayv0.ToolGrant
	Manifest      string
	WorkerID      string
	Version       string
	LocalCommands []string
}

// Usage counts tokens, and what they cost when the gateway says.
type Usage struct {
	Input, Output, CacheRead, CacheWrite uint64
	Cost                                 float64
	Currency                             string
}

func (u *Usage) add(m *gatewayv0.Usage) {
	u.Input += m.GetInputTokens()
	u.Output += m.GetOutputTokens()
	u.CacheRead += m.GetCacheReadTokens()
	u.CacheWrite += m.GetCacheWriteTokens()
	u.Cost += m.GetCost()
	if c := m.GetCurrency(); c != "" {
		u.Currency = Sanitize(c)
	}
}

// New returns an empty session view.
func New() *Session { return &Session{workers: map[string]*Worker{}} }

// Workers returns the workers as a tree: each parent before its
// subagents.
func (s *Session) Workers() []*Worker {
	out := make([]*Worker, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.workers[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return workerLess(out[i].ID, out[j].ID) })
	return out
}

// workerLess orders worker IDs as a tree: root, root.1, root.1.1, root.2,
// root.10.
func workerLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] == pb[i] {
			continue
		}
		var na, nb int
		_, ea := fmt.Sscan(pa[i], &na)
		_, eb := fmt.Sscan(pb[i], &nb)
		if ea == nil && eb == nil {
			return na < nb
		}
		return pa[i] < pb[i]
	}
	return len(pa) < len(pb)
}

// Worker returns a worker by ID, or nil.
func (s *Session) Worker(id string) *Worker { return s.workers[id] }

func (s *Session) worker(id string) *Worker {
	w := s.workers[id]
	if w == nil {
		w = &Worker{ID: id, calls: map[string]*ToolCall{}}
		if i := strings.LastIndex(id, "."); i > 0 {
			w.Parent = id[:i]
		}
		s.workers[id] = w
		s.order = append(s.order, id)
	}
	return w
}

func addItem(items *[]*Item, dropped *int, it *Item) {
	*items = append(*items, it)
	if len(*items) > MaxItems {
		n := len(*items) - MaxItems
		*items = append([]*Item(nil), (*items)[n:]...)
		*dropped += n
	}
}

func (w *Worker) add(it *Item) { addItem(&w.Items, &w.Dropped, it) }

// turn returns the worker's latest turn, for its tool calls: one this
// viewer never saw if there is none.
func (w *Worker) turn(ev *gatewayv0.ActivityEvent) *Turn {
	if w.last == nil {
		w.last = &Turn{Unseen: true}
		w.add(&Item{Kind: ItemTurn, Epoch: ev.GetEpoch(), Seq: ev.GetSeq(), Time: ev.GetTime().AsTime(), Turn: w.last})
	}
	return w.last
}

// call returns a tool call of the worker, adding it to its latest turn if
// it is new.
func (w *Worker) call(ev *gatewayv0.ActivityEvent, id, name string) *ToolCall {
	c := w.calls[id]
	if c == nil {
		c = &ToolCall{CallID: Sanitize(id), Name: Sanitize(name), Epoch: ev.GetEpoch(), Seq: ev.GetSeq()}
		t := w.turn(ev)
		t.Calls = append(t.Calls, c)
		w.calls[id] = c
	}
	return c
}

// unclaimedSpawn returns the first spawn_subagent call of the latest turn
// that no subagent claims yet, or makes one.
func (w *Worker) unclaimedSpawn() *ToolCall {
	if w.last != nil {
		for _, c := range w.last.Calls {
			if c.Name == "spawn_subagent" && c.Child == "" {
				return c
			}
		}
	}
	return nil
}

// Skip records events a viewer missed.
func (s *Session) Skip(n uint64) {
	s.Skipped += n
	s.system(nil, fmt.Sprintf("%d events missed", n), true)
}

func (s *Session) system(ev *gatewayv0.ActivityEvent, text string, warn bool) {
	it := &Item{Kind: ItemSystem, Epoch: ev.GetEpoch(), Seq: ev.GetSeq(), Time: ev.GetTime().AsTime(), Text: Sanitize(text), Warn: warn}
	addItem(&s.Items, &s.Dropped, it)
}

func (s *Session) log(ev *gatewayv0.ActivityEvent, text string) {
	s.Log = append(s.Log, LogLine{Epoch: ev.GetEpoch(), Seq: ev.GetSeq(), Time: ev.GetTime().AsTime(), Worker: Sanitize(ev.GetWorker()), Text: Sanitize(text)})
	if len(s.Log) > MaxLog {
		n := len(s.Log) - MaxLog
		s.Log = append([]LogLine(nil), s.Log[n:]...)
		s.LogDropped += n
	}
}

// Apply folds one event into the view.
func (s *Session) Apply(ev *gatewayv0.ActivityEvent) {
	s.Events++
	at := ev.GetTime().AsTime()
	if ev.GetTime() != nil {
		if s.First.IsZero() {
			s.First = at
		}
		s.Last = at
	}
	if ev.GetEpoch() > s.Epoch {
		s.Epoch = ev.GetEpoch()
	}
	s.log(ev, Describe(ev))
	var w *Worker
	if id := ev.GetWorker(); id != "" {
		w = s.worker(id)
	}
	switch b := ev.GetBody().(type) {
	case *gatewayv0.ActivityEvent_Started:
		s.Resumed = b.Started.GetResumed()
		s.Profile = Sanitize(b.Started.GetProfileId())
		text := fmt.Sprintf("run %d started", b.Started.GetEpoch())
		if b.Started.GetResumed() {
			text += ", resuming the session"
		}
		s.system(ev, text, false)
	case *gatewayv0.ActivityEvent_State:
		s.State = b.State.GetTo()
		s.Reason = Sanitize(b.State.GetReason())
		if b.State.GetFrom() != gatewayv0.SessionState_SESSION_STATE_UNSPECIFIED {
			text := "session " + StateName(b.State.GetTo())
			if r := b.State.GetReason(); r != "" {
				text += ": " + r
			}
			s.system(ev, text, b.State.GetTo() == gatewayv0.SessionState_SESSION_STATE_FAILED)
		}
	case *gatewayv0.ActivityEvent_Command:
		c := b.Command
		if c.GetCommand().GetSteer() != nil && c.GetResult() == gatewayv0.CommandResult_COMMAND_RESULT_ACCEPTED {
			break // the note itself shows
		}
		result := strings.ToLower(strings.TrimPrefix(c.GetResult().String(), "COMMAND_RESULT_"))
		text := fmt.Sprintf("%s from %s %s", commandName(c.GetCommand()), originName(c.GetCommand().GetOrigin()), result)
		if c.GetMessage() != "" {
			text += ": " + c.GetMessage()
		}
		s.system(ev, text, c.GetResult() == gatewayv0.CommandResult_COMMAND_RESULT_REJECTED)
	case *gatewayv0.ActivityEvent_Steer:
		who := b.Steer.GetActor()
		if who == "" {
			who = originName(b.Steer.GetOrigin())
		}
		addItem(&s.Items, &s.Dropped, &Item{Kind: ItemNote, Epoch: ev.GetEpoch(), Seq: ev.GetSeq(), Time: at,
			Text: Sanitize(b.Steer.GetNote()), From: Sanitize(who)})
	case *gatewayv0.ActivityEvent_Text:
		if w != nil {
			w.add(&Item{Kind: ItemSystem, Epoch: ev.GetEpoch(), Seq: ev.GetSeq(), Time: at, Text: Sanitize(b.Text.GetText())})
		} else {
			s.system(ev, b.Text.GetText(), false)
		}
	case *gatewayv0.ActivityEvent_Gap:
		s.Skipped += b.Gap.GetDropped()
		s.system(ev, fmt.Sprintf("%d events dropped by the worker", b.Gap.GetDropped()), true)
	case *gatewayv0.ActivityEvent_ModelTurn:
		if w == nil {
			break
		}
		m := b.ModelTurn
		w.Turns = max(w.Turns, m.GetTurn())
		if m.GetModel() != "" {
			w.Model = Sanitize(m.GetModel())
		}
		w.Context = m.GetUsage().GetInputTokens()
		w.Usage.add(m.GetUsage())
		s.Usage.add(m.GetUsage())
		t := &Turn{Number: m.GetTurn(), Model: Sanitize(m.GetModel()), Text: Sanitize(m.GetText()),
			Stop: strings.ToLower(strings.TrimPrefix(m.GetStopReason().String(), "STOP_REASON_"))}
		t.Usage.add(m.GetUsage())
		w.last = t
		w.add(&Item{Kind: ItemTurn, Epoch: ev.GetEpoch(), Seq: ev.GetSeq(), Time: at, Turn: t})
	case *gatewayv0.ActivityEvent_ToolStarted:
		if w == nil {
			break
		}
		t := b.ToolStarted
		if c := w.calls[t.GetCallId()]; c != nil && c.Status != ToolRunning {
			delete(w.calls, t.GetCallId()) // run again after recovery: a new call
		}
		c := w.call(ev, t.GetCallId(), t.GetTool())
		c.Input = Sanitize(t.GetInput())
	case *gatewayv0.ActivityEvent_ToolFinished:
		if w == nil {
			break
		}
		f := b.ToolFinished
		c := w.call(ev, f.GetCallId(), f.GetTool())
		c.Output = Sanitize(f.GetOutput())
		c.Took = f.GetTook().AsDuration()
		w.Usage.add(f.GetUsage())
		s.Usage.add(f.GetUsage())
		switch {
		case f.GetOutcomeUnknown():
			c.Status = ToolUnknown
		case f.GetIsError():
			c.Status = ToolError
		default:
			c.Status = ToolOK
		}
	case *gatewayv0.ActivityEvent_Progress:
		p := &Progress{Worker: Sanitize(ev.GetWorker()), Message: Sanitize(b.Progress.GetMessage()), Percent: -1, Time: at}
		if b.Progress.Percent != nil {
			p.Percent = int(min(b.Progress.GetPercent(), 100))
		}
		s.Progress = p
	case *gatewayv0.ActivityEvent_BudgetStatus:
		s.Budgets = nil
		for _, u := range b.BudgetStatus.GetBudgets() {
			s.Budgets = append(s.Budgets, BudgetState{Kind: u.GetKind(), Limit: u.GetLimit(), Used: u.GetUsed(),
				Currency: Sanitize(u.GetCurrency()), State: Sanitize(u.GetState())})
		}
	case *gatewayv0.ActivityEvent_Feedback:
		s.Feedback = append(s.Feedback, Feedback{Worker: Sanitize(ev.GetWorker()), Kind: Sanitize(b.Feedback.GetKind()),
			Message: Sanitize(b.Feedback.GetMessage()), Time: at})
		if n := len(s.Feedback) - MaxFeedback; n > 0 {
			s.Feedback = append([]Feedback(nil), s.Feedback[n:]...)
			s.FeedbackDropped += n
		}
	case *gatewayv0.ActivityEvent_FileChange:
		if w == nil {
			break
		}
		f := b.FileChange
		c := w.call(ev, f.GetCallId(), "file change")
		c.Diffs = append(c.Diffs, Diff{Path: Sanitize(f.GetPath()), Created: f.GetCreated(), Diff: Sanitize(f.GetDiff())})
	case *gatewayv0.ActivityEvent_SubagentSpawned:
		if w == nil {
			break
		}
		sp := b.SubagentSpawned
		w.Parent, w.Task, w.Model = Sanitize(sp.GetParent()), Sanitize(sp.GetTask()), Sanitize(sp.GetModel())
		w.CallID, w.Status = Sanitize(sp.GetCallId()), WorkerRunning
		if parent := sp.GetParent(); parent != "" {
			var c *ToolCall
			if sp.GetCallId() != "" {
				c = s.worker(parent).call(ev, sp.GetCallId(), "spawn_subagent")
			} else {
				c = s.worker(parent).unclaimedSpawn() // an older recording
			}
			if c != nil {
				c.Child = w.ID
				if c.Input == "" {
					in, _ := json.Marshal(map[string]string{"task": sp.GetTask()})
					c.Input = Sanitize(string(in))
				}
			} else {
				// No call to nest it under: show it where it is.
				w.Parent = ""
			}
		}
	case *gatewayv0.ActivityEvent_SubagentFinished:
		if w == nil {
			break
		}
		f := b.SubagentFinished
		w.Status = WorkerFinished
		if f.GetIsError() {
			w.Status = WorkerFailed
		}
		w.Result = Sanitize(f.GetResult())
		w.Turns = max(w.Turns, f.GetTurns())
	case *gatewayv0.ActivityEvent_SessionInfo:
		i := b.SessionInfo
		info := &Info{
			TaskID: Sanitize(i.GetTaskId()), TaskKind: Sanitize(i.GetTaskKind()), Task: Sanitize(i.GetTask()),
			Config: Sanitize(i.GetConfig()), Budgets: i.GetBudgets(), Models: i.GetModels(), Tools: i.GetTools(),
			Grants: i.GetGrants(), Manifest: Sanitize(i.GetToolsManifest()), WorkerID: Sanitize(i.GetWorkerId()),
			Version: Sanitize(i.GetActletVersion()),
		}
		for _, c := range i.GetLocalCommands() {
			info.LocalCommands = append(info.LocalCommands, Sanitize(c))
		}
		s.Info = info
	}
	// The session's end ends its workers too.
	if s.State == gatewayv0.SessionState_SESSION_STATE_COMPLETED {
		for _, w := range s.workers {
			if w.Status == WorkerRunning {
				w.Status = WorkerFinished
			}
		}
	}
}

// Models returns the models the workers use, sorted.
func (s *Session) Models() []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range s.workers {
		if w.Model != "" && !seen[w.Model] {
			seen[w.Model] = true
			out = append(out, w.Model)
		}
	}
	sort.Strings(out)
	return out
}

// Running returns the latest tool call still running and its worker, or
// nil, and how many calls are running.
func (s *Session) Running() (*Worker, *ToolCall, int) {
	var bw *Worker
	var bc *ToolCall
	n := 0
	for _, id := range s.order {
		w := s.workers[id]
		for _, c := range w.calls {
			if c.Status != ToolRunning {
				continue
			}
			n++
			if bc == nil || c.Seq > bc.Seq {
				bw, bc = w, c
			}
		}
	}
	return bw, bc, n
}

// File is a file changed in the session.
type File struct {
	// Path is relative to the session folder, such as
	// "workspace/totals.py" or "output/report.md".
	Path string
	// Added and Removed count the lines of all its changes together.
	Added, Removed int
	// Changes counts its changes.
	Changes int
}

// SessionPath turns a path as tools show it, relative to workspace/, into
// one relative to the session folder. Absolute paths are kept.
func SessionPath(p string) string {
	if path.IsAbs(p) || filepath.IsAbs(p) {
		return p
	}
	return path.Clean(path.Join("workspace", filepath.ToSlash(p)))
}

// ToolPath turns a path relative to the session folder back into one
// relative to workspace/, as tools take it.
func ToolPath(p string) string {
	if path.IsAbs(p) || filepath.IsAbs(p) {
		return p
	}
	if rest, ok := strings.CutPrefix(p, "workspace/"); ok {
		return rest
	}
	return "../" + p
}

// Files returns the files changed so far, in the order they were first
// changed.
func (s *Session) Files() []File {
	at := map[string]int{}
	var out []File
	s.eachDiff(func(d Diff) {
		p := SessionPath(d.Path)
		i, ok := at[p]
		if !ok {
			i = len(out)
			at[p] = i
			out = append(out, File{Path: p})
		}
		add, del := d.Stat()
		out[i].Added += add
		out[i].Removed += del
		out[i].Changes++
	})
	return out
}

// FileHistory returns the changes of a file, given relative to the
// session folder, in order.
func (s *Session) FileHistory(p string) []Diff {
	var out []Diff
	s.eachDiff(func(d Diff) {
		if SessionPath(d.Path) == p {
			out = append(out, d)
		}
	})
	return out
}

// eachDiff calls fn with every file change, in the order the tool calls
// started.
func (s *Session) eachDiff(fn func(Diff)) {
	var calls []*ToolCall
	for _, w := range s.Workers() {
		for _, it := range w.Items {
			if it.Turn != nil {
				calls = append(calls, it.Turn.Calls...)
			}
		}
	}
	sort.SliceStable(calls, func(i, j int) bool {
		a, b := calls[i], calls[j]
		return a.Epoch < b.Epoch || a.Epoch == b.Epoch && a.Seq < b.Seq
	})
	for _, c := range calls {
		for _, d := range c.Diffs {
			fn(d)
		}
	}
}

// ContextWindow returns a model alias's context window from the
// profile's catalogue, or 0 if it isn't known.
func (s *Session) ContextWindow(model string) uint64 {
	if s.Info == nil {
		return 0
	}
	for _, a := range s.Info.Models.GetAliases() {
		if a.GetAlias() == model {
			return a.GetContextWindow()
		}
	}
	return 0
}

// BudgetName is a budget kind in lower case, such as "money".
func BudgetName(k gatewayv0.BudgetKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "BUDGET_KIND_"))
}

// StateName is a session state in lower case, such as "running".
func StateName(s gatewayv0.SessionState) string {
	if s == gatewayv0.SessionState_SESSION_STATE_UNSPECIFIED {
		return "starting"
	}
	return strings.ToLower(strings.TrimPrefix(s.String(), "SESSION_STATE_"))
}

func originName(o gatewayv0.Origin) string {
	switch o {
	case gatewayv0.Origin_ORIGIN_LOCAL:
		return "a local operator"
	case gatewayv0.Origin_ORIGIN_REMOTE:
		return "a remote operator"
	case gatewayv0.Origin_ORIGIN_ORCHESTRATOR:
		return "the orchestrator"
	}
	return "someone"
}

func commandName(c *gatewayv0.Command) string {
	switch a := c.GetAction().(type) {
	case *gatewayv0.Command_Pause:
		return "pause (" + strings.ToLower(strings.TrimPrefix(a.Pause.GetMode().String(), "PAUSE_MODE_")) + ")"
	case *gatewayv0.Command_Stop:
		return "stop"
	case *gatewayv0.Command_Steer:
		return "note"
	}
	return "command"
}

// Describe describes an event in one line, for the raw event log.
func Describe(ev *gatewayv0.ActivityEvent) string {
	switch b := ev.GetBody().(type) {
	case *gatewayv0.ActivityEvent_Started:
		return fmt.Sprintf("started: run %d, resumed %v", b.Started.GetEpoch(), b.Started.GetResumed())
	case *gatewayv0.ActivityEvent_State:
		return fmt.Sprintf("state: %s → %s (%s)", StateName(b.State.GetFrom()), StateName(b.State.GetTo()), b.State.GetReason())
	case *gatewayv0.ActivityEvent_Command:
		return fmt.Sprintf("command %s: %s %s", b.Command.GetCommand().GetId(), commandName(b.Command.GetCommand()),
			strings.ToLower(strings.TrimPrefix(b.Command.GetResult().String(), "COMMAND_RESULT_")))
	case *gatewayv0.ActivityEvent_Steer:
		return "steer: " + b.Steer.GetNote()
	case *gatewayv0.ActivityEvent_Text:
		return "text: " + b.Text.GetText()
	case *gatewayv0.ActivityEvent_Gap:
		return fmt.Sprintf("gap: %d dropped", b.Gap.GetDropped())
	case *gatewayv0.ActivityEvent_ModelTurn:
		return fmt.Sprintf("model_turn %d (%s): %s → %s", b.ModelTurn.GetTurn(), b.ModelTurn.GetModel(), firstLine(b.ModelTurn.GetText()), strings.Join(b.ModelTurn.GetToolCalls(), ", "))
	case *gatewayv0.ActivityEvent_ToolStarted:
		return fmt.Sprintf("tool_started %s %s: %s", b.ToolStarted.GetTool(), b.ToolStarted.GetCallId(), b.ToolStarted.GetInput())
	case *gatewayv0.ActivityEvent_ToolFinished:
		f := b.ToolFinished
		return fmt.Sprintf("tool_finished %s %s error=%v unknown=%v: %s", f.GetTool(), f.GetCallId(), f.GetIsError(), f.GetOutcomeUnknown(), firstLine(f.GetOutput()))
	case *gatewayv0.ActivityEvent_FileChange:
		return fmt.Sprintf("file_change %s (call %s)", b.FileChange.GetPath(), b.FileChange.GetCallId())
	case *gatewayv0.ActivityEvent_SubagentSpawned:
		return fmt.Sprintf("subagent_spawned by %s (call %s): %s", b.SubagentSpawned.GetParent(), b.SubagentSpawned.GetCallId(), firstLine(b.SubagentSpawned.GetTask()))
	case *gatewayv0.ActivityEvent_SubagentFinished:
		return fmt.Sprintf("subagent_finished error=%v: %s", b.SubagentFinished.GetIsError(), firstLine(b.SubagentFinished.GetResult()))
	case *gatewayv0.ActivityEvent_SessionInfo:
		return "session_info: task " + b.SessionInfo.GetTaskId()
	case *gatewayv0.ActivityEvent_Progress:
		if b.Progress.Percent != nil {
			return fmt.Sprintf("progress %d%%: %s", b.Progress.GetPercent(), firstLine(b.Progress.GetMessage()))
		}
		return "progress: " + firstLine(b.Progress.GetMessage())
	case *gatewayv0.ActivityEvent_Feedback:
		return fmt.Sprintf("feedback (%s): %s", b.Feedback.GetKind(), firstLine(b.Feedback.GetMessage()))
	case *gatewayv0.ActivityEvent_BudgetStatus:
		var parts []string
		for _, u := range b.BudgetStatus.GetBudgets() {
			parts = append(parts, fmt.Sprintf("%s %g/%g %s", BudgetName(u.GetKind()), u.GetUsed(), u.GetLimit(), u.GetState()))
		}
		return fmt.Sprintf("budget_status (%s): %s", b.BudgetStatus.GetReason(), strings.Join(parts, ", "))
	}
	return "an event this viewer doesn't know"
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
