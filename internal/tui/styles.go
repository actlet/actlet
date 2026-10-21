package tui

import (
	"image/color"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// styles are a theme's styles, made once when the theme is set rather
// than for every line drawn.
type styles struct {
	theme Theme
	plain bool // no colours: emphasis by attributes only

	// Background escape sequences of the screen, of panels and of a
	// selected row, which paint keeps under styled text.
	bg, panelBg, selBg string

	text, muted, faint, bold, accent lipgloss.Style
	ok, err, warn                    lipgloss.Style
	add, del, hunk                   lipgloss.Style
	border                           lipgloss.Style

	// The header: the logo, the active and other tabs, the attach and
	// replay badges.
	logo, tabOn, tabOff, badge, replayBadge lipgloss.Style
	// Speakers: their bar and their name, by speaker.
	speaker map[string]speakerStyle
	// A selected row, and the sidebar's selected worker.
	selected, selMarker lipgloss.Style
	sideHead            lipgloss.Style
	// The status line's state, by kind, and its other segments.
	state             map[string]lipgloss.Style
	segment, segFaint lipgloss.Style
	key               lipgloss.Style // keys of key-value lines
	popupBorder       lipgloss.Style

	// Syntax highlighting.
	synKeyword, synString, synNumber, synComment, synKey, synFunc, synPunct lipgloss.Style
}

type speakerStyle struct{ bar, name lipgloss.Style }

// Speakers.
const (
	spAgent    = "agent"
	spSubagent = "subagent"
	spOperator = "operator"
)

// States of the status line.
const (
	stRunning = "running"
	stDone    = "done"
	stFailed  = "failed"
	stPaused  = "paused"
)

// colorOf turns a theme colour into a lipgloss colour.
func colorOf(c string) color.Color {
	if c == "" || c == "none" {
		return lipgloss.NoColor{}
	}
	return lipgloss.Color(c)
}

// bgSeq is the escape sequence that sets a background colour, or "".
func bgSeq(c string) string {
	if c == "" || c == "none" {
		return ""
	}
	return ansi.Style{}.BackgroundColor(lipgloss.Color(c)).String()
}

func newStyles(t Theme) *styles {
	p := t.Colors
	fg := func(c string) lipgloss.Style { return lipgloss.NewStyle().Foreground(colorOf(c)) }
	// A pill: dark text on a colour, or reverse video without colours.
	pill := func(c string) lipgloss.Style {
		if c == "" || c == "none" {
			return lipgloss.NewStyle().Reverse(true).Bold(true).Padding(0, 1)
		}
		on := p.Background
		if on == "" || on == "none" {
			on = "#000000"
		}
		return lipgloss.NewStyle().Background(colorOf(c)).Foreground(colorOf(on)).Bold(true).Padding(0, 1)
	}
	s := &styles{theme: t, bg: bgSeq(p.Background), panelBg: bgSeq(p.Panel), selBg: bgSeq(p.Selection)}
	s.plain = true
	for _, c := range p.fields() {
		if *c != "" && *c != "none" {
			s.plain = false
		}
	}
	s.text, s.muted, s.faint = fg(p.Text), fg(p.Muted), fg(p.Faint)
	s.bold = fg(p.Text).Bold(true)
	s.accent = fg(p.Accent).Bold(true)
	s.ok, s.err, s.warn = fg(p.OK), fg(p.Error), fg(p.Warning)
	s.add, s.del, s.hunk = fg(p.OK), fg(p.Error), fg(p.Subagent)
	s.border = fg(p.Border)
	s.logo = pill(p.Accent)
	s.tabOn = pill(p.Accent)
	s.tabOff = fg(p.Muted).Padding(0, 1)
	s.badge = pill(p.Attach)
	s.replayBadge = pill(p.Subagent)
	s.speaker = map[string]speakerStyle{}
	for name, c := range map[string]string{spAgent: p.Agent, spSubagent: p.Subagent, spOperator: p.Operator} {
		s.speaker[name] = speakerStyle{bar: fg(c), name: fg(c).Bold(true)}
	}
	s.selected = lipgloss.NewStyle().Background(colorOf(p.Selection))
	s.selMarker = fg(p.Accent).Bold(true)
	s.sideHead = fg(p.Accent).Bold(true)
	s.state = map[string]lipgloss.Style{
		stRunning: pill(p.Warning), stDone: pill(p.OK), stFailed: pill(p.Error), stPaused: pill(p.Operator),
	}
	s.segment = fg(p.Text)
	s.segFaint = fg(p.Faint)
	s.key = fg(p.Key)
	s.popupBorder = fg(p.Border)
	s.synKeyword, s.synString, s.synNumber = fg(p.Keyword), fg(p.String), fg(p.Number)
	s.synComment, s.synKey, s.synFunc, s.synPunct = fg(p.Comment).Italic(true), fg(p.Key), fg(p.Function), fg(p.Punctuation)
	if s.plain {
		s.tabOff = lipgloss.NewStyle().Padding(0, 1)
		s.selected = lipgloss.NewStyle().Reverse(true)
		s.selBg = ansi.Style{}.Reverse(true).String()
		s.accent = lipgloss.NewStyle().Bold(true)
		s.sideHead = lipgloss.NewStyle().Bold(true).Underline(true)
		s.synComment = lipgloss.NewStyle()
	}
	return s
}

// inputStyles are the note input's styles in the theme.
func (st *styles) inputStyles(s textinput.Styles) textinput.Styles {
	s.Focused.Prompt, s.Blurred.Prompt = st.accent, st.accent
	s.Focused.Text, s.Blurred.Text = st.text, st.text
	s.Focused.Placeholder, s.Blurred.Placeholder = st.faint, st.faint
	return s
}

// paint lays a line out w cells wide on the background bg: styled text
// within it keeps the background where its style ends.
func paint(l string, w int, bg string) string {
	l = ansi.Truncate(l, w, "")
	pad := strings.Repeat(" ", max(w-ansi.StringWidth(l), 0))
	if bg == "" {
		return l + pad
	}
	l = strings.ReplaceAll(l, "\x1b[m", "\x1b[m"+bg)
	l = strings.ReplaceAll(l, "\x1b[0m", "\x1b[0m"+bg)
	return bg + l + pad + "\x1b[m"
}

// block lays lines out as a box w cells wide and h high on bg.
func block(lines []string, w, h int, bg string) []string {
	out := make([]string, h)
	for i := range out {
		l := ""
		if i < len(lines) {
			l = lines[i]
		}
		out[i] = paint(l, w, bg)
	}
	return out
}
