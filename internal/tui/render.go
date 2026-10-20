package tui

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/actlet/actlet/internal/view"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Colours: muted, with one accent per speaker.
var (
	colAgent    = lipgloss.Color("#8B7CF6")
	colSubagent = lipgloss.Color("#5FA8D3")
	colOperator = lipgloss.Color("#E5A84B")
	colOK       = lipgloss.Color("#6CBF84")
	colErr      = lipgloss.Color("#E06C75")
	colRun      = lipgloss.Color("#D7BA7D")
	colMuted    = lipgloss.Color("#7A7F8C")
	colFaint    = lipgloss.Color("#4B5060")

	styleMuted  = lipgloss.NewStyle().Foreground(colMuted)
	styleFaint  = lipgloss.NewStyle().Foreground(colFaint)
	styleBold   = lipgloss.NewStyle().Bold(true)
	styleAdd    = lipgloss.NewStyle().Foreground(colOK)
	styleDel    = lipgloss.NewStyle().Foreground(colErr)
	styleWarn   = lipgloss.NewStyle().Foreground(colRun)
	styleErr    = lipgloss.NewStyle().Foreground(colErr)
	styleOKText = lipgloss.NewStyle().Foreground(colOK)
	styleTabOn  = lipgloss.NewStyle().Bold(true).Underline(true)
	styleSelect = lipgloss.NewStyle().Background(lipgloss.Color("#2A2E3A"))
)

// Render draws the screen.
func (m *Model) Render() string {
	w, h := max(m.width, 40), max(m.height, 10)
	header := m.renderHeader(w)
	input := m.renderInput(w)
	status := m.renderStatus(w)
	bodyH := h - 4 // header, rule, input, status
	var body string
	switch m.tab {
	case TabChat:
		body = m.renderChat(w, bodyH)
	case TabSession:
		body = scrollText(m.sessionLines(w), bodyH, m.scroll)
	case TabTask:
		body = scrollText(m.taskLines(w), bodyH, m.scroll)
	case TabProfile:
		body = scrollText(m.profileLines(w), bodyH, m.scroll)
	case TabTools:
		body = scrollText(m.toolsLines(w), bodyH, m.scroll)
	case TabEvents:
		body = scrollText(m.eventLines(w), bodyH, m.scroll)
	}
	if m.popup != nil {
		body = m.renderPopup(w, bodyH)
	}
	body = lipgloss.NewStyle().Height(bodyH).MaxHeight(bodyH).Render(body)
	rule := styleFaint.Render(strings.Repeat("─", w))
	return lipgloss.JoinVertical(lipgloss.Left, header, body, rule, input, status)
}

func (m *Model) renderHeader(w int) string {
	title := lipgloss.NewStyle().Bold(true).Foreground(colAgent).Render("actlet")
	if info := m.sess.Info; info != nil && info.TaskID != "" {
		title += styleMuted.Render("  " + info.TaskID)
	}
	var tabs []string
	for i, name := range tabNames {
		if i == m.tab {
			tabs = append(tabs, styleTabOn.Render(name))
		} else {
			tabs = append(tabs, styleMuted.Render(name))
		}
	}
	right := strings.Join(tabs, "  ")
	gap := max(w-lipgloss.Width(title)-lipgloss.Width(right)-2, 1)
	// Where each tab is, for mouse clicks.
	m.tabSpans = m.tabSpans[:0]
	x := 1 + lipgloss.Width(title) + gap
	for _, name := range tabNames {
		m.tabSpans = append(m.tabSpans, [2]int{x, x + len(name)})
		x += len(name) + 2
	}
	return ansi.Truncate(" "+title+strings.Repeat(" ", gap)+right+" ", w, "")
}

// line is a rendered line, and the tool call block it belongs to.
type line struct {
	text  string
	block string
}

// renderChat is the main screen: the conversation, with a sidebar when
// there is room.
func (m *Model) renderChat(w, h int) string {
	sideW := 0
	if w >= 100 {
		sideW = 30
	}
	chatW := w - sideW
	if sideW > 0 {
		chatW--
	}
	lines := m.chatLines(chatW - 1)
	m.blocks = m.blocks[:0]
	start, end := -1, -1
	for i, l := range lines {
		if l.block != "" && !slices.Contains(m.blocks, l.block) {
			m.blocks = append(m.blocks, l.block)
		}
		if m.selected != "" && l.block == m.selected {
			if start < 0 {
				start = i
			}
			end = i
		}
	}
	// Keep the selected block in view; otherwise follow the end.
	if start >= 0 {
		bottom := len(lines) - m.scroll
		switch {
		case end >= bottom:
			m.scroll = max(len(lines)-end-1, 0)
		case start < bottom-h:
			m.scroll = max(len(lines)-start-h, 0)
		}
	}
	texts := make([]string, len(lines))
	for i, l := range lines {
		texts[i] = " " + l.text
		if m.selected != "" && l.block == m.selected {
			texts[i] = styleSelect.Render(padRight(texts[i], chatW))
		}
	}
	// Which block each screen row shows, for mouse clicks.
	from, to := visible(len(lines), h, m.scroll)
	m.rows = m.rows[:0]
	for _, l := range lines[from:to] {
		m.rows = append(m.rows, l.block)
	}
	chat := lipgloss.NewStyle().Width(chatW).MaxWidth(chatW).Render(scrollText(texts, h, m.scroll))
	if sideW == 0 {
		return chat
	}
	sep := styleFaint.Render(strings.TrimRight(strings.Repeat("│\n", h), "\n"))
	m.sideX = chatW + 1
	side := lipgloss.NewStyle().Width(sideW).MaxWidth(sideW).Render(strings.Join(fit(m.sidebarLines(sideW-2), h), "\n"))
	return lipgloss.JoinHorizontal(lipgloss.Top, chat, sep, " "+side)
}

// chatLines lays the session out as a conversation: the session's own
// items and the agent's turns, in order, with subagents nested under the
// calls that started them.
func (m *Model) chatLines(w int) []line {
	s := m.sess
	type entry struct {
		it     *view.Item
		worker *view.Worker
	}
	var items []entry
	if m.worker == "" {
		for _, it := range s.Items {
			items = append(items, entry{it, nil})
		}
	}
	top := s.Worker("root")
	if m.worker != "" {
		top = s.Worker(m.worker)
	}
	if top != nil {
		for _, it := range top.Items {
			items = append(items, entry{it, top})
		}
	}
	// Subagents whose parent this viewer never saw go at the top level.
	if m.worker == "" {
		for _, wk := range s.Workers() {
			if wk.ID != "root" && (wk.Parent == "" || s.Worker(wk.Parent) == nil) {
				for _, it := range wk.Items {
					items = append(items, entry{it, wk})
				}
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i].it, items[j].it
		if a.Epoch != b.Epoch {
			return a.Epoch < b.Epoch
		}
		return a.Seq < b.Seq
	})
	var out []line
	for _, e := range items {
		if m.tool != "" && e.it.Kind != view.ItemTurn {
			continue
		}
		lines := m.itemLines(e.it, e.worker, "", w)
		if m.separators && len(out) > 0 && e.it.Kind != view.ItemSystem && len(lines) > 0 && lines[0].text == "" {
			lines[0] = line{text: styleFaint.Render(strings.Repeat("─", w))}
		}
		out = append(out, lines...)
	}
	if len(out) == 0 {
		out = []line{{text: styleMuted.Render("Waiting for the agent…")}}
	}
	return out
}

// itemLines draws one timeline item, after a prefix for nesting.
func (m *Model) itemLines(it *view.Item, wk *view.Worker, prefix string, w int) []line {
	var out []line
	add := func(text string) { out = append(out, line{text: prefix + text}) }
	textW := w - lipgloss.Width(prefix) - 2
	switch it.Kind {
	case view.ItemNote:
		c := lipgloss.NewStyle().Foreground(colOperator)
		out = append(out, line{text: prefix})
		add(c.Render("▌ ") + c.Bold(true).Render("Operator") + styleMuted.Render(" · "+it.From) + m.clock(it.Time))
		for _, l := range wrap(it.Text, textW) {
			add(c.Render("▌") + " " + l)
		}
	case view.ItemSystem:
		st := styleMuted
		if it.Warn {
			st = styleWarn
		}
		for _, l := range wrap(it.Text, textW) {
			add(st.Render("· " + l))
		}
	case view.ItemTurn:
		out = append(out, m.turnLines(it, wk, prefix, w)...)
	}
	return out
}

// turnLines draws a model turn: who speaks, the text, then each tool call
// with its result.
func (m *Model) turnLines(it *view.Item, wk *view.Worker, prefix string, w int) []line {
	t := it.Turn
	var calls []*view.ToolCall
	for _, c := range t.Calls {
		if m.tool == "" || c.Name == m.tool || c.Child != "" {
			calls = append(calls, c)
		}
	}
	if m.tool != "" && len(calls) == 0 {
		return nil
	}
	var out []line
	add := func(text string) { out = append(out, line{text: prefix + text}) }
	col, who := colAgent, "Agent"
	if wk != nil && wk.ID != "root" {
		col, who = colSubagent, "Subagent "+wk.ID
	}
	c := lipgloss.NewStyle().Foreground(col)
	out = append(out, line{text: prefix})
	if t.Unseen {
		add(c.Render("▌ ") + c.Bold(true).Render(who) + styleMuted.Render(" · an earlier turn"))
	} else {
		meta := fmt.Sprintf(" · turn %d", t.Number)
		if t.Model != "" {
			meta += " · " + t.Model
		}
		if t.Usage.Input+t.Usage.Output > 0 {
			meta += fmt.Sprintf(" · %s in / %s out", tokens(t.Usage.Input), tokens(t.Usage.Output))
		}
		add(c.Render("▌ ") + c.Bold(true).Render(who) + styleMuted.Render(meta) + m.clock(it.Time))
		if t.Text != "" {
			for _, l := range wrap(t.Text, w-lipgloss.Width(prefix)-2) {
				add(c.Render("▌") + " " + l)
			}
		}
	}
	for i, call := range calls {
		if m.separators && i > 0 {
			add("  " + styleFaint.Render(strings.Repeat("┄", max(w-lipgloss.Width(prefix)-2, 0))))
		}
		out = append(out, m.callLines(call, wk, prefix+"  ", w)...)
	}
	return out
}

// callLines draws a tool call and its result as one block.
func (m *Model) callLines(call *view.ToolCall, wk *view.Worker, prefix string, w int) []line {
	owner := "root"
	if wk != nil {
		owner = wk.ID
	}
	key := owner + "/" + call.CallID
	expanded := m.expandAll || m.expanded[key]
	var out []line
	add := func(text string) { out = append(out, line{text: prefix + text, block: key}) }
	icon := styleWarn.Render(m.spinner())
	switch call.Status {
	case view.ToolOK:
		icon = styleOKText.Render("✓")
	case view.ToolError:
		icon = styleErr.Render("✗")
	case view.ToolUnknown:
		icon = styleWarn.Render("?")
	}
	head := icon + " " + styleBold.Render(toolLabel(call.Name))
	right := ""
	switch {
	case call.Status == view.ToolUnknown:
		right = "outcome unknown"
	case call.Status == view.ToolRunning:
		right = "running"
	case call.Took > 0:
		right = fmtDuration(call.Took)
	}
	avail := w - lipgloss.Width(prefix) - lipgloss.Width(head) - lipgloss.Width(right) - 3
	if args := keyArgs(call); args != "" && avail > 5 {
		head += " " + styleMuted.Render(ansi.Truncate(args, avail, "…"))
	}
	gap := max(w-lipgloss.Width(prefix)-lipgloss.Width(head)-lipgloss.Width(right), 1)
	add(head + strings.Repeat(" ", gap) + styleMuted.Render(right))

	gutter := "  " + styleFaint.Render("│") + " "
	bodyW := w - lipgloss.Width(prefix) - 4
	// A subagent's own turns, nested under the call that started it.
	if call.Child != "" {
		child := m.sess.Worker(call.Child)
		if child == nil {
			return out
		}
		if expanded || call.Status == view.ToolRunning || !m.collapseSubs {
			nested := prefix + "  " + styleFaint.Render("┊") + " "
			for _, it := range child.Items {
				for _, l := range m.itemLines(it, child, nested, w) {
					if l.block == "" {
						l.block = key // the subagent's text belongs to its call
					}
					out = append(out, l)
				}
			}
		} else {
			add(gutter + styleMuted.Render(fmt.Sprintf("%s worked for %s (enter shows them)", child.ID, plural(child.Turns, "turn"))))
		}
		if child.Result != "" {
			st := lipgloss.NewStyle()
			if child.Status == view.WorkerFailed {
				st = styleErr
			}
			for _, l := range wrap(child.Result, bodyW-2) {
				add(gutter + styleMuted.Render("↳ ") + st.Render(l))
			}
		}
		return out
	}
	limit := 3
	if expanded {
		limit = 400
	}
	lines := resultLines(call)
	shown := lines
	if len(lines) > limit {
		shown = lines[:limit]
	}
	for _, l := range shown {
		for _, wl := range wrap(l.text, bodyW) {
			add(gutter + l.style.Render(wl))
		}
	}
	if more := len(lines) - len(shown); more > 0 {
		add(gutter + styleFaint.Render("… "+plural(uint64(more), "more line")))
	}
	return out
}

type styled struct {
	text  string
	style lipgloss.Style
}

// resultLines are what a call shows: its diffs if it changed files, or its
// output.
func resultLines(call *view.ToolCall) []styled {
	var out []styled
	if len(call.Diffs) > 0 {
		if call.Status == view.ToolError {
			out = append(out, styled{firstLine(call.Output), styleErr})
		}
		for _, d := range call.Diffs {
			for _, l := range strings.Split(strings.TrimRight(d.Diff, "\n"), "\n") {
				switch {
				case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
				case strings.HasPrefix(l, "@@"):
					out = append(out, styled{l, styleFaint})
				case strings.HasPrefix(l, "+"):
					out = append(out, styled{l, styleAdd})
				case strings.HasPrefix(l, "-"):
					out = append(out, styled{l, styleDel})
				default:
					out = append(out, styled{l, styleMuted})
				}
			}
		}
		return out
	}
	base := lipgloss.NewStyle()
	if call.Status == view.ToolError || call.Status == view.ToolUnknown {
		base = styleErr
	}
	for _, l := range strings.Split(strings.TrimRight(call.Output, "\n"), "\n") {
		st := base
		switch {
		case l == "" && len(out) == 0:
			continue
		case l == "stdout:" || l == "stderr:":
			continue // the output speaks for itself
		case strings.HasPrefix(l, "exit code 0"):
			continue
		case strings.HasPrefix(l, "exception:"), strings.HasPrefix(l, "outcome_unknown"), strings.HasPrefix(l, "exit code"):
			st = styleErr
		case strings.HasPrefix(l, "traceback") || strings.HasPrefix(l, "hint:"):
			st = styleMuted
		}
		out = append(out, styled{l, st})
	}
	return out
}

// toolLabel names a tool for people.
func toolLabel(name string) string {
	switch name {
	case "read_file":
		return "Read"
	case "write_file":
		return "Write"
	case "edit_file":
		return "Edit"
	case "glob":
		return "Glob"
	case "grep":
		return "Grep"
	case "shell":
		return "Shell"
	case "python":
		return "Python"
	case "spawn_subagent":
		return "Subagent"
	case "report_progress":
		return "Progress"
	case "send_feedback":
		return "Feedback"
	case "budget_status":
		return "Budget"
	case "decide":
		return "Decide"
	case "llm":
		return "LLM"
	}
	return name
}

// keyArgs are a call's most telling arguments.
func keyArgs(call *view.ToolCall) string {
	a := call.Args()
	str := func(k string) string {
		v, _ := a[k].(string)
		return firstLine(v)
	}
	switch call.Name {
	case "read_file":
		return str("path")
	case "write_file", "edit_file":
		p := str("path")
		for _, d := range call.Diffs {
			add, del := d.Stat()
			p += fmt.Sprintf("  +%d −%d", add, del)
		}
		return p
	case "glob":
		return strings.TrimSpace(str("pattern") + " " + str("path"))
	case "grep":
		s := str("pattern")
		if p := str("path"); p != "" {
			s += "  in " + p
		}
		return s
	case "shell":
		return "$ " + str("command")
	case "python":
		if p := str("path"); p != "" {
			return p
		}
		return firstLine(str("code"))
	case "spawn_subagent":
		s := call.Child
		if model := str("model"); model != "" {
			s += " · " + model
		}
		return strings.TrimSpace(s + "  " + str("task"))
	case "report_progress":
		if p, ok := a["percent"].(float64); ok {
			return fmt.Sprintf("%.0f%% · %s", p, str("message"))
		}
		return str("message")
	case "send_feedback":
		return str("kind") + ": " + str("message")
	case "decide":
		return str("question")
	case "llm":
		s := str("prompt")
		if model := str("model"); model != "" {
			s = model + " · " + s
		}
		return s
	case "budget_status":
		return ""
	}
	return firstLine(call.Input)
}

// sidebarLines are the chat's sidebar: the workers and the changed files.
func (m *Model) sidebarLines(w int) []string {
	s := m.sess
	var out []string
	head := func(t string) { out = append(out, styleMuted.Render(strings.ToUpper(t))) }
	head("Workers")
	workers := s.Workers()
	for _, wk := range workers {
		icon := styleWarn.Render("●")
		switch wk.Status {
		case view.WorkerFinished:
			icon = styleOKText.Render("✓")
		case view.WorkerFailed:
			icon = styleErr.Render("✗")
		}
		label := strings.Repeat(" ", strings.Count(wk.ID, ".")) + wk.ID
		if wk.ID == m.worker {
			label = styleBold.Render(label)
		}
		extra := styleMuted.Render(fmt.Sprintf(" %s %s", orNA(wk.Model), plural(wk.Turns, "turn")))
		out = append(out, ansi.Truncate(icon+" "+label+extra, w, "…"))
	}
	if len(workers) == 0 {
		out = append(out, styleFaint.Render("none yet"))
	}
	out = append(out, "")
	head("Files")
	files := s.Files()
	if len(files) == 0 {
		out = append(out, styleFaint.Render("none changed"))
	}
	for k := range m.fileRows {
		delete(m.fileRows, k)
	}
	for i, f := range files {
		stat := styleAdd.Render(fmt.Sprintf("+%d", f.Added)) + " " + styleDel.Render(fmt.Sprintf("−%d", f.Removed))
		name := truncateLeft(f.Path, w-lipgloss.Width(stat)-1)
		if m.filesFocus && i == m.fileSel {
			name = styleSelect.Render(name)
		}
		m.fileRows[len(out)+1] = f.Path // screen row: under the header line
		out = append(out, name+" "+stat)
	}
	if len(files) > 0 {
		out = append(out, styleFaint.Render("o or a click opens a file"))
	}
	return out
}

// renderStatus is the one status line: what is happening, the model, how
// full its context is, cost, time and what the viewer shows.
func (m *Model) renderStatus(w int) string {
	s := m.sess
	state := view.StateName(s.State)
	if s.State == gatewayv0.SessionState_SESSION_STATE_UNSPECIFIED && m.info != nil {
		state = view.StateName(m.info.GetState())
	}
	var icon string
	switch s.State {
	case gatewayv0.SessionState_SESSION_STATE_COMPLETED:
		icon = styleOKText.Render("✓")
	case gatewayv0.SessionState_SESSION_STATE_FAILED, gatewayv0.SessionState_SESSION_STATE_STOPPED:
		icon = styleErr.Render("✗")
	case gatewayv0.SessionState_SESSION_STATE_PAUSED:
		icon = styleWarn.Render("‖")
	default:
		icon = styleWarn.Render(m.spinner())
	}
	sep := styleFaint.Render(" · ")
	parts := []string{icon + " " + styleMuted.Render(state)}
	if root := s.Worker("root"); root != nil {
		if root.Model != "" {
			parts = append(parts, styleMuted.Render(root.Model))
		}
		if window := s.ContextWindow(root.Model); window > 0 && root.Context > 0 {
			parts = append(parts, styleMuted.Render(fmt.Sprintf("ctx %d%%", root.Context*100/window)))
		} else if root.Context > 0 {
			parts = append(parts, styleMuted.Render("ctx "+tokens(root.Context)))
		}
	}
	if s.Usage.Cost > 0 {
		parts = append(parts, styleMuted.Render(fmt.Sprintf("%.2f %s", s.Usage.Cost, s.Usage.Currency)))
	}
	parts = append(parts, styleMuted.Render(m.elapsed()), styleMuted.Render(m.connLabel()))
	if alarm := m.budgetAlarm(); alarm != "" {
		parts = append(parts, styleErr.Render(alarm))
	}
	left := " " + strings.Join(parts, sep)
	if m.notice != "" {
		left += sep + styleWarn.Render(m.notice)
	}
	right := styleFaint.Render("? keys ")
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return ansi.Truncate(left, w, "…")
	}
	return left + strings.Repeat(" ", gap) + right
}

// connLabel says briefly what the viewer shows.
func (m *Model) connLabel() string {
	switch {
	case m.player != nil:
		p := m.player
		pct := 100
		if p.Len() > 0 {
			pct = p.Pos() * 100 / p.Len()
		}
		return fmt.Sprintf("replay %s %gx %d%%", p.State(), p.Speed(), pct)
	case m.lost != nil:
		return m.conn.Kind + " lost"
	case m.ended:
		return m.conn.Kind + ", run ended"
	}
	return m.conn.Kind
}

func (m *Model) renderInput(w int) string {
	if m.typing {
		return m.input.View()
	}
	if m.help {
		keys := "i note · ↑/↓ select · enter expand · e all · o files · s subagents · d lines · [ ] worker · t tool · tab views · P pause · X stop · q detach"
		if m.player != nil {
			keys = "space play · ←/→ seek · +/- speed · f follow · ↑/↓ select · enter expand · e all · o files · d lines · [ ] worker · t tool · tab views · q quit"
		}
		return styleMuted.Render(ansi.Truncate(" "+keys, w, "…"))
	}
	if m.player != nil {
		return styleFaint.Render(" a recording · space play/pause · ←/→ seek · +/- speed")
	}
	return styleMuted.Render(" › ") + styleFaint.Render("press i to send the agent a note")
}

// sessionLines draw the Session tab: the figures the status line leaves
// out.
func (m *Model) sessionLines(w int) []string {
	s := m.sess
	var out []string
	add := func(s string) { out = append(out, wrap(s, w)...) }
	kv := func(k, v string) { add(fmt.Sprintf("  %-12s %s", k, v)) }
	head := func(t string) { out = append(out, "", styleBold.Render(t)) }

	out = append(out, styleBold.Render("Session"))
	kv("id", orNA(s.ID))
	state := view.StateName(s.State)
	if s.Reason != "" {
		state += ": " + s.Reason
	}
	kv("state", state)
	run := fmt.Sprint(s.Epoch)
	if s.Resumed {
		run += ", resumed"
	}
	kv("run", run)
	kv("elapsed", m.elapsed())
	conn := m.conn.Kind
	if m.conn.Target != "" {
		conn += " " + m.conn.Target
	}
	kv("viewing", conn)
	events := fmt.Sprint(s.Events)
	if s.Skipped > 0 {
		events += fmt.Sprintf(", %d missed", s.Skipped)
	}
	kv("events", events)
	if p := s.Progress; p != nil {
		text := p.Message
		if p.Percent >= 0 {
			text = fmt.Sprintf("%d%% · %s", p.Percent, text)
		}
		kv("progress", text)
	}

	head("Box")
	folder, output, mem, cpu, gw := "n/a", "n/a", "n/a", "n/a", "n/a"
	if st := m.info.GetStats(); st != nil {
		folder, output = sizeOf(st.GetFolderBytes()), sizeOf(st.GetOutputBytes())
		mem = sizeOf(int64(st.GetMemoryBytes()))
		if d := st.GetCpu().AsDuration(); d > 0 {
			cpu = d.Round(10 * time.Millisecond).String()
		}
		gw = st.GetGateway()
		if st.GetGatewayConnected() {
			gw += " (connected)"
		} else {
			gw += " (not connected)"
		}
	}
	kv("folder", folder)
	kv("output", output)
	kv("memory", mem)
	kv("cpu", cpu)
	kv("gateway", gw)

	head("Usage")
	cost := "n/a"
	if s.Usage.Cost > 0 {
		cost = fmt.Sprintf("%.4f %s", s.Usage.Cost, s.Usage.Currency)
	}
	kv("tokens", fmt.Sprintf("%s in, %s out", tokens(s.Usage.Input), tokens(s.Usage.Output)))
	kv("cost", cost)
	add(styleMuted.Render(fmt.Sprintf("  %-10s %-8s %6s %8s %8s %9s", "worker", "model", "turns", "context", "tokens", "cost")))
	for _, wk := range s.Workers() {
		ctx := tokens(wk.Context)
		if window := s.ContextWindow(wk.Model); window > 0 && wk.Context > 0 {
			ctx = fmt.Sprintf("%d%%", wk.Context*100/window)
		}
		c := "n/a"
		if wk.Usage.Cost > 0 {
			c = fmt.Sprintf("%.4f", wk.Usage.Cost)
		}
		add(fmt.Sprintf("  %-10s %-8s %6d %8s %8s %9s", wk.ID, orNA(wk.Model), wk.Turns, ctx, tokens(wk.Usage.Input+wk.Usage.Output), c))
	}

	head("Budgets")
	if bs := m.budgets(); len(bs) == 0 {
		add("  none")
	} else {
		for _, b := range bs {
			add("  " + m.budgetLine(b))
		}
	}

	if len(s.Feedback) > 0 {
		head("Feedback")
		if s.FeedbackDropped > 0 {
			add(styleFaint.Render(fmt.Sprintf("  … %d earlier", s.FeedbackDropped)))
		}
		for _, f := range s.Feedback {
			add(fmt.Sprintf("  %s %-10s %s", f.Time.UTC().Format("15:04:05"), f.Kind, f.Message))
		}
	}
	return out
}

// budgets are the session's budgets: as the latest budget status reported
// them, or else as the run started with them, with no usage known.
func (m *Model) budgets() []view.BudgetState {
	if len(m.sess.Budgets) > 0 {
		return m.sess.Budgets
	}
	var out []view.BudgetState
	if info := m.sess.Info; info != nil {
		for _, b := range info.Budgets {
			out = append(out, view.BudgetState{Kind: b.GetKind(), Limit: b.GetLimit(), Used: -1, Currency: view.Sanitize(b.GetCurrency())})
		}
	}
	return out
}

func (m *Model) budgetLine(b view.BudgetState) string {
	kind := view.BudgetName(b.Kind)
	limit := fmt.Sprintf("%g", b.Limit)
	if b.Currency != "" {
		limit += " " + b.Currency
	}
	used, ok := m.budgetUsed(b)
	usedText := "n/a"
	if ok {
		var amount string
		switch b.Kind {
		case gatewayv0.BudgetKind_BUDGET_KIND_TOKENS:
			amount, limit = tokens(uint64(used)), tokens(uint64(b.Limit))
		case gatewayv0.BudgetKind_BUDGET_KIND_TIME:
			amount = (time.Duration(used) * time.Second).String()
			limit = (time.Duration(b.Limit) * time.Second).String()
		case gatewayv0.BudgetKind_BUDGET_KIND_DISK:
			amount, limit = sizeOf(int64(used)), sizeOf(int64(b.Limit))
		default:
			amount = fmt.Sprintf("%.2f", used)
		}
		usedText = fmt.Sprintf("%s (%.0f%%)", amount, used*100/b.Limit)
	}
	line := fmt.Sprintf("%-6s limit %-10s used %s", kind, limit, usedText)
	switch b.State {
	case "alerted":
		line += "  " + styleWarn.Render("past the alert")
	case "exhausted":
		line += "  " + styleErr.Render("used up")
	case "waiting":
		line += "  " + styleErr.Render("used up; work waits")
	}
	return line
}

// budgetUsed is what the session has used of a budget: the latest figure
// the viewer has, from the model calls or the budget status.
func (m *Model) budgetUsed(b view.BudgetState) (float64, bool) {
	s := m.sess
	used, ok := b.Used, b.Used >= 0
	switch b.Kind {
	case gatewayv0.BudgetKind_BUDGET_KIND_MONEY:
		if s.Usage.Cost > 0 && s.Usage.Currency == b.Currency && s.Usage.Cost > used {
			return s.Usage.Cost, true
		}
	case gatewayv0.BudgetKind_BUDGET_KIND_TOKENS:
		if n := float64(s.Usage.Input + s.Usage.Output); n > 0 && !ok {
			return n, true
		}
	case gatewayv0.BudgetKind_BUDGET_KIND_TIME:
		if !ok {
			if d, ok := m.elapsedTime(); ok {
				return d.Seconds(), true
			}
		}
	}
	return used, ok
}

// budgetAlarm says which budget is used up, for the status line.
func (m *Model) budgetAlarm() string {
	for _, b := range m.sess.Budgets {
		switch b.State {
		case "exhausted":
			return view.BudgetName(b.Kind) + " budget used up"
		case "waiting":
			return "waiting for the " + view.BudgetName(b.Kind) + " budget"
		}
	}
	return ""
}

// taskLines draw the Task tab: the task and the worker tree.
func (m *Model) taskLines(w int) []string {
	var out []string
	add := func(s string) { out = append(out, wrap(s, w)...) }
	info := m.sess.Info
	if info == nil {
		return []string{styleMuted.Render("no task information yet")}
	}
	add(styleBold.Render("Task ") + info.TaskID + styleMuted.Render(" ("+info.TaskKind+")"))
	out = append(out, "")
	for _, l := range strings.Split(strings.TrimRight(info.Task, "\n"), "\n") {
		add("  " + l)
	}
	out = append(out, "", styleBold.Render("Workers"))
	for _, wk := range m.sess.Workers() {
		indent := strings.Repeat("  ", strings.Count(wk.ID, ".")+1)
		add(fmt.Sprintf("%s%s  %s  %s  model %s", indent, styleBold.Render(wk.ID), wk.Status, plural(wk.Turns, "turn"), orNA(wk.Model)))
		if wk.Task != "" {
			add(indent + "  task: " + firstLine(wk.Task))
		}
		if wk.Result != "" {
			add(indent + "  result: " + firstLine(wk.Result))
		}
	}
	return out
}

// profileLines draw the Profile tab: the effective configuration and
// models.
func (m *Model) profileLines(w int) []string {
	var out []string
	add := func(s string) { out = append(out, wrap(s, w)...) }
	info := m.sess.Info
	if info == nil {
		return []string{styleMuted.Render("no profile information yet")}
	}
	add(styleBold.Render("Effective configuration") + styleMuted.Render(" (profile "+m.sess.Profile+")"))
	for _, l := range strings.Split(strings.TrimRight(info.Config, "\n"), "\n") {
		add("  " + l)
	}
	out = append(out, "", styleBold.Render("Models"))
	if mc := info.Models; mc != nil && len(mc.GetAliases()) > 0 {
		for _, a := range mc.GetAliases() {
			l := "  " + view.Sanitize(a.GetAlias())
			if d := view.Sanitize(a.GetDescription()); d != "" {
				l += "  " + d
			}
			if a.GetContextWindow() > 0 {
				l += styleMuted.Render("  " + tokens(a.GetContextWindow()) + " context")
			}
			add(l)
		}
		var roles []string
		for r, a := range mc.GetRoles() {
			roles = append(roles, view.Sanitize(r)+" → "+view.Sanitize(a))
		}
		slices.Sort(roles)
		add("  roles: " + orNA(strings.Join(roles, ", ")))
		add("  selectable: " + orNA(view.Sanitize(strings.Join(mc.GetSelectable(), ", "))))
	} else {
		add("  none listed; the agent asks for the \"main\" alias")
	}
	out = append(out, "", styleBold.Render("Budgets"))
	bs := m.budgets()
	if len(bs) == 0 {
		add("  none")
	}
	for _, b := range bs {
		add("  " + m.budgetLine(b))
	}
	out = append(out, "", styleBold.Render("Local viewers may"))
	add("  " + orNA(strings.Join(info.LocalCommands, ", ")))
	return out
}

// toolsLines draw the Tools tab: the tools, the profile's grants and the
// manifest.
func (m *Model) toolsLines(w int) []string {
	var out []string
	add := func(s string) { out = append(out, wrap(s, w)...) }
	info := m.sess.Info
	if info == nil {
		return []string{styleMuted.Render("no tool information yet")}
	}
	add(styleBold.Render("Tools") + styleMuted.Render(" (recovery policy)"))
	if len(info.Tools) == 0 {
		add("  none")
	}
	for _, t := range info.Tools {
		l := fmt.Sprintf("  %-15s %-10s ", view.Sanitize(t.GetName()), view.Sanitize(t.GetPolicy()))
		desc := ansi.Truncate(firstSentence(view.Sanitize(t.GetDescription())), max(w-lipgloss.Width(l), 10), "…")
		out = append(out, l+styleMuted.Render(desc))
	}
	out = append(out, "", styleBold.Render("Grants"))
	if len(info.Grants) == 0 {
		add("  none: every local tool, with default settings")
	}
	for _, g := range info.Grants {
		var settings []string
		for k, v := range g.GetSettings() {
			settings = append(settings, view.Sanitize(k)+"="+view.Sanitize(v))
		}
		slices.Sort(settings)
		add("  " + view.Sanitize(g.GetName()) + "  " + strings.Join(settings, " "))
	}
	out = append(out, "", styleBold.Render("Installed (tools manifest)"))
	if info.Manifest == "" {
		add("  none")
	}
	for _, l := range strings.Split(info.Manifest, "\n") {
		if l != "" {
			add("  " + l)
		}
	}
	return out
}

// eventLines draw the Events tab: every event in a line, for debugging.
func (m *Model) eventLines(w int) []string {
	s := m.sess
	var out []string
	if s.LogDropped > 0 {
		out = append(out, styleMuted.Render(fmt.Sprintf("… %d older events", s.LogDropped)))
	}
	for _, l := range s.Log {
		who := l.Worker
		if who == "" {
			who = "session"
		}
		out = append(out, ansi.Truncate(styleMuted.Render(fmt.Sprintf("%d.%-4d %-8s ", l.Epoch, l.Seq, who))+l.Text, w, "…"))
	}
	if len(out) == 0 {
		out = []string{styleMuted.Render("no events yet")}
	}
	return out
}

func (m *Model) clock(t time.Time) string {
	if t.IsZero() || t.Unix() <= 0 {
		return ""
	}
	return styleFaint.Render(" · " + t.UTC().Format("15:04:05"))
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (m *Model) spinner() string {
	return spinnerFrames[m.frame%len(spinnerFrames)]
}

func (m *Model) elapsed() string {
	if d, ok := m.elapsedTime(); ok {
		return fmtDuration(d.Round(time.Second))
	}
	return "0s"
}

func (m *Model) elapsedTime() (time.Duration, bool) {
	s := m.sess
	if s.First.IsZero() {
		return 0, false
	}
	end := s.Last
	if m.player == nil && !m.ended && m.lost == nil && s.State == gatewayv0.SessionState_SESSION_STATE_RUNNING {
		end = m.now()
	}
	if st := m.info.GetStats(); st != nil && st.GetStarted() != nil && m.player == nil {
		return end.Sub(st.GetStarted().AsTime()), true
	}
	return end.Sub(s.First), true
}

func fmtDuration(d time.Duration) string {
	switch {
	case d >= 10*time.Second:
		return d.Round(time.Second).String()
	case d >= time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Millisecond).String()
}

// scrollText shows h lines ending scroll lines before the end.
func scrollText(lines []string, h, scroll int) string {
	from, to := visible(len(lines), h, scroll)
	return strings.Join(lines[from:to], "\n")
}

// visible is the range of n lines that h rows show, scroll lines up from
// the end.
func visible(n, h, scroll int) (from, to int) {
	if h <= 0 {
		return 0, 0
	}
	to = max(n-scroll, min(h, n))
	return max(to-h, 0), to
}

// fit cuts lines to h.
func fit(lines []string, h int) []string {
	if len(lines) > h {
		return lines[:h]
	}
	return lines
}

// wrap wraps s to w columns; continuation lines keep their paragraph's
// indentation.
func wrap(s string, w int) []string {
	if w <= 0 {
		return []string{s}
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		indent := para[:len(para)-len(strings.TrimLeft(para, " "))]
		if len(indent) > w/2 {
			indent = ""
		}
		for _, l := range strings.Split(ansi.Wrap(strings.TrimLeft(para, " "), w-len(indent), ""), "\n") {
			out = append(out, indent+l)
		}
	}
	return out
}

func padRight(s string, w int) string {
	if n := lipgloss.Width(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

func short(id string) string {
	if id == "" {
		return "?"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func plural(n uint64, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func orNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}

func tokens(n uint64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func sizeOf(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Dump renders the screen as plain text, without colours.
func (m *Model) Dump() string { return ansi.Strip(m.Render()) }

// truncateLeft shortens s to width cells by cutting its start, so a
// path keeps its file name.
func truncateLeft(s string, width int) string {
	over := lipgloss.Width(s) - width
	if over <= 0 {
		return s
	}
	if width < 2 {
		return ansi.Truncate(s, width, "")
	}
	return ansi.TruncateLeft(s, over+1, "…")
}
