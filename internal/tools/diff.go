package tools

import (
	"fmt"
	"slices"
	"strings"
)

// maxDiffEdits bounds the work of finding a minimal diff. Beyond it, the
// changed middle of the file is shown as removed and added whole: still a
// correct diff, only not a minimal one.
const maxDiffEdits = 1000

// maxDiff bounds the size of a diff kept for activity and the model.
const maxDiff = 64 << 10

// unifiedDiff returns a unified diff from old to new, with three lines of
// context, or "" if they are equal.
func unifiedDiff(path, old, new string) string {
	if old == new {
		return ""
	}
	a, b := splitLines(old), splitLines(new)
	ops := diffLines(a, b)
	var sb strings.Builder
	from, to := "a/"+path, "b/"+path
	if old == "" {
		from = "/dev/null"
	}
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", from, to)
	writeHunks(&sb, ops, a, b, 3)
	return truncate(sb.String(), maxDiff)
}

// splitLines splits s into lines, each keeping its newline.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type opKind byte

const (
	opEqual opKind = ' '
	opDel   opKind = '-'
	opIns   opKind = '+'
)

// op is one line of the edit script: an index into a (equal, delete) or b
// (insert).
type op struct {
	kind opKind
	a, b int
}

// diffLines returns an edit script from a to b.
func diffLines(a, b []string) []op {
	// Common prefix and suffix first: most edits are small.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var ops []op
	for i := range pre {
		ops = append(ops, op{opEqual, i, i})
	}
	ops = append(ops, myers(a[pre:len(a)-suf], b[pre:len(b)-suf], pre, pre)...)
	for i := range suf {
		ops = append(ops, op{opEqual, len(a) - suf + i, len(b) - suf + i})
	}
	return ops
}

// myers finds a shortest edit script with Myers' algorithm, giving up
// after maxDiffEdits edits. offA and offB offset the indices.
func myers(a, b []string, offA, offB int) []op {
	n, m := len(a), len(b)
	whole := func() []op {
		var ops []op
		for i := range n {
			ops = append(ops, op{opDel, offA + i, offB})
		}
		for j := range m {
			ops = append(ops, op{opIns, offA + n, offB + j})
		}
		return ops
	}
	if n == 0 || m == 0 {
		return whole()
	}
	maxD := min(n+m, maxDiffEdits)
	off := maxD + 1
	v := make([]int, 2*maxD+3)
	var trace [][]int
	found := -1
	for d := 0; d <= maxD && found < 0; d++ {
		trace = append(trace, slices.Clone(v))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[off+k] = x
			if x >= n && y >= m {
				found = d
				break
			}
		}
	}
	if found < 0 {
		return whole()
	}
	// Walk back through the trace.
	var rev []op
	x, y := n, m
	for d := found; d > 0; d-- {
		vp := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && vp[off+k-1] < vp[off+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := vp[off+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x, y = x-1, y-1
			rev = append(rev, op{opEqual, offA + x, offB + y})
		}
		if x == prevX {
			y--
			rev = append(rev, op{opIns, offA + x, offB + y})
		} else {
			x--
			rev = append(rev, op{opDel, offA + x, offB + y})
		}
	}
	for x > 0 && y > 0 {
		x, y = x-1, y-1
		rev = append(rev, op{opEqual, offA + x, offB + y})
	}
	ops := make([]op, len(rev))
	for i, o := range rev {
		ops[len(rev)-1-i] = o
	}
	return ops
}

// writeHunks writes the edit script as unified diff hunks.
func writeHunks(sb *strings.Builder, ops []op, a, b []string, context int) {
	for i := 0; i < len(ops); {
		// Find the next change.
		for i < len(ops) && ops[i].kind == opEqual {
			i++
		}
		if i == len(ops) {
			return
		}
		start := max(i-context, 0)
		// Extend the hunk while changes are close together.
		end := i
		for end < len(ops) {
			if ops[end].kind != opEqual {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == opEqual {
				run++
			}
			if run == len(ops) || run-end > 2*context {
				end = min(end+context, len(ops))
				break
			}
			end = run
		}
		var aLen, bLen int
		for _, o := range ops[start:end] {
			switch o.kind {
			case opEqual:
				aLen++
				bLen++
			case opDel:
				aLen++
			case opIns:
				bLen++
			}
		}
		first := ops[start]
		aStart, bStart := first.a+1, first.b+1
		if aLen == 0 {
			aStart--
		}
		if bLen == 0 {
			bStart--
		}
		fmt.Fprintf(sb, "@@ -%s +%s @@\n", hunkRange(aStart, aLen), hunkRange(bStart, bLen))
		for _, o := range ops[start:end] {
			line := ""
			if o.kind == opIns {
				line = b[o.b]
			} else {
				line = a[o.a]
			}
			sb.WriteByte(byte(o.kind))
			sb.WriteString(line)
			if !strings.HasSuffix(line, "\n") {
				sb.WriteString("\n\\ No newline at end of file\n")
			}
		}
		i = end
	}
}

func hunkRange(start, n int) string {
	if n == 1 {
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, n)
}
