package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestParseTheme(t *testing.T) {
	tests := []struct {
		name, data string
		check      func(Theme) bool
		err        string
	}{
		{"partial overrides dark", "colors:\n  accent: \"#ff0000\"\n", func(th Theme) bool {
			return th.Colors.Accent == "#ff0000" && th.Colors.Text == ThemeDark.Colors.Text
		}, ""},
		{"on a light base", "base: light\ncolors:\n  ok: \"2\"\n", func(th Theme) bool {
			return th.Colors.OK == "2" && th.Colors.Background == ThemeLight.Colors.Background
		}, ""},
		{"none clears a colour", "colors:\n  background: none\n", func(th Theme) bool { return th.Colors.Background == "none" }, ""},
		{"json", `{"name": "j", "colors": {"key": "#abc"}}`, func(th Theme) bool { return th.Name == "j" && th.Colors.Key == "#abc" }, ""},
		{"bad colour", "colors:\n  accent: red\n", nil, "isn't a colour"},
		{"out of range", "colors:\n  accent: \"256\"\n", nil, "isn't a colour"},
		{"unknown colour", "colors:\n  acent: \"#fff\"\n", nil, "unknown field"},
		{"unknown base", "base: neon\n", nil, "isn't a built-in theme"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			th, err := ParseTheme([]byte(tt.data))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("got %v, want an error with %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tt.check(th) {
				t.Errorf("got %+v", th)
			}
		})
	}
}

func TestLoadTheme(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mine.yaml"), []byte("colors:\n  accent: \"#123456\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "other.yml")
	if err := os.WriteFile(path, []byte("base: light\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.yaml")
	if err := os.WriteFile(big, []byte(strings.Repeat("#", maxThemeFile+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if th, err := LoadTheme("light", dir, false); err != nil || th.Name != "light" {
		t.Errorf("light: %v %v", th.Name, err)
	}
	if th, err := LoadTheme("mine", dir, false); err != nil || th.Name != "mine" || th.Colors.Accent != "#123456" {
		t.Errorf("mine: %+v %v", th, err)
	}
	if th, err := LoadTheme(path, dir, true); err != nil || th.Colors.Background != ThemeLight.Colors.Background {
		t.Errorf("by path: %+v %v", th, err)
	}
	for _, name := range []string{path, "../mine", "sub/mine", "mine.yaml", `..\mine`} {
		if _, err := LoadTheme(name, dir, false); err == nil || !strings.Contains(err.Error(), "not a path") {
			t.Errorf("%s without paths: %v", name, err)
		}
	}
	if _, err := LoadTheme("nope", dir, false); err == nil {
		t.Error("an unknown theme loaded")
	}
	if _, err := LoadTheme(big, dir, true); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Errorf("a large theme: %v", err)
	}
}

func TestResolveLook(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "actlet", "themes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "actlet", "themes", "mine.yaml"), []byte("base: light\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		flag   string
		env    map[string]string
		theme  string
		notice bool
	}{
		{"", nil, "dark", false},
		{"", map[string]string{"ACTLET_THEME": "light"}, "light", false},
		{"", map[string]string{"ACTLET_THEME": "mine"}, "mine", false},
		{"", map[string]string{"NO_COLOR": "1", "ACTLET_THEME": "light"}, "plain", false},
		{"light", map[string]string{"NO_COLOR": "1"}, "light", false},
		{"nope", nil, "dark", true},
		{"", map[string]string{"ACTLET_THEME": "/no/such.yaml"}, "dark", true},
	}
	for _, tt := range tests {
		env := map[string]string{"XDG_CONFIG_HOME": dir}
		for k, v := range tt.env {
			env[k] = v
		}
		l := ResolveLook(tt.flag, func(k string) string { return env[k] })
		if l.Theme.Name != tt.theme || (l.Notice != "") != tt.notice {
			t.Errorf("flag %q env %v: theme %q notice %q", tt.flag, tt.env, l.Theme.Name, l.Notice)
		}
	}
}

// Every theme draws the same screen; only the colours differ, and the
// plain theme has none.
func TestThemesRender(t *testing.T) {
	m := liveModel()
	want := m.Dump()
	for name, th := range BuiltinThemes {
		m.SetTheme(th)
		if got := m.Dump(); got != want {
			t.Errorf("%s draws another screen:\n%s", name, got)
		}
		colored := strings.Contains(m.DumpColor(), "\x1b[38;2;")
		if colored != (name != "plain") {
			t.Errorf("%s: colours %v", name, colored)
		}
	}
}

// Painting keeps a background under styled text and lays lines out to
// the width.
func TestPaint(t *testing.T) {
	st := newStyles(ThemeDark)
	got := paint(st.ok.Render("ok")+" then", 10, st.bg)
	if n := strings.Count(got, st.bg); n != 2 { // at the start, and after "ok"
		t.Errorf("the background is set %d times in %q", n, got)
	}
	if w := ansi.StringWidth(got); w != 10 {
		t.Errorf("%q is %d wide", got, w)
	}
	if got := paint("abcdefghijkl", 5, ""); got != "abcde" {
		t.Errorf("cut to %q", got)
	}
}
