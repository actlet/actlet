package tui

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// A small highlighter of our own, line by line: keys, strings, numbers,
// keywords, comments, calls and punctuation in JSON, YAML and the files a
// session usually holds. It is cheap and has no lexer tables to load,
// since every box carries the viewer.

// maxHighlight bounds the text highlighted at once; longer text is shown
// plain.
const maxHighlight = 1 << 20

// maxHighlightCache bounds the highlighted texts kept, so that a tab
// redrawn many times a second highlights its text once.
const maxHighlightCache = 32

// language is how to highlight a kind of text.
type language struct {
	name     string
	comment  []string // line comment starts
	keywords []string
	// constants such as true and null, shown as keywords.
	constants []string
	quotes    string // characters that quote strings
	keys      bool   // "key": in JSON, key: in YAML and TOML
	diff      bool
	markdown  bool
}

var languages = map[string]*language{
	"json": {name: "json", keys: true, quotes: `"`, constants: []string{"true", "false", "null"}},
	"yaml": {name: "yaml", keys: true, quotes: `"'`, comment: []string{"#"}, constants: []string{"true", "false", "null", "yes", "no", "on", "off"}},
	"toml": {name: "toml", keys: true, quotes: `"'`, comment: []string{"#"}, constants: []string{"true", "false"}},
	"python": {name: "python", quotes: `"'`, comment: []string{"#"}, constants: []string{"None", "True", "False"},
		keywords: []string{"def", "class", "import", "from", "return", "for", "in", "if", "elif", "else", "while", "with", "as", "try",
			"except", "finally", "raise", "and", "or", "not", "is", "lambda", "yield", "pass", "break", "continue", "async", "await", "global"}},
	"go": {name: "go", quotes: "\"'`", comment: []string{"//"}, constants: []string{"nil", "true", "false", "iota"},
		keywords: []string{"package", "import", "func", "return", "for", "range", "if", "else", "var", "const", "type", "struct",
			"interface", "go", "defer", "switch", "case", "default", "map", "chan", "select", "break", "continue", "fallthrough", "goto"}},
	"js": {name: "js", quotes: "\"'`", comment: []string{"//"}, constants: []string{"null", "undefined", "true", "false", "this"},
		keywords: []string{"function", "const", "let", "var", "return", "if", "else", "for", "while", "do", "import", "export", "from",
			"class", "new", "async", "await", "try", "catch", "finally", "throw", "switch", "case", "default", "typeof", "of", "in",
			"interface", "type", "extends", "implements"}},
	"shell": {name: "shell", quotes: `"'`, comment: []string{"#"},
		keywords: []string{"if", "then", "else", "elif", "fi", "for", "do", "done", "while", "until", "case", "esac", "in", "function",
			"export", "local", "return", "echo", "set", "exit"}},
	"sql": {name: "sql", quotes: `'"`, comment: []string{"--"}, constants: []string{"NULL", "TRUE", "FALSE", "null", "true", "false"},
		keywords: []string{"SELECT", "FROM", "WHERE", "AND", "OR", "NOT", "INSERT", "INTO", "VALUES", "UPDATE", "SET", "DELETE",
			"CREATE", "TABLE", "JOIN", "LEFT", "ON", "GROUP", "BY", "ORDER", "AS", "LIMIT", "select", "from", "where", "and", "or",
			"insert", "into", "values", "update", "set", "delete", "create", "table", "join", "on", "group", "by", "order", "as", "limit"}},
	"diff":     {name: "diff", diff: true},
	"markdown": {name: "markdown", markdown: true},
}

// languageOf names the language of a file by its name.
func languageOf(file string) string {
	switch strings.ToLower(path.Ext(file)) {
	case ".json", ".jsonl", ".ndjson":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".py":
		return "python"
	case ".go":
		return "go"
	case ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx":
		return "js"
	case ".sh", ".bash", ".zsh":
		return "shell"
	case ".sql":
		return "sql"
	case ".diff", ".patch":
		return "diff"
	case ".md", ".markdown":
		return "markdown"
	}
	return ""
}

var (
	reNumber = regexp.MustCompile(`^-?(?:0[xX][0-9a-fA-F]+|\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)`)
	reIdent  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
	// A YAML or TOML key, after its indentation and any list dash.
	reKey = regexp.MustCompile(`^(\s*(?:- )?)([A-Za-z0-9_.\-/$@]+|"[^"]*"|'[^']*')(\s*[:=])(\s|$)`)
)

// highlight colours text, already rid of escape sequences, as the
// language lang, and returns its lines. Whitespace stays unstyled, so
// that indentation can still be read off a line.
func (st *styles) highlight(lang, text string) []string {
	l := languages[lang]
	if l == nil || len(text) > maxHighlight {
		return strings.Split(text, "\n")
	}
	key := lang + "\x00" + text
	if lines, ok := st.hlCache[key]; ok {
		return lines
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = st.highlightLine(l, line)
	}
	if len(st.hlCache) >= maxHighlightCache {
		clear(st.hlCache)
	}
	st.hlCache[key] = lines
	return lines
}

func (st *styles) highlightLine(l *language, line string) string {
	switch {
	case l.diff:
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			return st.bold.Render(line)
		}
		return st.diffStyle(line).Render(line)
	case l.markdown:
		if strings.HasPrefix(line, "#") {
			return st.accent.Render(line)
		}
		return st.markdownLine(line)
	}
	var b strings.Builder
	rest := line
	if l.keys && l.name != "json" {
		if m := reKey.FindStringSubmatch(rest); m != nil {
			b.WriteString(m[1])
			b.WriteString(st.synKey.Render(m[2]))
			b.WriteString(st.synPunct.Render(strings.TrimLeft(m[3], " ")))
			b.WriteString(m[3][:len(m[3])-len(strings.TrimLeft(m[3], " "))] + m[4])
			rest = rest[len(m[0]):]
		}
	}
	for rest != "" {
		c := rest[0]
		switch {
		case c == ' ' || c == '\t':
			i := len(rest) - len(strings.TrimLeft(rest, " \t"))
			b.WriteString(rest[:i])
			rest = rest[i:]
		case l.isComment(rest):
			b.WriteString(st.synComment.Render(rest))
			rest = ""
		case strings.IndexByte(l.quotes, c) >= 0:
			end := quoted(rest)
			s := st.synString
			// In JSON, a string followed by a colon is a key.
			if l.name == "json" && strings.HasPrefix(strings.TrimLeft(rest[end:], " "), ":") {
				s = st.synKey
			}
			b.WriteString(s.Render(rest[:end]))
			rest = rest[end:]
		case c >= '0' && c <= '9' || c == '-' && len(rest) > 1 && rest[1] >= '0' && rest[1] <= '9':
			m := reNumber.FindString(rest)
			if m == "" {
				m = rest[:1]
			}
			b.WriteString(st.synNumber.Render(m))
			rest = rest[len(m):]
		case c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z':
			w := reIdent.FindString(rest)
			after := rest[len(w):]
			switch {
			case slices.Contains(l.keywords, w) || slices.Contains(l.constants, w):
				b.WriteString(st.synKeyword.Render(w))
			case strings.HasPrefix(after, "("):
				b.WriteString(st.synFunc.Render(w))
			default:
				b.WriteString(w)
			}
			rest = after
		case strings.IndexByte("{}[]():;,.=<>+-*/%!&|^~?", c) >= 0:
			b.WriteString(st.synPunct.Render(rest[:1]))
			rest = rest[1:]
		default:
			// Up to the next character that may start something.
			i := 1
			for i < len(rest) && rest[i] >= 0x80 {
				i++
			}
			b.WriteString(rest[:i])
			rest = rest[i:]
		}
	}
	return b.String()
}

func (l *language) isComment(s string) bool {
	for _, c := range l.comment {
		if strings.HasPrefix(s, c) {
			return true
		}
	}
	return false
}

// quoted is the length of the string that starts s, to its closing
// quote or the end of the line.
func quoted(s string) int {
	q := s[0]
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case q:
			return i + 1
		}
	}
	return len(s)
}

// markdownLine colours a Markdown line's code spans.
func (st *styles) markdownLine(line string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(line, '`')
		if i < 0 {
			break
		}
		j := strings.IndexByte(line[i+1:], '`')
		if j < 0 {
			break
		}
		b.WriteString(line[:i])
		b.WriteString(st.synString.Render(line[i : i+j+2]))
		line = line[i+j+2:]
	}
	b.WriteString(line)
	return b.String()
}
