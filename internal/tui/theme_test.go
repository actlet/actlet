package tui

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

func TestParseTheme(t *testing.T) {
	tests := []struct {
		name, data string
		check      func(Theme) bool
		err        string
	}{
		{"partial overrides dark", "colors:\n  accent: \"#ff0000\"\n", func(th Theme) bool {
			return th.Colors["accent"] == "#ff0000" && th.Colors["text"] == ThemeDark.Colors["text"]
		}, ""},
		{"on a light base", "base: light\ncolors:\n  ok: \"2\"\n", func(th Theme) bool {
			return th.Colors["ok"] == "2" && th.Colors["background"] == ThemeLight.Colors["background"]
		}, ""},
		{"none clears a colour", "colors:\n  background: none\n", func(th Theme) bool { return th.Colors["background"] == "none" }, ""},
		{"json", `{"name": "j", "colors": {"key": "#abc"}}`, func(th Theme) bool { return th.Name == "j" && th.Colors["key"] == "#abc" }, ""},
		{"bad colour", "colors:\n  accent: red\n", nil, "isn't a colour"},
		{"out of range", "colors:\n  accent: \"256\"\n", nil, "isn't a colour"},
		{"unknown colour", "colors:\n  acent: \"#fff\"\n", nil, "unknown token"},
		{"unknown field", "colours:\n  accent: \"#fff\"\n", nil, "unknown field"},
		{"emphasis", "emphasis:\n  key: [bold, underline]\n", func(th Theme) bool {
			return slices.Equal(th.Emphasis["key"], []string{"bold", "underline"}) && slices.Equal(th.Emphasis["comment"], []string{"italic"})
		}, ""},
		{"unknown attribute", "emphasis:\n  key: [blink]\n", nil, "unknown attribute"},
		{"emphasis of no token", "emphasis:\n  kee: [bold]\n", nil, "unknown token"},
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
	if th, err := LoadTheme("mine", dir, false); err != nil || th.Name != "mine" || th.Colors["accent"] != "#123456" {
		t.Errorf("mine: %+v %v", th, err)
	}
	if th, err := LoadTheme(path, dir, true); err != nil || th.Colors["background"] != ThemeLight.Colors["background"] {
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
		l := ResolveLook(tt.flag, nil, func(k string) string { return env[k] })
		if l.Theme.Name != tt.theme || (l.Notice != "") != tt.notice {
			t.Errorf("flag %q env %v: theme %q notice %q", tt.flag, tt.env, l.Theme.Name, l.Notice)
		}
	}
}

// Built-in themes give every token, and the plain one no colour.
func TestBuiltinThemes(t *testing.T) {
	for name, th := range BuiltinThemes {
		if th.Name != name || th.Base != "" || len(th.Colors) != len(Tokens) {
			t.Errorf("%s: name %q, base %q, %d colours", name, th.Name, th.Base, len(th.Colors))
		}
	}
	for tok, c := range ThemePlain.Colors {
		if c != "none" {
			t.Errorf("plain colours %s", tok)
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

// Separators: the flag, then the environment, then the profile, then on.
func TestSeparatorsPrecedence(t *testing.T) {
	on, off := true, false
	tests := []struct {
		name    string
		flag    *bool
		env     string
		profile *bool
		want    bool
	}{
		{"default", nil, "", nil, true},
		{"profile", nil, "", &off, false},
		{"env over profile", nil, "1", &off, true},
		{"env", nil, "false", nil, false},
		{"flag over env", &on, "0", &off, true},
		{"flag", &off, "", &on, false},
		{"bad env", nil, "maybe", &off, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := ResolveLook("", tt.flag, func(k string) string {
				if k == "ACTLET_TUI_SEPARATORS" {
					return tt.env
				}
				return ""
			})
			m := liveModel()
			m.SetLook(l)
			m.profileApplied = false
			m.Update(InfoMsg{Info: &attachv0.InfoResponse{Tui: &gatewayv0.ViewerSettings{Separators: tt.profile}}})
			if m.separators != tt.want {
				t.Errorf("separators %v, want %v", m.separators, tt.want)
			}
			if (tt.env == "maybe") != (l.Notice != "") {
				t.Errorf("notice %q", l.Notice)
			}
		})
	}
}

// The profile's theme is a name, applied once when the viewer attaches,
// unless the viewer chose one; a path or an unknown name leaves dark.
func TestProfileTheme(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "actlet", "themes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "actlet", "themes", "mine.yaml"), []byte("base: light\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "evil.yaml")
	if err := os.WriteFile(outside, []byte("base: light\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		profile, flag, want string
		notice              bool
	}{
		{"light", "", "light", false},
		{"mine", "", "mine", false},
		{"light", "plain", "plain", false},
		{outside, "", "dark", true},
		{"../themes/mine", "", "dark", true},
		{"nope", "", "dark", true},
	}
	for _, tt := range tests {
		m := liveModel()
		m.SetLook(ResolveLook(tt.flag, nil, func(k string) string {
			if k == "XDG_CONFIG_HOME" {
				return dir
			}
			return ""
		}))
		m.profileApplied, m.notice = false, ""
		info := &attachv0.InfoResponse{Tui: &gatewayv0.ViewerSettings{Theme: tt.profile}}
		m.Update(InfoMsg{Info: info})
		if m.Theme().Name != tt.want || (m.notice != "") != tt.notice {
			t.Errorf("profile %q, flag %q: theme %q, notice %q", tt.profile, tt.flag, m.Theme().Name, m.notice)
		}
		// Once only: the viewer's choices since then stand.
		m.SetTheme(ThemeDark)
		m.Update(InfoMsg{Info: info})
		if m.Theme().Name != "dark" {
			t.Errorf("profile %q applied again", tt.profile)
		}
	}
}

// The built-in themes keep text readable: at least 4.5:1 contrast, the
// WCAG AA level for text, for every text token on the background and on
// panels, selected text on the selection, and the background's colour on
// every pill.
func TestBuiltinContrast(t *testing.T) {
	text := []string{"text", "muted", "accent", "agent", "subagent", "operator", "ok", "error", "warning", "attach",
		"added", "removed", "hunk", "keyword", "string", "number", "comment", "key", "function", "punctuation"}
	pills := []string{"accent", "attach", "subagent", "warning", "ok", "error", "operator"}
	for _, th := range []Theme{ThemeDark, ThemeLight} {
		c := th.Colors
		check := func(fg, bg string) {
			if r := contrast(c[fg], c[bg]); r < 4.5 {
				t.Errorf("%s: %s on %s is %.2f:1", th.Name, fg, bg, r)
			}
		}
		for _, fg := range text {
			check(fg, "background")
			check(fg, "panel")
		}
		for _, fg := range []string{"text", "accent", "muted"} {
			check(fg, "selection")
		}
		for _, p := range pills {
			check("background", p)
		}
	}
}

// contrast is the WCAG contrast ratio of two #rrggbb colours.
func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func luminance(hex string) float64 {
	var rgb [3]float64
	for i := range rgb {
		v, _ := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		c := float64(v) / 255
		if c <= 0.04045 {
			c /= 12.92
		} else {
			c = math.Pow((c+0.055)/1.055, 2.4)
		}
		rgb[i] = c
	}
	return 0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2]
}

// The status line gives cost and time out of their budgets, coloured as
// they near their limits, and the context out of the model's window.
func TestStatusBudgets(t *testing.T) {
	m := liveModel()
	m.sess.Info.Budgets = append(m.sess.Info.Budgets, &gatewayv0.Budget{Kind: gatewayv0.BudgetKind_BUDGET_KIND_TIME, Limit: 40, AlertAtPercent: 50})
	status := func() string {
		lines := strings.Split(m.DumpColor(), "\n")
		return lines[len(lines)-1]
	}
	plain := ansi.Strip(status())
	for _, want := range []string{"ctx 6%/200k", "0.08/5.00 EUR", "30s/40s"} {
		if !strings.Contains(plain, want) {
			t.Errorf("no %q in %q", want, plain)
		}
	}
	// 30s of 40s is past the alert at 50%.
	if !strings.Contains(status(), m.st.warn.Render("30s")) {
		t.Errorf("time isn't in the warning colour: %q", status())
	}
	if !strings.Contains(status(), m.st.segment.Render("0.08")) {
		t.Errorf("cost isn't plain: %q", status())
	}
	m.SetClock(func() time.Time { return epoch0.Add(time.Minute) })
	if !strings.Contains(status(), m.st.err.Render("1m0s")) {
		t.Errorf("time past its limit isn't in the error colour: %q", ansi.Strip(status()))
	}
	if got := shortDuration(90 * time.Minute); got != "1h30m" {
		t.Errorf("shortDuration %q", got)
	}
}

// The example theme loads, and keeps its text readable as the built-in
// themes do.
func TestExampleTheme(t *testing.T) {
	th, err := LoadTheme("../../examples/themes/ocean.yaml", "", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, fg := range []string{"text", "muted", "accent", "agent", "subagent", "operator", "ok", "error", "warning", "key", "string", "comment"} {
		for _, bg := range []string{"background", "panel"} {
			if r := contrast(th.Colors[fg], th.Colors[bg]); r < 4.5 {
				t.Errorf("%s on %s is %.2f:1", fg, bg, r)
			}
		}
	}
}
