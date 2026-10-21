package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/actlet/actlet/internal/attach"
	"github.com/actlet/actlet/internal/view"
	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
)

// FileReader reads a file of the session from the box, given relative
// to the session folder.
type FileReader func(path string) (*attachv0.ReadFileResponse, error)

// Files returns a FileReader for the box. When the box can't be reached,
// such as after the session ended, it reads the session folder dir
// itself, if it is given.
func Files(ctx context.Context, client attachv0.AttachClient, dir string) FileReader {
	return func(p string) (*attachv0.ReadFileResponse, error) {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req := &attachv0.ReadFileRequest{Path: view.ToolPath(p)}
		resp, err := client.ReadFile(cctx, req)
		if status.Code(err) == codes.Unavailable && dir != "" {
			return attach.ReadFile(dir, req)
		}
		return resp, err
	}
}

// SetFileReader lets the viewer open files from the box.
func (m *Model) SetFileReader(r FileReader) { m.readFile = r }

// popup is an open file, shown read only.
type popup struct {
	path   string
	lines  []string
	note   string
	scroll int
	diff   bool
	// styled are its lines as drawn styledW wide, made once.
	styled  []string
	styledW int
}

// fileMsg is a file read from the box.
type fileMsg struct {
	path string
	resp *attachv0.ReadFileResponse
	err  error
}

// openFile opens one of the changed files: from the box when it is live,
// or from what the recording holds.
func (m *Model) openFile(p string) tea.Cmd {
	if m.readFile != nil {
		m.popup = &popup{path: p, note: "loading…"}
		read := m.readFile
		return func() tea.Msg {
			resp, err := read(p)
			return fileMsg{path: p, resp: resp, err: err}
		}
	}
	m.popup = recordedFile(m.sess, p)
	return nil
}

// Open opens a file in the popup at once, waiting for the box if it is
// live: for rendering without a terminal.
func (m *Model) Open(p string) {
	if m.readFile == nil {
		m.popup = recordedFile(m.sess, p)
		return
	}
	m.popup = &popup{path: p}
	resp, err := m.readFile(p)
	m.showFile(fileMsg{path: p, resp: resp, err: err})
}

// showFile fills the popup with a file read from the box.
func (m *Model) showFile(msg fileMsg) {
	if m.popup == nil || m.popup.path != msg.path {
		return
	}
	m.popup.styled = nil
	switch {
	case msg.err != nil:
		m.popup.lines, m.popup.note = nil, "can't read it: "+msg.err.Error()
	case msg.resp.GetBinary():
		m.popup.lines, m.popup.note = nil, fmt.Sprintf("a binary file of %s", sizeOf(msg.resp.GetSize()))
	default:
		m.popup.lines = strings.Split(strings.TrimRight(view.Sanitize(string(msg.resp.GetContent())), "\n"), "\n")
		m.popup.note = fmt.Sprintf("%s · %s · read only", msg.resp.GetPath(), sizeOf(msg.resp.GetSize()))
		if msg.resp.GetTruncated() {
			m.popup.note += fmt.Sprintf(" · the first %s shown", sizeOf(int64(len(msg.resp.GetContent()))))
		}
	}
}

// recordedFile is what a recording holds of a file: the whole of it if
// it was created in one go, or else its latest change.
func recordedFile(s *view.Session, p string) *popup {
	h := s.FileHistory(p)
	pp := &popup{path: p}
	switch {
	case len(h) == 0:
		pp.note = "not in the recording"
	case len(h) == 1 && h[0].Created && !strings.Contains(h[0].Diff, "bytes omitted") && !strings.HasSuffix(h[0].Diff, "[cut]"):
		for _, l := range strings.Split(strings.TrimRight(h[0].Diff, "\n"), "\n") {
			if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
				pp.lines = append(pp.lines, l[1:])
			}
		}
		pp.note = "from the recording · read only"
	default:
		last := h[len(h)-1]
		for _, l := range strings.Split(strings.TrimRight(last.Diff, "\n"), "\n") {
			if !strings.HasPrefix(l, "+++") && !strings.HasPrefix(l, "---") {
				pp.lines = append(pp.lines, l)
			}
		}
		pp.diff = true
		pp.note = fmt.Sprintf("the recording holds only changes: the latest of %d", len(h))
	}
	return pp
}

// renderPopup draws the open file over the body.
func (m *Model) renderPopup(w, h int) []string {
	st := m.st
	p := m.popup
	innerW := panelWidth(w) - 4
	if p.styled == nil || p.styledW != innerW {
		p.styled, p.styledW = m.popupLines(p, innerW), innerW
	}
	body := p.styled
	if len(body) == 0 {
		body = []string{st.muted.Render(p.note)}
	}
	return m.panel(w, h, p.path, body, &p.scroll, p.note)
}

// panelWidth is how wide a panel is on a screen w wide.
func panelWidth(w int) int { return max(w-4, 20) }

// panel draws a bordered panel over the body, centred: a title, lines
// scrolled to *scroll, and a note at the foot.
func (m *Model) panel(w, h int, title string, body []string, scroll *int, note string) []string {
	st := m.st
	boxW, boxH := panelWidth(w), max(h-2, 5)
	innerW, inner := boxW-4, boxH-4 // border and padding; title, rule and foot
	*scroll = max(0, min(*scroll, len(body)-inner))
	end := min(*scroll+inner, len(body))
	foot := st.muted.Render(note)
	if len(body) > inner {
		foot += st.muted.Render(fmt.Sprintf(" · lines %d-%d of %d", *scroll+1, end, len(body)))
	}
	foot += st.muted.Render(" · esc closes")
	rows := []string{st.accent.Render(title), st.border.Render(strings.Repeat("─", innerW))}
	rows = append(rows, block(body[*scroll:end], innerW, inner, st.panelBg)...)
	rows = append(rows, foot)
	bar := func(l, r string) string { return st.popupBorder.Render(l + strings.Repeat("─", boxW-2) + r) }
	side := st.popupBorder.Render("│")
	box := []string{bar("╭", "╮")}
	for _, r := range rows {
		box = append(box, side+paint(" "+r, boxW-2, st.panelBg)+side)
	}
	box = append(box, bar("╰", "╯"))
	top := max((h-len(box))/2, 0)
	left := strings.Repeat(" ", max((w-boxW)/2, 0))
	out := make([]string, top, h)
	for _, l := range box {
		out = append(out, left+l)
	}
	return out
}

// popupLines are the open file's lines, numbered and highlighted by its
// kind, w cells wide.
func (m *Model) popupLines(p *popup, w int) []string {
	st := m.st
	var out []string
	if p.diff {
		for _, l := range p.lines {
			out = append(out, st.diffStyle(l).Render(ansi.Truncate(l, w, "…")))
		}
		return out
	}
	text := strings.ReplaceAll(strings.Join(p.lines, "\n"), "\t", "    ")
	for i, l := range st.highlight(languageOf(p.path), text) {
		out = append(out, st.muted.Render(fmt.Sprintf("%4d ", i+1))+ansi.Truncate(l, w-6, "…"))
	}
	return out
}
