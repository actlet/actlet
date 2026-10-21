package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestHighlight(t *testing.T) {
	st := newStyles(ThemeDark)
	tests := []struct {
		lang, file, text string
		want             string // a styled token that must appear
	}{
		{"json", "", "{\n  \"id\": \"sum\",\n  \"n\": 3\n}", st.synKey.Render(`"id"`)},
		{"yaml", "", "a: 1 # one\nb: \"s\"", st.synComment.Render("# one")},
		{"yaml", "", "list:\n  - name: x\n    n: 2", st.synKey.Render("name")},
		{"", "main.go", "func main() { fmt.Println(`x`, 0x1f) } // done", st.synFunc.Render("Println")},
		{"", "main.py", "def f():\n\treturn 'x'", st.synKeyword.Render("def")},
		{"", "change.diff", "@@ -1 +1 @@\n-a\n+b", st.add.Render("+b")},
		{"", "notes.unknown", "plain text", ""},
	}
	for _, tt := range tests {
		lang := tt.lang
		if lang == "" {
			lang = languageOf(tt.file)
		}
		lines := st.highlight(lang, tt.text)
		if got := ansi.Strip(strings.Join(lines, "\n")); got != tt.text {
			t.Errorf("%s%s: the text changed: %q", tt.lang, tt.file, got)
		}
		if tt.want != "" && !strings.Contains(strings.Join(lines, "\n"), tt.want) {
			t.Errorf("%s%s: no %q in %q", tt.lang, tt.file, tt.want, lines)
		}
		// Indentation stays plain, so wrapping keeps it.
		for _, l := range lines {
			if strings.HasPrefix(ansi.Strip(l), " ") && !strings.HasPrefix(l, " ") {
				t.Errorf("styled indentation in %q", l)
			}
		}
	}
	for i := range 2 * maxHighlightCache {
		st.highlight("json", fmt.Sprintf(`{"n": %d}`, i))
	}
	if len(st.hlCache) > maxHighlightCache {
		t.Errorf("the cache holds %d texts", len(st.hlCache))
	}
}

// The task, profile and file viewer are highlighted; the plain theme
// draws them without colour.
func TestHighlightedTabs(t *testing.T) {
	m := liveModel()
	for _, tab := range []int{TabTask, TabProfile} {
		m.SetTab(tab)
		if !strings.Contains(m.DumpColor(), m.st.synKey.Render(`"model"`)) && !strings.Contains(m.DumpColor(), m.st.synKey.Render(`"id"`)) {
			t.Errorf("%s isn't highlighted", tabNames[tab])
		}
	}
	m.SetTheme(ThemePlain)
	if strings.Contains(m.DumpColor(), "\x1b[38;") {
		t.Error("plain has colours")
	}
}
