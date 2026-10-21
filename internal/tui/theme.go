package tui

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/actlet/actlet/themes"
)

// Theme is the viewer's colours and emphasis, by token. A theme file
// gives some tokens and takes the rest from its base. The format is
// plain data, shared with other viewers.
type Theme struct {
	Name string `json:"name,omitempty"`
	// Base is the built-in theme a theme file changes: dark unless it
	// says otherwise. Built-in themes give every token and have none.
	Base string `json:"base,omitempty"`
	// Colors are "#rrggbb", "#rgb", an ANSI colour number from 0 to 255,
	// or "none" for the terminal's own, by token.
	Colors map[string]string `json:"colors,omitempty"`
	// Emphasis adds attributes to tokens: bold, italic, underline and
	// reverse.
	Emphasis map[string][]string `json:"emphasis,omitempty"`
}

// Tokens are the theme's colour tokens, in the order the docs list them.
var Tokens = []string{
	"background", "panel", "selection", "border", "text", "muted", "faint", "accent",
	"agent", "subagent", "operator", "ok", "error", "warning", "attach",
	"added", "removed", "hunk",
	"keyword", "string", "number", "comment", "key", "function", "punctuation",
}

// Attributes are the emphasis a token may have.
var Attributes = []string{"bold", "italic", "underline", "reverse"}

// Built-in themes, from the themes package's data files.
var (
	ThemeDark  = builtin("dark")
	ThemeLight = builtin("light")
	// ThemePlain has no colours, for NO_COLOR: emphasis is bold,
	// underline and reverse video.
	ThemePlain = builtin("plain")
)

// BuiltinThemes are the themes every viewer has, by name.
var BuiltinThemes = map[string]Theme{"dark": ThemeDark, "light": ThemeLight, "plain": ThemePlain}

func builtin(name string) Theme {
	data, err := themes.FS.ReadFile(name + ".yaml")
	if err != nil {
		panic(err)
	}
	var t Theme
	if err := yaml.UnmarshalStrict(data, &t); err != nil {
		panic(fmt.Sprintf("theme %s: %v", name, err))
	}
	if err := t.check(); err != nil {
		panic(fmt.Sprintf("theme %s: %v", name, err))
	}
	for _, tok := range Tokens {
		if _, ok := t.Colors[tok]; !ok {
			panic(fmt.Sprintf("theme %s: no %s", name, tok))
		}
	}
	return t
}

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
	if err := t.check(); err != nil {
		return Theme{}, err
	}
	out := Theme{Name: t.Name, Colors: maps.Clone(base.Colors), Emphasis: maps.Clone(base.Emphasis)}
	maps.Copy(out.Colors, t.Colors)
	if out.Emphasis == nil {
		out.Emphasis = map[string][]string{}
	}
	maps.Copy(out.Emphasis, t.Emphasis)
	return out, nil
}

// check reports a token, colour or attribute that isn't one.
func (t Theme) check() error {
	for tok, c := range t.Colors {
		if !slices.Contains(Tokens, tok) {
			return fmt.Errorf("colors: unknown token %q", tok)
		}
		if err := checkColor(c); err != nil {
			return fmt.Errorf("colors.%s: %w", tok, err)
		}
	}
	for tok, attrs := range t.Emphasis {
		if !slices.Contains(Tokens, tok) {
			return fmt.Errorf("emphasis: unknown token %q", tok)
		}
		for _, a := range attrs {
			if !slices.Contains(Attributes, a) {
				return fmt.Errorf("emphasis.%s: unknown attribute %q: use bold, italic, underline or reverse", tok, a)
			}
		}
	}
	return nil
}

func checkColor(c string) error {
	if c == "none" || reColor.MatchString(c) {
		return nil
	}
	if n, err := strconv.Atoi(c); err == nil && n >= 0 && n <= 255 {
		return nil
	}
	return fmt.Errorf("%q isn't a colour: give #rrggbb, #rgb, 0 to 255 or none", c)
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
