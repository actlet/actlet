package tui

import (
	"context"
	"fmt"
	"path"
	"regexp"
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

// renderPopup draws the open file over the body: a panel with a border,
// centred.
func (m *Model) renderPopup(w, h int) []string {
	st := m.st
	p := m.popup
	boxW, boxH := max(w-4, 20), max(h-2, 5)
	innerW, inner := boxW-4, boxH-4 // border and padding; title, rule and foot
	if p.styled == nil || p.styledW != innerW {
		p.styled, p.styledW = m.popupLines(p, innerW), innerW
	}
	body := p.styled
	if len(body) == 0 {
		body = []string{st.muted.Render(p.note)}
	}
	p.scroll = max(0, min(p.scroll, len(body)-inner))
	end := min(p.scroll+inner, len(body))
	foot := st.muted.Render(p.note)
	if len(body) > inner {
		foot += st.faint.Render(fmt.Sprintf(" · lines %d-%d of %d", p.scroll+1, end, len(body)))
	}
	foot += st.faint.Render(" · esc closes")
	rows := []string{st.accent.Render(p.path), st.border.Render(strings.Repeat("─", innerW))}
	rows = append(rows, block(body[p.scroll:end], innerW, inner, st.panelBg)...)
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

// popupLines are the open file's lines, numbered and highlighted, w
// cells wide.
func (m *Model) popupLines(p *popup, w int) []string {
	st := m.st
	var out []string
	for i, l := range p.lines {
		if p.diff {
			out = append(out, st.diffStyle(l).Render(ansi.Truncate(l, w, "…")))
			continue
		}
		num := st.faint.Render(fmt.Sprintf("%4d ", i+1))
		text := ansi.Truncate(strings.ReplaceAll(l, "\t", "    "), w-6, "…")
		out = append(out, num+st.highlight(p.path, text, i))
	}
	return out
}

var (
	reString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`)
	reWord   = regexp.MustCompile(`\b[A-Za-z_]+\b`)
	keywords = map[string][]string{
		".py":   {"def", "class", "import", "from", "return", "for", "in", "if", "elif", "else", "while", "with", "as", "try", "except", "raise", "None", "True", "False", "and", "or", "not", "lambda", "yield", "pass"},
		".go":   {"package", "import", "func", "return", "for", "range", "if", "else", "var", "const", "type", "struct", "interface", "go", "defer", "switch", "case", "nil", "true", "false", "map", "chan"},
		".sh":   {"if", "then", "else", "fi", "for", "do", "done", "while", "case", "esac", "echo", "export", "function", "in"},
		".js":   {"function", "const", "let", "var", "return", "if", "else", "for", "while", "import", "export", "from", "class", "new", "null", "true", "false", "async", "await"},
		".ts":   {"function", "const", "let", "var", "return", "if", "else", "for", "while", "import", "export", "from", "class", "new", "null", "true", "false", "async", "await", "interface", "type"},
		".yaml": {"true", "false", "null"},
	}
)

// highlight colours a line cheaply by the file's kind: comments,
// strings, keywords, Markdown headings, a CSV header.
func (st *styles) highlight(p, l string, n int) string {
	ext := strings.ToLower(path.Ext(p))
	switch ext {
	case ".md":
		if strings.HasPrefix(l, "#") {
			return st.bold.Render(l)
		}
		return l
	case ".csv", ".tsv":
		if n == 0 {
			return st.bold.Render(l)
		}
		return l
	case ".yml":
		ext = ".yaml"
	}
	comment := ""
	switch ext {
	case ".py", ".sh", ".yaml", ".toml":
		comment = "#"
	case ".go", ".js", ".ts", ".java", ".c", ".rs":
		comment = "//"
	}
	if comment != "" {
		if i := strings.Index(l, comment); i >= 0 && !inString(l, i) {
			return st.highlightCode(l[:i], ext) + st.synComment.Render(l[i:])
		}
	}
	if kw := keywords[ext]; kw != nil || ext == ".json" {
		return st.highlightCode(l, ext)
	}
	return l
}

func inString(l string, i int) bool {
	for _, loc := range reString.FindAllStringIndex(l, -1) {
		if i >= loc[0] && i < loc[1] {
			return true
		}
	}
	return false
}

func (st *styles) highlightCode(l, ext string) string {
	kw := keywords[ext]
	var b strings.Builder
	last := 0
	for _, loc := range reString.FindAllStringIndex(l, -1) {
		b.WriteString(st.highlightWords(l[last:loc[0]], kw))
		b.WriteString(st.synString.Render(l[loc[0]:loc[1]]))
		last = loc[1]
	}
	b.WriteString(st.highlightWords(l[last:], kw))
	return b.String()
}

func (st *styles) highlightWords(s string, kw []string) string {
	if len(kw) == 0 {
		return s
	}
	return reWord.ReplaceAllStringFunc(s, func(w string) string {
		for _, k := range kw {
			if w == k {
				return st.synKeyword.Render(w)
			}
		}
		return w
	})
}
