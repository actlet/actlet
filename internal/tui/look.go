package tui

// Look is how a viewer looks, as its own flags and environment choose
// it.
type Look struct {
	Theme Theme
	// Notice says why a theme asked for couldn't be used.
	Notice string
}

// ResolveLook chooses the viewer's theme: the -theme flag (a built-in
// theme, a theme file's name or its path), else no colours if NO_COLOR
// is set, else ACTLET_THEME, else dark. A theme that can't be loaded
// falls back to dark, with a notice.
func ResolveLook(theme string, getenv func(string) string) Look {
	l := Look{Theme: ThemeDark}
	switch {
	case theme != "":
	case getenv("NO_COLOR") != "":
		l.Theme = ThemePlain
		return l
	default:
		theme = getenv("ACTLET_THEME")
	}
	if theme == "" {
		return l
	}
	t, err := LoadTheme(theme, ThemesDir(getenv), true)
	if err != nil {
		l.Notice = "theme: " + err.Error() + "; using dark"
		return l
	}
	l.Theme = t
	return l
}

// SetLook sets how the viewer looks.
func (m *Model) SetLook(l Look) {
	m.SetTheme(l.Theme)
	if l.Notice != "" {
		m.notice = l.Notice
	}
}
