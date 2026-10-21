package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// The viewer's keys, in tables that both handle them and list them in
// the help panel, so that the two can't drift apart.

// binding is what keys do.
type binding struct {
	keys []string // as Bubble Tea names them
	// label shows the keys in the help, when listing them one by one
	// doesn't say it best; hidden leaves them out of it.
	label  string
	hidden bool
	help   string
	group  string
	// Where it applies: live viewers, replays, or both when neither is
	// set.
	live, replay bool
	do           func(m *Model) tea.Cmd
}

// Groups of keys, in the order the help lists them.
const (
	groupNavigation = "Navigation"
	groupTabs       = "Tabs"
	groupSidebar    = "Sidebar and files"
	groupView       = "View"
	groupSteer      = "Steering and commands"
	groupReplay     = "Replay and search"
	groupFiles      = "In the file list"
	groupFile       = "In the file viewer"
	groupHelp       = "In this help"
)

var groups = []string{groupNavigation, groupTabs, groupSidebar, groupView, groupSteer, groupReplay, groupFiles, groupFile, groupHelp}

// applies reports whether a binding is for this viewer.
func (b binding) applies(m *Model) bool {
	switch {
	case b.live:
		return m.player == nil
	case b.replay:
		return m.player != nil
	}
	return true
}

// dispatch runs the binding of a key, if one of the table's has it.
func (m *Model) dispatch(table []binding, key string) (tea.Cmd, bool) {
	for _, b := range table {
		if b.applies(m) && slices.Contains(b.keys, key) {
			return b.do(m), true
		}
	}
	return nil, false
}

func seekBy(m *Model, step int) tea.Cmd {
	p := m.player
	p.Seek(p.Pos() + step*max(p.Len()/20, 1))
	m.sess = p.View()
	return nil
}

func setTab(tab int) func(m *Model) tea.Cmd {
	return func(m *Model) tea.Cmd {
		m.tab, m.scroll = tab, 0
		return nil
	}
}

// mainKeys are the keys of the main screen.
var mainKeys = []binding{
	{keys: []string{"up", "k"}, help: "select the previous tool call; other tabs: scroll up", group: groupNavigation, do: func(m *Model) tea.Cmd {
		if m.tab == TabChat {
			m.selectBlock(-1)
		} else {
			m.scroll++
		}
		return nil
	}},
	{keys: []string{"down", "j"}, help: "select the next tool call; other tabs: scroll down", group: groupNavigation, do: func(m *Model) tea.Cmd {
		if m.tab == TabChat {
			m.selectBlock(1)
		} else {
			m.scroll = max(m.scroll-1, 0)
		}
		return nil
	}},
	{keys: []string{"enter"}, help: "expand or collapse the selected tool call", group: groupNavigation, do: func(m *Model) tea.Cmd {
		if m.selected != "" {
			m.expanded[m.selected] = !m.expanded[m.selected]
		}
		return nil
	}},
	{keys: []string{"e"}, help: "expand or collapse every tool call", group: groupNavigation, do: func(m *Model) tea.Cmd {
		m.expandAll = !m.expandAll
		return nil
	}},
	{keys: []string{"pgup"}, help: "scroll up a page", group: groupNavigation, do: func(m *Model) tea.Cmd {
		m.scroll += m.height / 2
		m.selected = ""
		return nil
	}},
	{keys: []string{"pgdown"}, help: "scroll down a page", group: groupNavigation, do: func(m *Model) tea.Cmd {
		m.scroll = max(m.scroll-m.height/2, 0)
		m.selected = ""
		return nil
	}},
	{keys: []string{"end", "G", "esc"}, help: "back to following the end", group: groupNavigation, do: func(m *Model) tea.Cmd {
		m.scroll, m.selected = 0, ""
		return nil
	}},

	{keys: []string{"tab"}, help: "the next tab", group: groupTabs, do: func(m *Model) tea.Cmd {
		m.tab, m.scroll = (m.tab+1)%tabCount, 0
		return nil
	}},
	{keys: []string{"shift+tab"}, help: "the previous tab", group: groupTabs, do: func(m *Model) tea.Cmd {
		m.tab, m.scroll = (m.tab+tabCount-1)%tabCount, 0
		return nil
	}},
	{keys: []string{"1"}, label: "1–6", help: "Chat, Session, Task, Profile, Tools, Events", group: groupTabs, do: setTab(0)},
	{keys: []string{"2"}, hidden: true, do: setTab(1)},
	{keys: []string{"3"}, hidden: true, do: setTab(2)},
	{keys: []string{"4"}, hidden: true, do: setTab(3)},
	{keys: []string{"5"}, hidden: true, do: setTab(4)},
	{keys: []string{"6"}, hidden: true, do: setTab(5)},

	{keys: []string{"["}, help: "show the previous worker alone; the agent shows them all", group: groupSidebar, do: func(m *Model) tea.Cmd {
		m.worker, m.scroll, m.selected = m.nextWorker(-1), 0, ""
		return nil
	}},
	{keys: []string{"]"}, help: "show the next worker alone", group: groupSidebar, do: func(m *Model) tea.Cmd {
		m.worker, m.scroll, m.selected = m.nextWorker(1), 0, ""
		return nil
	}},
	{keys: []string{"t"}, help: "show one tool's calls at a time", group: groupSidebar, do: func(m *Model) tea.Cmd {
		m.tool, m.scroll, m.selected = m.nextTool(), 0, ""
		return nil
	}},
	{keys: []string{"o"}, help: "go to the changed files, to open one", group: groupSidebar, do: func(m *Model) tea.Cmd {
		if len(m.sess.Files()) == 0 {
			m.notice = "no files changed yet"
			return nil
		}
		m.tab, m.filesFocus = TabChat, true
		m.fileSel = min(m.fileSel, len(m.sess.Files())-1)
		return nil
	}},

	{keys: []string{"d"}, help: "hide or show the lines between turns and tool calls", group: groupView, do: func(m *Model) tea.Cmd {
		m.separators = !m.separators
		return nil
	}},
	{keys: []string{"s"}, help: "collapse or show subagents' turns", group: groupView, do: func(m *Model) tea.Cmd {
		m.collapseSubs = !m.collapseSubs
		return nil
	}},
	{keys: []string{"?"}, help: "this help", group: groupView, do: func(m *Model) tea.Cmd {
		m.help, m.helpScroll = true, 0
		return nil
	}},

	{keys: []string{"i"}, help: "type a note for the agent; enter sends it, esc cancels", group: groupSteer, live: true, do: func(m *Model) tea.Cmd {
		m.typing = true
		return m.input.Focus()
	}},
	{keys: []string{"P"}, help: "pause the session, if the profile lets viewers", group: groupSteer, live: true, do: func(m *Model) tea.Cmd {
		return m.command(&attachv0.CommandRequest{Action: &attachv0.CommandRequest_Pause{Pause: &gatewayv0.Pause{Mode: gatewayv0.PauseMode_PAUSE_MODE_DRAIN}}}, "pause")
	}},
	{keys: []string{"X"}, help: "stop the session, if the profile lets viewers", group: groupSteer, live: true, do: func(m *Model) tea.Cmd {
		return m.command(&attachv0.CommandRequest{Action: &attachv0.CommandRequest_Stop{Stop: &gatewayv0.Stop{}}}, "stop")
	}},
	{keys: []string{"q", "ctrl+c"}, help: "detach; the session carries on", group: groupSteer, live: true, do: func(*Model) tea.Cmd { return tea.Quit }},

	{keys: []string{"space"}, help: "play or pause", group: groupReplay, replay: true, do: func(m *Model) tea.Cmd {
		m.player.Toggle(m.now())
		return nil
	}},
	{keys: []string{"left", "h"}, help: "seek back", group: groupReplay, replay: true, do: func(m *Model) tea.Cmd { return seekBy(m, -1) }},
	{keys: []string{"right", "l"}, help: "seek forward", group: groupReplay, replay: true, do: func(m *Model) tea.Cmd { return seekBy(m, 1) }},
	{keys: []string{"home", "g"}, help: "back to the start", group: groupReplay, replay: true, do: func(m *Model) tea.Cmd {
		m.player.Seek(0)
		m.sess = m.player.View()
		return nil
	}},
	{keys: []string{"+", "="}, help: "faster", group: groupReplay, replay: true, do: func(m *Model) tea.Cmd {
		m.player.Faster()
		return nil
	}},
	{keys: []string{"-"}, help: "slower", group: groupReplay, replay: true, do: func(m *Model) tea.Cmd {
		m.player.Slower()
		return nil
	}},
	{keys: []string{"f"}, help: "follow a recording that is still being written", group: groupReplay, replay: true, do: func(m *Model) tea.Cmd {
		p := m.player
		p.Seek(p.Len())
		p.SetFollow(!p.Follow())
		m.sess = p.View()
		return nil
	}},
	{keys: []string{"/"}, help: "search the recording for a phrase", group: groupReplay, replay: true, do: func(m *Model) tea.Cmd {
		m.searching, m.typing = true, true
		m.input.Prompt = " / "
		m.input.Placeholder = "search the recording; Enter finds, Esc cancels"
		m.input.SetValue(m.query)
		m.input.CursorEnd()
		return m.input.Focus()
	}},
	{keys: []string{"n"}, help: "the next match", group: groupReplay, replay: true, do: func(m *Model) tea.Cmd {
		m.find(1)
		return nil
	}},
	{keys: []string{"N"}, help: "the previous match", group: groupReplay, replay: true, do: func(m *Model) tea.Cmd {
		m.find(-1)
		return nil
	}},
	{keys: []string{"q", "ctrl+c"}, help: "quit", group: groupReplay, replay: true, do: func(*Model) tea.Cmd { return tea.Quit }},
}

// filesKeys are the keys of the file list, after o; others fall through
// to the main screen's.
var filesKeys = []binding{
	{keys: []string{"up", "k"}, help: "the previous file", group: groupFiles, do: func(m *Model) tea.Cmd {
		m.fileSel = max(m.fileSel-1, 0)
		return nil
	}},
	{keys: []string{"down", "j"}, help: "the next file", group: groupFiles, do: func(m *Model) tea.Cmd {
		m.fileSel = min(m.fileSel+1, max(len(m.sess.Files())-1, 0))
		return nil
	}},
	{keys: []string{"enter"}, help: "open it", group: groupFiles, do: func(m *Model) tea.Cmd {
		if files := m.sess.Files(); m.fileSel < len(files) {
			return m.openFile(files[m.fileSel].Path)
		}
		return nil
	}},
	{keys: []string{"esc", "o"}, help: "back to the conversation", group: groupFiles, do: func(m *Model) tea.Cmd {
		m.filesFocus = false
		return nil
	}},
}

// popupKeys are the keys of the file viewer.
var popupKeys = []binding{
	{keys: []string{"up", "k"}, help: "scroll up", group: groupFile, do: func(m *Model) tea.Cmd {
		m.popup.scroll = max(m.popup.scroll-1, 0)
		return nil
	}},
	{keys: []string{"down", "j"}, help: "scroll down", group: groupFile, do: func(m *Model) tea.Cmd {
		m.popup.scroll++
		return nil
	}},
	{keys: []string{"pgup"}, help: "scroll up a page", group: groupFile, do: func(m *Model) tea.Cmd {
		m.popup.scroll = max(m.popup.scroll-m.height/2, 0)
		return nil
	}},
	{keys: []string{"pgdown", "space"}, help: "scroll down a page", group: groupFile, do: func(m *Model) tea.Cmd {
		m.popup.scroll += m.height / 2
		return nil
	}},
	{keys: []string{"home", "g"}, help: "the top", group: groupFile, do: func(m *Model) tea.Cmd {
		m.popup.scroll = 0
		return nil
	}},
	{keys: []string{"esc", "q", "enter", "o"}, help: "close it", group: groupFile, do: func(m *Model) tea.Cmd {
		m.popup = nil
		return nil
	}},
	{keys: []string{"ctrl+c"}, hidden: true, do: func(*Model) tea.Cmd { return tea.Quit }},
}

// helpKeys are the keys of the help panel.
var helpKeys = []binding{
	{keys: []string{"up", "k"}, help: "scroll up", group: groupHelp, do: func(m *Model) tea.Cmd {
		m.helpScroll = max(m.helpScroll-1, 0)
		return nil
	}},
	{keys: []string{"down", "j"}, help: "scroll down", group: groupHelp, do: func(m *Model) tea.Cmd {
		m.helpScroll++
		return nil
	}},
	{keys: []string{"?", "esc", "q"}, help: "close it", group: groupHelp, do: func(m *Model) tea.Cmd {
		m.help = false
		return nil
	}},
	{keys: []string{"ctrl+c"}, hidden: true, do: func(*Model) tea.Cmd { return tea.Quit }},
}

// keyNames show keys in the help.
var keyNames = map[string]string{
	"up": "↑", "down": "↓", "left": "←", "right": "→", "pgup": "PgUp", "pgdown": "PgDn", "enter": "Enter", "esc": "Esc",
	"tab": "Tab", "shift+tab": "Shift+Tab", "space": "Space", "home": "Home", "end": "End", "ctrl+c": "Ctrl+C",
}

// keysLabel shows a binding's keys.
func (b binding) keysLabel() string {
	if b.label != "" {
		return b.label
	}
	names := make([]string, len(b.keys))
	for i, k := range b.keys {
		if n, ok := keyNames[k]; ok {
			k = n
		}
		names[i] = k
	}
	return strings.Join(names, " ")
}

// helpLines are the help panel's lines: every key that applies to this
// viewer, by group, from the tables that handle them.
func (m *Model) helpLines() []string {
	st := m.st
	var out []string
	for _, g := range groups {
		var rows []string
		for _, table := range [][]binding{mainKeys, filesKeys, popupKeys, helpKeys} {
			for _, b := range table {
				if b.group != g || b.hidden || !b.applies(m) {
					continue
				}
				rows = append(rows, "  "+st.key.Render(padTo(b.keysLabel(), 12))+" "+st.text.Render(b.help))
			}
		}
		if len(rows) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, st.accent.Render(g))
		out = append(out, rows...)
	}
	return out
}

func padTo(s string, w int) string {
	if n := len([]rune(s)); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}
