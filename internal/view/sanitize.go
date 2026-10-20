package view

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sanitize makes untrusted text safe to show in a terminal: it removes
// escape sequences (CSI, OSC, DCS and the like) and other control
// characters, keeping newlines and tabs, and replaces invalid UTF-8.
// Carriage returns are dropped, so text can't overwrite what came before
// it on a line.
func Sanitize(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f || c >= 0x80 {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			b.WriteRune(utf8.RuneError)
			i++
		case r == 0x1b: // ESC: skip the whole sequence
			i += escapeLen(s[i:])
		case r == 0x9b: // C1 CSI
			i += size + csiLen(s[i+size:])
		case r == 0x9d || r == 0x90 || r == 0x98 || r == 0x9e || r == 0x9f: // C1 OSC, DCS, SOS, PM, APC
			i += size + stringLen(s[i+size:])
		case r == '\n' || r == '\t':
			b.WriteRune(r)
			i += size
		case unicode.IsControl(r) || r == 0x7f:
			i += size // other C0 and C1 controls, including \r
		case r == '‮' || r == '‭' || r == '⁦' || r == '⁧' || r == '⁨' || r == '⁩' || r == '‪' || r == '‫' || r == '‬':
			i += size // bidirectional overrides
		default:
			b.WriteString(s[i : i+size])
			i += size
		}
	}
	return b.String()
}

// escapeLen returns the length of the escape sequence at the start of s,
// which starts with ESC.
func escapeLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch s[1] {
	case '[': // CSI
		return 2 + csiLen(s[2:])
	case ']', 'P', 'X', '^', '_': // OSC, DCS, SOS, PM, APC: up to ST or BEL
		return 2 + stringLen(s[2:])
	}
	// Two-character sequences, and ones with intermediate bytes.
	n := 1
	for n < len(s) && s[n] >= 0x20 && s[n] <= 0x2f {
		n++
	}
	if n < len(s) {
		n++
	}
	return n
}

// csiLen returns the length of a CSI sequence's parameters and final byte.
func csiLen(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x40 && s[i] <= 0x7e {
			return i + 1
		}
		if s[i] < 0x20 || s[i] > 0x3f && s[i] < 0x40 {
			return i // malformed: stop here
		}
	}
	return len(s)
}

// stringLen returns the length of a control string up to and including
// its terminator: BEL, ESC \ or the C1 ST.
func stringLen(s string) int {
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == 0x07:
			return i + 1
		case s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\':
			return i + 2
		case s[i] == 0xc2 && i+1 < len(s) && s[i+1] == 0x9c:
			return i + 2
		}
	}
	return len(s)
}
