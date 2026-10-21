package tui

import (
	"strconv"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Look is how a viewer looks, as its own flags and environment choose
// it. What they leave open, the profile's viewer settings choose when the
// viewer attaches.
type Look struct {
	Theme Theme
	// Separators, if set, draws or hides the lines between turns and
	// tool calls whatever the profile says; nil leaves it to the profile,
	// and they are on if it says nothing.
	Separators *bool
	// Notice says why a theme or setting asked for couldn't be used.
	Notice string

	// themeSet says that the flag, ACTLET_THEME or NO_COLOR chose the
	// theme, so the profile's doesn't apply.
	themeSet bool
	// themesDir is where the profile's theme is looked for.
	themesDir string
}

// BoolFlag is a boolean flag that knows whether it was given.
type BoolFlag struct {
	v, set bool
}

// IsBoolFlag implements flag's boolFlag: -name alone means true.
func (b *BoolFlag) IsBoolFlag() bool { return true }

func (b *BoolFlag) String() string {
	if b == nil || !b.set {
		return ""
	}
	return strconv.FormatBool(b.v)
}

// Set implements flag.Value.
func (b *BoolFlag) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	b.v, b.set = v, true
	return nil
}

// Value is the flag's value, or nil if it wasn't given.
func (b *BoolFlag) Value() *bool {
	if !b.set {
		return nil
	}
	v := b.v
	return &v
}

// ResolveLook chooses how the viewer looks from its flags and the
// environment.
//
// The theme is the -theme flag (a built-in theme, a theme file's name or
// its path), else plain if NO_COLOR is set, else ACTLET_THEME, else the
// profile's, else dark. A theme that can't be loaded falls back to dark,
// with a notice.
//
// The separators are the -separators flag, else ACTLET_TUI_SEPARATORS,
// else the profile's, else on.
func ResolveLook(theme string, separators *bool, getenv func(string) string) Look {
	l := Look{Theme: ThemeDark, Separators: separators, themesDir: ThemesDir(getenv)}
	if l.Separators == nil {
		if v := getenv("ACTLET_TUI_SEPARATORS"); v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				l.Separators = &b
			} else {
				l.Notice = "ACTLET_TUI_SEPARATORS: give true or false"
			}
		}
	}
	switch {
	case theme != "":
	case getenv("NO_COLOR") != "":
		l.Theme, l.themeSet = ThemePlain, true
		return l
	default:
		theme = getenv("ACTLET_THEME")
	}
	if theme == "" {
		return l
	}
	l.themeSet = true
	t, err := LoadTheme(theme, l.themesDir, true)
	if err != nil {
		l.Notice = "theme: " + err.Error() + "; using dark"
		return l
	}
	l.Theme = t
	return l
}

// SetLook sets how the viewer looks.
func (m *Model) SetLook(l Look) {
	m.look = l
	m.SetTheme(l.Theme)
	m.separators = l.Separators == nil || *l.Separators
	if l.Notice != "" {
		m.notice = l.Notice
	}
}

// applyProfile takes the profile's viewer settings for what the viewer's
// own flags and environment leave open, once, when the viewer attaches.
func (m *Model) applyProfile(s *gatewayv0.ViewerSettings) {
	if m.profileApplied {
		return
	}
	m.profileApplied = true
	if s != nil && s.Separators != nil && m.look.Separators == nil {
		m.separators = s.GetSeparators()
	}
	if name := s.GetTheme(); name != "" && !m.look.themeSet {
		t, err := LoadTheme(name, m.look.themesDir, false)
		if err != nil {
			m.notice = "the profile's theme: " + err.Error() + "; using dark"
			t = ThemeDark
		}
		m.SetTheme(t)
	}
}
