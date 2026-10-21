// Package ansisvg draws text with ANSI colour escape sequences, such as
// a terminal viewer's screen dumped with -dump -color, as an SVG image of
// the terminal: no browser or terminal needed, so screenshots can be made
// again from the same input at any time.
//
// It understands the select graphic rendition sequences a viewer writes:
// 24-bit, 256 and 16 colours, bold, italic, underline, reverse video and
// their resets. Other escape sequences are dropped.
package ansisvg

import (
	"bytes"
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Options lay the picture out.
type Options struct {
	// FontSize is in pixels; a cell is 0.6 of it wide and 1.25 of it high.
	FontSize float64
	// Foreground and Background are the terminal's own colours, as
	// #rrggbb, for text that sets none.
	Foreground, Background string
	// Padding is the margin around the screen, in pixels.
	Padding float64
	// Title, if set, is drawn in a window bar above the screen.
	Title string
}

func (o *Options) defaults() {
	if o.FontSize == 0 {
		o.FontSize = 14
	}
	if o.Foreground == "" {
		o.Foreground = "#DCDCE6"
	}
	if o.Background == "" {
		o.Background = "#17171F"
	}
}

// style is a cell's look.
type style struct {
	fg, bg                           string // "" is the terminal's
	bold, italic, underline, reverse bool
}

type cell struct {
	r     string // the grapheme
	width int
	st    style
}

// parse turns text into rows of cells.
func parse(text string) [][]cell {
	var rows [][]cell
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		var row []cell
		var st style
		p := ansi.NewParser()
		state := byte(0)
		for len(line) > 0 {
			seq, width, n, newState := ansi.DecodeSequence(line, state, p)
			state = newState
			switch {
			case width > 0:
				row = append(row, cell{r: seq, width: width, st: st})
			case ansi.HasCsiPrefix(seq) && ansi.Cmd(p.Command()).Final() == 'm':
				st = st.apply(p.Params())
			}
			line = line[n:]
		}
		rows = append(rows, row)
	}
	return rows
}

// apply applies an SGR sequence's parameters.
func (s style) apply(params ansi.Params) style {
	if len(params) == 0 {
		return style{}
	}
	for i := 0; i < len(params); i++ {
		p := params[i].Param(0)
		switch {
		case p == 0:
			s = style{}
		case p == 1:
			s.bold = true
		case p == 3:
			s.italic = true
		case p == 4:
			s.underline = true
		case p == 7:
			s.reverse = true
		case p == 22:
			s.bold = false
		case p == 23:
			s.italic = false
		case p == 24:
			s.underline = false
		case p == 27:
			s.reverse = false
		case p == 39:
			s.fg = ""
		case p == 49:
			s.bg = ""
		case p >= 30 && p <= 37:
			s.fg = basic[p-30]
		case p >= 90 && p <= 97:
			s.fg = basic[p-90+8]
		case p >= 40 && p <= 47:
			s.bg = basic[p-40]
		case p >= 100 && p <= 107:
			s.bg = basic[p-100+8]
		case p == 38 || p == 48:
			var c string
			c, i = extended(params, i)
			if p == 38 {
				s.fg = c
			} else {
				s.bg = c
			}
		}
	}
	return s
}

// extended reads a 38 or 48 colour at params[i] and returns it and the
// index of its last parameter.
func extended(params ansi.Params, i int) (string, int) {
	if i+1 >= len(params) {
		return "", i
	}
	switch params[i+1].Param(0) {
	case 2:
		if i+4 < len(params) {
			return fmt.Sprintf("#%02X%02X%02X", params[i+2].Param(0), params[i+3].Param(0), params[i+4].Param(0)), i + 4
		}
	case 5:
		if i+2 < len(params) {
			return indexed(params[i+2].Param(0)), i + 2
		}
	}
	return "", i + 1
}

var basic = []string{"#000000", "#CD3131", "#0DBC79", "#E5E510", "#2472C8", "#BC3FBC", "#11A8CD", "#E5E5E5",
	"#666666", "#F14C4C", "#23D18B", "#F5F543", "#3B8EEA", "#D670D6", "#29B8DB", "#FFFFFF"}

// indexed is a 256-colour palette entry.
func indexed(n int) string {
	switch {
	case n < 16:
		return basic[max(n, 0)]
	case n < 232:
		n -= 16
		level := func(v int) int {
			if v == 0 {
				return 0
			}
			return 55 + v*40
		}
		return fmt.Sprintf("#%02X%02X%02X", level(n/36), level(n/6%6), level(n%6))
	case n < 256:
		v := 8 + (n-232)*10
		return fmt.Sprintf("#%02X%02X%02X", v, v, v)
	}
	return ""
}

// Render draws text as an SVG image.
func Render(text string, o Options) []byte {
	o.defaults()
	rows := parse(text)
	cols := 0
	for _, r := range rows {
		w := 0
		for _, c := range r {
			w += c.width
		}
		cols = max(cols, w)
	}
	cw, ch := o.FontSize*0.6, o.FontSize*1.25
	bar := 0.0
	if o.Title != "" {
		bar = ch * 1.6
	}
	width := float64(cols)*cw + 2*o.Padding
	height := float64(len(rows))*ch + 2*o.Padding + bar
	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%s" height="%s" viewBox="0 0 %s %s">`+"\n",
		num(width), num(height), num(width), num(height))
	fmt.Fprintf(&b, `<rect width="100%%" height="100%%" rx="8" fill="%s"/>`+"\n", o.Background)
	if o.Title != "" {
		fmt.Fprintf(&b, `<g font-family="ui-sans-serif, -apple-system, 'Segoe UI', sans-serif" font-size="%s">`, num(o.FontSize*0.9))
		for i, c := range []string{"#FF5F57", "#FEBC2E", "#28C840"} {
			fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="%s" fill="%s"/>`, num(o.Padding+8+float64(i)*18), num(bar/2+2), num(5.5), c)
		}
		fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="middle" fill="%s" opacity="0.6">%s</text></g>`+"\n",
			num(width/2), num(bar/2+6), o.Foreground, html.EscapeString(o.Title))
	}
	fmt.Fprintf(&b, `<g font-family="ui-monospace, SFMono-Regular, Menlo, Consolas, 'DejaVu Sans Mono', monospace" font-size="%s" xml:space="preserve" style="white-space:pre">`+"\n", num(o.FontSize))
	for y, row := range rows {
		top := o.Padding + bar + float64(y)*ch
		// Backgrounds first, in runs of one colour, then text in runs of
		// one style, each run stretched to exactly its cells.
		x := 0
		for i := 0; i < len(row); {
			_, bg := colours(row[i].st, o)
			j, w := i, 0
			for ; j < len(row); j++ {
				_, bg2 := colours(row[j].st, o)
				if bg2 != bg {
					break
				}
				w += row[j].width
			}
			if bg != o.Background {
				fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" fill="%s"/>`+"\n",
					num(o.Padding+float64(x)*cw), num(top), num(float64(w)*cw+0.5), num(ch+0.5), bg)
			}
			x += w
			i = j
		}
		x = 0
		for i := 0; i < len(row); {
			st := row[i].st
			j, w := i, 0
			var text strings.Builder
			for ; j < len(row) && row[j].st == st; j++ {
				text.WriteString(row[j].r)
				w += row[j].width
			}
			if s := strings.TrimRight(text.String(), " "); s != "" {
				fg, _ := colours(st, o)
				attrs := ""
				if st.bold {
					attrs += ` font-weight="bold"`
				}
				if st.italic {
					attrs += ` font-style="italic"`
				}
				if st.underline {
					attrs += ` text-decoration="underline"`
				}
				lead := len(s) - len(strings.TrimLeft(s, " "))
				cells := w - (len(text.String()) - len(s)) - lead
				fmt.Fprintf(&b, `<text x="%s" y="%s" fill="%s" textLength="%s" lengthAdjust="spacingAndGlyphs"%s>%s</text>`+"\n",
					num(o.Padding+float64(x+lead)*cw), num(top+ch*0.78), fg, num(float64(cells)*cw), attrs, html.EscapeString(strings.TrimLeft(s, " ")))
			}
			x += w
			i = j
		}
	}
	b.WriteString("</g>\n</svg>\n")
	return b.Bytes()
}

// colours are a style's text and background colours.
func colours(s style, o Options) (fg, bg string) {
	fg, bg = s.fg, s.bg
	if fg == "" {
		fg = o.Foreground
	}
	if bg == "" {
		bg = o.Background
	}
	if s.reverse {
		fg, bg = bg, fg
	}
	return fg, bg
}

// num writes a number briefly, to two decimals.
func num(f float64) string { return strconv.FormatFloat(math.Round(f*100)/100, 'f', -1, 64) }
