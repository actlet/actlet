// Package tui is the terminal viewer of a session: a Bubble Tea program
// over the session-view model, for a running box (attached locally) or a
// recording played back. It is only set up when a viewer is asked for;
// headless runs never start it.
package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/actlet/actlet/internal/search"
	"github.com/actlet/actlet/internal/view"
	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Tabs of the viewer.
const (
	TabChat = iota
	TabSession
	TabTask
	TabProfile
	TabTools
	TabEvents
	tabCount
)

var tabNames = []string{"Chat", "Session", "Task", "Profile", "Tools", "Events"}

// TabByName maps tab names in lower case to tabs.
var TabByName = map[string]int{"chat": TabChat, "session": TabSession, "task": TabTask, "profile": TabProfile, "tools": TabTools, "events": TabEvents}

// Connection says what the viewer shows.
type Connection struct {
	// Kind is "local box", "this process" or "recording".
	Kind string
	// Target is the socket or file.
	Target string
	// Dir is the session folder of a box on this machine, to read its
	// files from once the box has gone.
	Dir string
}

// Commander sends a command to the box; nil when there is none, as in a
// replay.
type Commander func(req *attachv0.CommandRequest) (*gatewayv0.CommandAck, error)

// Model is the viewer's state.
type Model struct {
	sess *view.Session
	conn Connection
	// Live only.
	info  *attachv0.InfoResponse
	send  Commander
	lost  error // the connection to the box ended
	ended bool  // the run ended
	actor string
	// Replay only.
	player *Player

	now    func() time.Time
	frame  int // spinner frame
	tab    int
	worker string // "" shows the agent with its subagents nested
	tool   string // "" shows every tool
	scroll int    // lines up from the end
	width  int
	height int

	// Tool call blocks on the chat screen, selected and expanded ones.
	blocks       []string
	selected     string
	expanded     map[string]bool
	expandAll    bool
	collapseSubs bool

	// Where tabs and chat lines are on screen, for the mouse.
	tabSpans [][2]int
	rows     []string

	// The changed files in the sidebar: focused with o, opened in a
	// popup; and where they are on screen.
	filesFocus bool
	fileSel    int
	fileRows   map[int]string
	sideX      int
	popup      *popup
	readFile   FileReader

	// separators draws a line between turns and tool calls.
	separators bool

	input  textinput.Model
	typing bool
	help   bool
	notice string

	// searching is typing a search of the recording; query is the last
	// one, which n and N find again.
	searching bool
	query     string
}

// NewLive returns a viewer of a running box.
func NewLive(conn Connection, send Commander, actor string) *Model {
	m := newModel(conn)
	m.send, m.actor = send, actor
	return m
}

// NewReplay returns a viewer of a recording.
func NewReplay(p *Player) *Model {
	m := newModel(Connection{Kind: "recording", Target: p.Path()})
	m.player = p
	m.sess = p.View()
	return m
}

func newModel(conn Connection) *Model {
	in := textinput.New()
	in.Prompt = " › "
	in.Placeholder = "a note for the agent; Enter sends, Esc cancels"
	in.CharLimit = 4096
	return &Model{sess: view.New(), conn: conn, input: in, now: time.Now, width: 100, height: 30, expanded: map[string]bool{}, fileRows: map[int]string{}}
}

// Session returns the view being shown.
func (m *Model) Session() *view.Session { return m.sess }

// SetSize sets the screen size, for rendering without a terminal.
func (m *Model) SetSize(w, h int) { m.width, m.height = w, h }

// SetTab chooses the tab shown.
func (m *Model) SetTab(tab int) { m.tab = tab % tabCount }

// SetFilter shows one worker or one tool only; "" shows all.
func (m *Model) SetFilter(worker, tool string) { m.worker, m.tool = worker, tool }

// SetConnection says what the viewer shows.
func (m *Model) SetConnection(c Connection) { m.conn = c }

// SetClock replaces the clock, for reproducible renders.
func (m *Model) SetClock(now func() time.Time) { m.now = now }

// SetSeparators draws lines between turns and tool calls.
func (m *Model) SetSeparators(on bool) { m.separators = on }

// ExpandAll expands every tool call block.
func (m *Model) ExpandAll(on bool) { m.expandAll = on }

// Messages the viewer receives.
type (
	// EventsMsg are activity messages from the box, in order.
	EventsMsg []*attachv0.WatchResponse
	// InfoMsg is the box's description and figures.
	InfoMsg struct{ Info *attachv0.InfoResponse }
	// LostMsg ends a live view: the run ended (nil) or the connection
	// broke.
	LostMsg struct{ Err error }
	// noticeMsg shows a line in the status bar.
	noticeMsg string
	// tickMsg advances a replay and the spinner.
	tickMsg time.Time
)

// Apply folds activity messages into the view.
func (m *Model) Apply(msgs []*attachv0.WatchResponse) {
	for _, r := range msgs {
		if ev := r.GetEvent(); ev != nil {
			m.sess.Apply(ev)
		} else if s := r.GetSkipped(); s != nil {
			m.sess.Skip(s.GetCount())
		}
	}
}

func tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd { return tick() }

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case EventsMsg:
		m.Apply(msg)
	case InfoMsg:
		m.info = msg.Info
		if id := msg.Info.GetSessionId(); id != "" {
			m.sess.ID = id
		}
	case LostMsg:
		if msg.Err == nil {
			m.ended = true
			m.notice = "the run ended; q quits"
		} else {
			m.lost = msg.Err
			m.notice = "lost the box: " + msg.Err.Error()
		}
	case noticeMsg:
		m.notice = string(msg)
	case fileMsg:
		m.showFile(msg)
	case tickMsg:
		m.frame++
		if m.player != nil && m.player.Advance(time.Time(msg)) {
			m.sess = m.player.View()
		}
		return m, tick()
	case tea.KeyPressMsg:
		return m, m.key(msg)
	case tea.MouseClickMsg:
		return m, m.click(msg.Mouse())
	case tea.MouseWheelMsg:
		switch msg.Mouse().Button {
		case tea.MouseWheelUp:
			if m.popup != nil {
				m.popup.scroll = max(m.popup.scroll-3, 0)
			} else {
				m.scroll += 3
				m.selected = ""
			}
		case tea.MouseWheelDown:
			if m.popup != nil {
				m.popup.scroll += 3
			} else {
				m.scroll = max(m.scroll-3, 0)
				m.selected = ""
			}
		}
	}
	if m.typing {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// click handles a mouse click: on a tab, or on a tool call block, which a
// second click expands or collapses.
func (m *Model) click(c tea.Mouse) tea.Cmd {
	if c.Button != tea.MouseLeft {
		return nil
	}
	if m.popup != nil {
		m.popup = nil // a click closes it
		return nil
	}
	if p, ok := m.fileRows[c.Y]; ok && m.tab == TabChat && c.X >= m.sideX {
		return m.openFile(p)
	}
	if c.Y == 0 {
		for i, span := range m.tabSpans {
			if c.X >= span[0] && c.X < span[1] {
				m.tab, m.scroll = i, 0
			}
		}
		return nil
	}
	if row := c.Y - 1; m.tab == TabChat && row >= 0 && row < len(m.rows) {
		if b := m.rows[row]; b != "" {
			if m.selected == b {
				m.expanded[b] = !m.expanded[b]
			}
			m.selected = b
		}
	}
	return nil
}

// key handles a key press.
func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if m.typing {
		switch key {
		case "esc":
			m.typing = false
			m.input.Blur()
			m.endSearchInput()
			return nil
		case "enter":
			note := strings.TrimSpace(m.input.Value())
			m.input.Reset()
			m.typing = false
			m.input.Blur()
			if m.searching {
				m.endSearchInput()
				if note != "" {
					m.query = note
					m.find(1)
				}
				return nil
			}
			if note == "" {
				return nil
			}
			return m.command(&attachv0.CommandRequest{Action: &attachv0.CommandRequest_Steer{Steer: &gatewayv0.Steer{Note: note}}}, "note")
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(k)
		return cmd
	}
	if p := m.popup; p != nil {
		switch key {
		case "esc", "q", "enter", "o":
			m.popup = nil
		case "up", "k":
			p.scroll = max(p.scroll-1, 0)
		case "down", "j":
			p.scroll++
		case "pgup":
			p.scroll = max(p.scroll-m.height/2, 0)
		case "pgdown", "space":
			p.scroll += m.height / 2
		case "home", "g":
			p.scroll = 0
		case "ctrl+c":
			return tea.Quit
		}
		return nil
	}
	if m.filesFocus {
		files := m.sess.Files()
		switch key {
		case "up", "k":
			m.fileSel = max(m.fileSel-1, 0)
			return nil
		case "down", "j":
			m.fileSel = min(m.fileSel+1, max(len(files)-1, 0))
			return nil
		case "enter":
			if m.fileSel < len(files) {
				return m.openFile(files[m.fileSel].Path)
			}
			return nil
		case "esc", "o":
			m.filesFocus = false
			return nil
		}
	}
	switch key {
	case "q", "ctrl+c":
		return tea.Quit
	}
	m.notice = ""
	switch key {
	case "?":
		m.help = !m.help
	case "tab":
		m.tab, m.scroll = (m.tab+1)%tabCount, 0
	case "shift+tab":
		m.tab, m.scroll = (m.tab+tabCount-1)%tabCount, 0
	case "1", "2", "3", "4", "5", "6":
		m.tab, m.scroll = int(key[0]-'1'), 0
	case "up", "k":
		if m.tab == TabChat {
			m.selectBlock(-1)
		} else {
			m.scroll++
		}
	case "down", "j":
		if m.tab == TabChat {
			m.selectBlock(1)
		} else {
			m.scroll = max(m.scroll-1, 0)
		}
	case "pgup":
		m.scroll += m.height / 2
		m.selected = ""
	case "pgdown":
		m.scroll = max(m.scroll-m.height/2, 0)
		m.selected = ""
	case "end", "G", "esc":
		m.scroll, m.selected = 0, ""
	case "enter":
		if m.selected != "" {
			m.expanded[m.selected] = !m.expanded[m.selected]
		}
	case "e":
		m.expandAll = !m.expandAll
	case "o":
		if len(m.sess.Files()) == 0 {
			m.notice = "no files changed yet"
			return nil
		}
		m.tab, m.filesFocus = TabChat, true
		m.fileSel = min(m.fileSel, len(m.sess.Files())-1)
	case "d":
		m.separators = !m.separators
	case "s":
		m.collapseSubs = !m.collapseSubs
	case "]":
		m.worker, m.scroll, m.selected = m.nextWorker(1), 0, ""
	case "[":
		m.worker, m.scroll, m.selected = m.nextWorker(-1), 0, ""
	case "t":
		m.tool, m.scroll, m.selected = m.nextTool(), 0, ""
	case "i":
		if m.player != nil {
			m.notice = "a recording can't be sent notes"
			return nil
		}
		m.typing = true
		return m.input.Focus()
	case "P":
		return m.command(&attachv0.CommandRequest{Action: &attachv0.CommandRequest_Pause{Pause: &gatewayv0.Pause{Mode: gatewayv0.PauseMode_PAUSE_MODE_DRAIN}}}, "pause")
	case "X":
		return m.command(&attachv0.CommandRequest{Action: &attachv0.CommandRequest_Stop{Stop: &gatewayv0.Stop{}}}, "stop")
	}
	if p := m.player; p != nil {
		switch key {
		case "/":
			m.searching, m.typing = true, true
			m.input.Prompt = " / "
			m.input.Placeholder = "search the recording; Enter finds, Esc cancels"
			m.input.SetValue(m.query)
			m.input.CursorEnd()
			return m.input.Focus()
		case "n":
			m.find(1)
		case "N":
			m.find(-1)
		case "space":
			p.Toggle(m.now())
		case "+", "=":
			p.Faster()
		case "-":
			p.Slower()
		case "right", "l":
			p.Seek(p.Pos() + max(p.Len()/20, 1))
			m.sess = p.View()
		case "left", "h":
			p.Seek(p.Pos() - max(p.Len()/20, 1))
			m.sess = p.View()
		case "home", "g":
			p.Seek(0)
			m.sess = p.View()
		case "f":
			p.Seek(p.Len())
			p.SetFollow(!p.Follow())
			m.sess = p.View()
		}
	}
	return nil
}

// endSearchInput puts the input back to taking notes.
func (m *Model) endSearchInput() {
	if m.searching {
		m.searching = false
		m.input.Prompt = " › "
		m.input.Placeholder = "a note for the agent; Enter sends, Esc cancels"
	}
}

// find moves the replay to the next or previous event matching the
// query, as the last event shown, wrapping around the recording.
func (m *Model) find(step int) {
	p := m.player
	if p == nil || m.query == "" {
		return
	}
	f := &search.Filter{Text: m.query}
	evs := p.Events()
	var hits []int
	for i, ev := range evs {
		if f.Match(ev) {
			hits = append(hits, i)
		}
	}
	if len(hits) == 0 {
		m.notice = fmt.Sprintf("no event matches %q", m.query)
		return
	}
	cur := p.Pos() - 1 // the last event shown
	var k int
	if step > 0 {
		k = sort.SearchInts(hits, cur+1)
		if k == len(hits) {
			k = 0
		}
	} else {
		k = sort.SearchInts(hits, cur) - 1
		if k < 0 {
			k = len(hits) - 1
		}
	}
	p.Seek(hits[k] + 1)
	m.sess = p.View()
	m.scroll, m.selected = 0, ""
	m.notice = fmt.Sprintf("%q: match %d of %d, event %s", m.query, k+1, len(hits), evs[hits[k]].GetId())
}

// selectBlock moves the selection to the previous or next tool call
// block; past the last one it follows the end again.
func (m *Model) selectBlock(step int) {
	if len(m.blocks) == 0 {
		return
	}
	i := -1
	for j, b := range m.blocks {
		if b == m.selected {
			i = j
		}
	}
	switch {
	case i < 0 && step < 0:
		i = len(m.blocks) - 1
	case i < 0:
		return
	default:
		i += step
	}
	if i < 0 {
		i = 0
	}
	if i >= len(m.blocks) {
		m.selected, m.scroll = "", 0
		return
	}
	m.selected = m.blocks[i]
}

// command sends a command to the box in the background.
func (m *Model) command(req *attachv0.CommandRequest, what string) tea.Cmd {
	if m.send == nil {
		return func() tea.Msg { return noticeMsg("a recording takes no commands") }
	}
	req.Actor = m.actor
	send := m.send
	return func() tea.Msg {
		ack, err := send(req)
		if err != nil {
			return noticeMsg(fmt.Sprintf("%s refused: %v", what, err))
		}
		result := strings.ToLower(strings.TrimPrefix(ack.GetResult().String(), "COMMAND_RESULT_"))
		return noticeMsg(fmt.Sprintf("%s %s", what, result))
	}
}

func (m *Model) nextWorker(step int) string {
	ids := []string{""}
	for _, w := range m.sess.Workers() {
		ids = append(ids, w.ID)
	}
	for i, id := range ids {
		if id == m.worker {
			return ids[(i+step+len(ids))%len(ids)]
		}
	}
	return ""
}

func (m *Model) nextTool() string {
	names := []string{""}
	seen := map[string]bool{}
	for _, w := range m.sess.Workers() {
		for _, it := range w.Items {
			if it.Turn == nil {
				continue
			}
			for _, c := range it.Turn.Calls {
				if !seen[c.Name] {
					seen[c.Name] = true
					names = append(names, c.Name)
				}
			}
		}
	}
	for i, n := range names {
		if n == m.tool {
			return names[(i+1)%len(names)]
		}
	}
	return ""
}

// View implements tea.Model.
func (m *Model) View() tea.View {
	v := tea.NewView(m.Render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "actlet · " + short(m.sess.ID)
	if m.typing {
		v.Cursor = m.input.Cursor()
		if v.Cursor != nil {
			v.Cursor.Y = m.height - 2
		}
	}
	return v
}
