package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// Theme is the viewer's colours. A theme file gives some of them and
// takes the rest from its base.
type Theme struct {
	Name   string  `json:"name,omitempty"`
	Base   string  `json:"base,omitempty"`
	Colors Palette `json:"colors"`
}

// Palette is a theme's colours: each "#rrggbb", "#rgb", an ANSI colour
// number from 0 to 255, or "none" for the terminal's own.
type Palette struct {
	// The screen, and the panels on it: the header, the sidebar, the
	// status line and popups.
	Background string `json:"background,omitempty"`
	Panel      string `json:"panel,omitempty"`
	// The selected tool call, file or worker.
	Selection string `json:"selection,omitempty"`
	Border    string `json:"border,omitempty"`
	Text      string `json:"text,omitempty"`
	Muted     string `json:"muted,omitempty"`
	Faint     string `json:"faint,omitempty"`
	// The title, the active tab and the input prompt.
	Accent string `json:"accent,omitempty"`
	// Speakers.
	Agent    string `json:"agent,omitempty"`
	Subagent string `json:"subagent,omitempty"`
	Operator string `json:"operator,omitempty"`
	// States: done, failed, and running or warned.
	OK      string `json:"ok,omitempty"`
	Error   string `json:"error,omitempty"`
	Warning string `json:"warning,omitempty"`
	// The attach badge.
	Attach string `json:"attach,omitempty"`
	// Syntax highlighting.
	Keyword     string `json:"keyword,omitempty"`
	String      string `json:"string,omitempty"`
	Number      string `json:"number,omitempty"`
	Comment     string `json:"comment,omitempty"`
	Key         string `json:"key,omitempty"`
	Function    string `json:"function,omitempty"`
	Punctuation string `json:"punctuation,omitempty"`
}

// Built-in themes.
var (
	ThemeDark = Theme{Name: "dark", Colors: Palette{
		Background: "#17171F", Panel: "#21212C", Selection: "#33304A", Border: "#3A3950",
		Text: "#DCDCE6", Muted: "#8E8EA6", Faint: "#5A5A72", Accent: "#A48BFA",
		Agent: "#A48BFA", Subagent: "#5FB8E6", Operator: "#F2B65E",
		OK: "#5FD192", Error: "#F2687C", Warning: "#E9C46A", Attach: "#F28FB0",
		Keyword: "#C79BF2", String: "#A3D982", Number: "#F5A86E", Comment: "#6B6B85",
		Key: "#7FAAF7", Function: "#5ED6E3", Punctuation: "#8E8EA6",
	}}
	ThemeLight = Theme{Name: "light", Colors: Palette{
		Background: "#FBFBFD", Panel: "#EEEFF5", Selection: "#DCD8F7", Border: "#CACCDA",
		Text: "#2A2C38", Muted: "#676A7E", Faint: "#A2A5B5", Accent: "#6A45E8",
		Agent: "#6A45E8", Subagent: "#1C74B4", Operator: "#B0630C",
		OK: "#23854A", Error: "#C8304A", Warning: "#946500", Attach: "#C2306E",
		Keyword: "#7A35D9", String: "#2E7D32", Number: "#B5520B", Comment: "#8C8FA0",
		Key: "#1D4FD6", Function: "#0B7488", Punctuation: "#676A7E",
	}}
	// ThemePlain has no colours, for NO_COLOR: emphasis is bold,
	// underline and reverse video.
	ThemePlain = Theme{Name: "plain"}
)

// BuiltinThemes are the themes every viewer has, by name.
var BuiltinThemes = map[string]Theme{"dark": ThemeDark, "light": ThemeLight, "plain": ThemePlain}

// maxThemeFile bounds a theme file.
const maxThemeFile = 64 << 10

var reColor = regexp.MustCompile(`^#(?:[0-9A-Fa-f]{3}|[0-9A-Fa-f]{6})$`)

// ParseTheme reads a theme file's YAML or JSON, on top of its base
// (dark unless it names another built-in theme).
func ParseTheme(data []byte) (Theme, error) {
	var t Theme
	if err := yaml.UnmarshalStrict(data, &t); err != nil {
		return Theme{}, err
	}
	base := ThemeDark
	if t.Base != "" {
		b, ok := BuiltinThemes[t.Base]
		if !ok {
			return Theme{}, fmt.Errorf("base %q isn't a built-in theme: dark, light or plain", t.Base)
		}
		base = b
	}
	for _, c := range t.Colors.fields() {
		if err := checkColor(*c); err != nil {
			return Theme{}, err
		}
	}
	base.Colors.merge(t.Colors)
	base.Name = t.Name
	return base, nil
}

func checkColor(c string) error {
	if c == "" || c == "none" || reColor.MatchString(c) {
		return nil
	}
	if n, err := strconv.Atoi(c); err == nil && n >= 0 && n <= 255 {
		return nil
	}
	return fmt.Errorf("%q isn't a colour: give #rrggbb, #rgb, 0 to 255 or none", c)
}

// fields are the palette's colours, to check and merge them.
func (p *Palette) fields() []*string {
	return []*string{&p.Background, &p.Panel, &p.Selection, &p.Border, &p.Text, &p.Muted, &p.Faint, &p.Accent,
		&p.Agent, &p.Subagent, &p.Operator, &p.OK, &p.Error, &p.Warning, &p.Attach,
		&p.Keyword, &p.String, &p.Number, &p.Comment, &p.Key, &p.Function, &p.Punctuation}
}

// merge takes the colours over gives.
func (p *Palette) merge(over Palette) {
	dst, src := p.fields(), over.fields()
	for i, c := range src {
		if *c != "" {
			*dst[i] = *c
		}
	}
}

// ThemesDir is where a viewer finds theme files by name:
// $XDG_CONFIG_HOME/actlet/themes, or ~/.config/actlet/themes.
func ThemesDir(getenv func(string) string) string {
	if d := getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "actlet", "themes")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "actlet", "themes")
	}
	return ""
}

// LoadTheme finds a theme: a built-in one by name, a file by name in
// dir (name.yaml, name.yml or name.json), or, if pathOK, a file by its
// path.
func LoadTheme(name, dir string, pathOK bool) (Theme, error) {
	if t, ok := BuiltinThemes[name]; ok {
		return t, nil
	}
	if name == "" {
		return Theme{}, errors.New("no theme named")
	}
	isPath := strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") || filepath.Ext(name) != ""
	if isPath {
		if !pathOK {
			return Theme{}, fmt.Errorf("theme %q: a profile names a theme, not a path", name)
		}
		return readTheme(name)
	}
	if dir == "" {
		return Theme{}, fmt.Errorf("no theme %q", name)
	}
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		t, err := readTheme(filepath.Join(dir, name+ext))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return t, err
	}
	return Theme{}, fmt.Errorf("no theme %q: not built in, nor in %s", name, dir)
}

func readTheme(path string) (Theme, error) {
	f, err := os.Open(path)
	if err != nil {
		return Theme{}, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxThemeFile+1))
	if err != nil {
		return Theme{}, err
	}
	if len(data) > maxThemeFile {
		return Theme{}, fmt.Errorf("theme %s is larger than %d KiB", path, maxThemeFile>>10)
	}
	t, err := ParseTheme(data)
	if err != nil {
		return Theme{}, fmt.Errorf("theme %s: %w", path, err)
	}
	if t.Name == "" {
		t.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return t, nil
}
