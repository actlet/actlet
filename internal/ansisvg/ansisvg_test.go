package ansisvg

import (
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	rows := parse("\x1b[1;38;2;255;0;0;48;5;21mab\x1b[mc\x1b[7;32md\x1b[27;39m界\n\x1b]0;title\x07e")
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	r := rows[0]
	want := []struct {
		s     string
		width int
		st    style
	}{
		{"a", 1, style{fg: "#FF0000", bg: "#0000FF", bold: true}},
		{"b", 1, style{fg: "#FF0000", bg: "#0000FF", bold: true}},
		{"c", 1, style{}},
		{"d", 1, style{fg: "#0DBC79", reverse: true}},
		{"界", 2, style{}},
	}
	if len(r) != len(want) {
		t.Fatalf("row %+v", r)
	}
	for i, w := range want {
		if r[i].r != w.s || r[i].width != w.width || r[i].st != w.st {
			t.Errorf("cell %d: %+v, want %+v", i, r[i], w)
		}
	}
	// Other escape sequences, such as a window title, are dropped.
	if len(rows[1]) != 1 || rows[1][0].r != "e" {
		t.Errorf("second row %+v", rows[1])
	}
}

// The image is well-formed XML, its text escaped, its colours and cells
// where the screen has them.
func TestRender(t *testing.T) {
	svg := string(Render("\x1b[48;2;1;2;3m  <a & b>  \x1b[m\n\x1b[7mrev\x1b[m", Options{Title: "t & u", Padding: 10}))
	dec := xml.NewDecoder(strings.NewReader(svg))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("not XML: %v\n%s", err, svg)
		}
	}
	for _, want := range []string{
		`fill="#010203"`,                   // the background run
		`&lt;a &amp; b&gt;</text>`,         // escaped, without its spaces
		`textLength="58.8"`,                // 7 cells of 8.4 pixels
		`fill="#17171F" textLength="25.2"`, // reverse video: the background's colour on the text's
		`>t &amp; u</text>`,
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("no %s in\n%s", want, svg)
		}
	}
}

func TestIndexed(t *testing.T) {
	for n, want := range map[int]string{1: "#CD3131", 16: "#000000", 21: "#0000FF", 231: "#FFFFFF", 232: "#080808", 255: "#EEEEEE"} {
		if got := indexed(n); got != want {
			t.Errorf("indexed(%d) = %s, want %s", n, got, want)
		}
	}
}
