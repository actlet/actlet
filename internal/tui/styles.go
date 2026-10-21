package tui

import (
	"image/color"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// styles are a theme's styles, made once when the theme is set rather
// than for every line drawn.
type styles struct {
	theme Theme

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
	// The sidebar's marker of the worker shown.
	selMarker lipgloss.Style
	sideHead  lipgloss.Style
	// The status line's state, by kind, and its other segments.
	state             map[string]lipgloss.Style
	segment, segFaint lipgloss.Style
	key               lipgloss.Style // keys of key-value lines
	popupBorder       lipgloss.Style

	// Syntax highlighting, and the texts highlighted so far.
	hlCache                                                                 map[string][]string
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
	col := func(tok string) string { return t.Colors[tok] }
	has := func(tok, attr string) bool { return slices.Contains(t.Emphasis[tok], attr) }
	// fg is a token's colour, with its emphasis.
	fg := func(tok string) lipgloss.Style {
		return lipgloss.NewStyle().Foreground(colorOf(col(tok))).
			Bold(has(tok, "bold")).Italic(has(tok, "italic")).Underline(has(tok, "underline")).Reverse(has(tok, "reverse"))
	}
	// A pill: the background's colour on a token's, or reverse video
	// when either has none.
	pill := func(tok string) lipgloss.Style {
		on, c := col("background"), col(tok)
		if c == "" || c == "none" {
			return lipgloss.NewStyle().Reverse(true).Bold(true).Padding(0, 1)
		}
		if on == "" || on == "none" {
			on = "#000000"
		}
		return lipgloss.NewStyle().Background(colorOf(c)).Foreground(colorOf(on)).Bold(true).Padding(0, 1)
	}
	s := &styles{theme: t, hlCache: map[string][]string{}, bg: bgSeq(col("background")), panelBg: bgSeq(col("panel")), selBg: bgSeq(col("selection"))}
	if has("selection", "reverse") {
		s.selBg += ansi.Style{}.Reverse(true).String()
	}
	s.text, s.muted, s.faint = fg("text"), fg("muted"), fg("faint")
	s.bold = fg("text").Bold(true)
	s.accent = fg("accent")
	s.ok, s.err, s.warn = fg("ok"), fg("error"), fg("warning")
	s.add, s.del, s.hunk = fg("added"), fg("removed"), fg("hunk")
	s.border = fg("border")
	s.logo = pill("accent")
	s.tabOn = pill("accent")
	s.tabOff = fg("muted").Padding(0, 1)
	s.badge = pill("attach")
	s.replayBadge = pill("subagent")
	s.speaker = map[string]speakerStyle{}
	for _, name := range []string{spAgent, spSubagent, spOperator} {
		s.speaker[name] = speakerStyle{bar: fg(name), name: fg(name).Bold(true)}
	}
	s.selMarker = fg("accent").Bold(true)
	s.sideHead = fg("accent").Bold(true).Underline(has("accent", "underline") || col("accent") == "none")
	s.state = map[string]lipgloss.Style{
		stRunning: pill("warning"), stDone: pill("ok"), stFailed: pill("error"), stPaused: pill("operator"),
	}
	s.segment = fg("text")
	s.segFaint = fg("faint")
	s.key = fg("key")
	s.popupBorder = fg("border")
	s.synKeyword, s.synString, s.synNumber = fg("keyword"), fg("string"), fg("number")
	s.synComment, s.synKey, s.synFunc, s.synPunct = fg("comment"), fg("key"), fg("function"), fg("punctuation")
	return s
}

// inputStyles are the note input's styles in the theme.
func (st *styles) inputStyles(s textinput.Styles) textinput.Styles {
	s.Focused.Prompt, s.Blurred.Prompt = st.accent, st.accent
	s.Focused.Text, s.Blurred.Text = st.text, st.text
	s.Focused.Placeholder, s.Blurred.Placeholder = st.muted, st.muted
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
