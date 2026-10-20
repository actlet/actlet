package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// session lays out a session folder and returns tools working in it.
func session(t *testing.T, o Options) (*Set, string) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"inputs", "workspace", "output", ".actlet"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	o.Workspace = filepath.Join(root, "workspace")
	o.ReadOnly = []string{filepath.Join(root, "inputs"), filepath.Join(root, ".actlet")}
	o.TempDir = t.TempDir()
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

func ctxs(t *testing.T) RunContext {
	return RunContext{Soft: t.Context(), Hard: t.Context()}
}

// call prepares and runs a tool call; a preparation error becomes an error
// result, as the agent reports it.
func call(t *testing.T, s *Set, name string, input any) Result {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Prepare(name, data)
	if err != nil {
		return Result{Output: err.Error(), IsError: true}
	}
	return c.Run(ctxs(t))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestReadFile(t *testing.T) {
	s, root := session(t, Options{})
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, "line "+strconv.Itoa(i))
	}
	write(t, filepath.Join(root, "inputs", "data.txt"), strings.Join(lines, "\n")+"\n")
	write(t, filepath.Join(root, "workspace", "bin"), "a\x00b")

	tests := []struct {
		name  string
		input map[string]any
		want  []string
		err   bool
	}{
		{"whole", map[string]any{"path": "../inputs/data.txt"}, []string{"     1\tline 1\n", "    30\tline 30\n"}, false},
		{"part", map[string]any{"path": "../inputs/data.txt", "offset": 10, "limit": 2}, []string{"    10\tline 10\n    11\tline 11\n", "[lines 10-11 of 30; read on with offset 12]"}, false},
		{"absolute", map[string]any{"path": filepath.Join(root, "inputs", "data.txt"), "offset": 30}, []string{"line 30"}, false},
		{"past the end", map[string]any{"path": "../inputs/data.txt", "offset": 31}, []string{"offset 31 is past the end"}, true},
		{"binary", map[string]any{"path": "bin"}, []string{"binary file"}, true},
		{"missing", map[string]any{"path": "nope.txt"}, []string{"no such file"}, true},
		{"directory", map[string]any{"path": "."}, []string{"is a directory"}, true},
		{"unknown field", map[string]any{"path": "bin", "mode": "fast"}, []string{"unknown field"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := call(t, s, ReadFile, tt.input)
			if r.IsError != tt.err {
				t.Fatalf("IsError = %v: %s", r.IsError, r.Output)
			}
			for _, w := range tt.want {
				if !strings.Contains(r.Output, w) {
					t.Errorf("output lacks %q:\n%s", w, r.Output)
				}
			}
		})
	}
}

func TestWriteAndEdit(t *testing.T) {
	s, root := session(t, Options{})
	ws := filepath.Join(root, "workspace")

	r := call(t, s, WriteFile, map[string]any{"path": "src/a.txt", "content": "one\ntwo\nthree\n"})
	if r.IsError || len(r.Changes) != 1 || !r.Changes[0].Created || !strings.Contains(r.Changes[0].Diff, "+two") {
		t.Fatalf("write: %+v", r)
	}
	r = call(t, s, EditFile, map[string]any{"path": "src/a.txt", "old_string": "two", "new_string": "2"})
	if r.IsError || read(t, filepath.Join(ws, "src", "a.txt")) != "one\n2\nthree\n" {
		t.Fatalf("edit: %+v", r)
	}
	if want := "@@ -1,3 +1,3 @@\n one\n-two\n+2\n three\n"; !strings.Contains(r.Changes[0].Diff, want) || !strings.Contains(r.Output, want) {
		t.Errorf("edit diff:\n%s", r.Changes[0].Diff)
	}
	r = call(t, s, WriteFile, map[string]any{"path": "../output/report.md", "content": "# Report\n"})
	if r.IsError || read(t, filepath.Join(root, "output", "report.md")) != "# Report\n" {
		t.Fatalf("write to output: %+v", r)
	}

	write(t, filepath.Join(ws, "dup.txt"), "x x x")
	refused := []struct {
		name  string
		tool  string
		input map[string]any
		want  string
	}{
		{"write inputs", WriteFile, map[string]any{"path": "../inputs/new.txt", "content": "x"}, "read-only"},
		{"write state", WriteFile, map[string]any{"path": "../.actlet/journal", "content": "x"}, "read-only"},
		{"edit inputs", EditFile, map[string]any{"path": "../inputs/new.txt", "old_string": "a", "new_string": "b"}, "doesn't exist"},
		{"not found", EditFile, map[string]any{"path": "src/a.txt", "old_string": "four", "new_string": "4"}, "doesn't occur"},
		{"ambiguous", EditFile, map[string]any{"path": "dup.txt", "old_string": "x", "new_string": "y"}, "occurs 3 times"},
		{"no content", WriteFile, map[string]any{"path": "a"}, "content is required"},
		{"same", EditFile, map[string]any{"path": "dup.txt", "old_string": "x", "new_string": "x"}, "the same"},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			r := call(t, s, tt.tool, tt.input)
			if !r.IsError || !strings.Contains(r.Output, tt.want) {
				t.Fatalf("got %+v, want an error containing %q", r, tt.want)
			}
		})
	}
	r = call(t, s, EditFile, map[string]any{"path": "dup.txt", "old_string": "x", "new_string": "y", "replace_all": true})
	if r.IsError || read(t, filepath.Join(ws, "dup.txt")) != "y y y" {
		t.Fatalf("replace_all: %+v", r)
	}
}

func TestReadOnlyThroughSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	s, root := session(t, Options{})
	ws := filepath.Join(root, "workspace")
	write(t, filepath.Join(root, "inputs", "data.txt"), "data")
	links := map[string]string{
		"in":       "../inputs",               // a directory
		"data":     "../inputs/data.txt",      // an existing file
		"dangling": "../inputs/not-there-yet", // a file to be created
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(ws, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"in/new.txt", "data", "dangling"} {
		r := call(t, s, WriteFile, map[string]any{"path": p, "content": "x"})
		if !r.IsError || !strings.Contains(r.Output, "read-only") {
			t.Errorf("writing %s: %+v", p, r)
		}
	}
	if read(t, filepath.Join(root, "inputs", "data.txt")) != "data" {
		t.Error("an input changed")
	}
}

func TestFileRecovery(t *testing.T) {
	s, root := session(t, Options{})
	path := filepath.Join(root, "workspace", "f.txt")
	write(t, path, "before\n")
	input, _ := json.Marshal(map[string]any{"path": "f.txt", "old_string": "before", "new_string": "after"})
	c, err := s.Prepare(EditFile, input)
	if err != nil {
		t.Fatal(err)
	}
	// Crashed before the edit: run it again.
	if rec := s.Recover(EditFile, input, c.Check); !rec.Rerun {
		t.Fatalf("unchanged file: %+v", rec)
	}
	c.Run(ctxs(t))
	// Crashed after the edit: it is done.
	if rec := s.Recover(EditFile, input, c.Check); rec.Rerun || rec.Result.IsError || !strings.Contains(rec.Result.Output, "completed before") {
		t.Fatalf("edited file: %+v", rec)
	}
	// Changed by something else: unknown.
	write(t, path, "other\n")
	if rec := s.Recover(EditFile, input, c.Check); rec.Rerun || !rec.Result.Unknown {
		t.Fatalf("changed file: %+v", rec)
	}
	// Running a stale plan refuses.
	if r := c.Run(ctxs(t)); !r.IsError || !strings.Contains(r.Output, "changed since") {
		t.Fatalf("stale plan: %+v", r)
	}

	// Policies of the other tools.
	for name, want := range map[string]bool{ReadFile: true, Glob: true, Grep: true, Shell: false} {
		if rec := s.Recover(name, json.RawMessage(`{}`), nil); rec.Rerun != want || (!want && !rec.Result.Unknown) {
			t.Errorf("%s: %+v", name, rec)
		}
	}
}

func TestDiff(t *testing.T) {
	tests := []struct{ old, new, want string }{
		{"a\nb\nc\n", "a\nb\nc\n", ""},
		{"", "x\n", "--- /dev/null\n+++ b/f\n@@ -0,0 +1 @@\n+x\n"},
		{"x\n", "", "--- a/f\n+++ b/f\n@@ -1 +0,0 @@\n-x\n"},
		{"a\nb\n", "a\nb", "--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n-b\n+b\n\\ No newline at end of file\n"},
		{"1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n", "1\n2\n3\n4\nfive\n6\n7\n8\n9\n10\n", "--- a/f\n+++ b/f\n@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+five\n 6\n 7\n 8\n"},
	}
	for _, tt := range tests {
		if got := unifiedDiff("f", tt.old, tt.new); got != tt.want {
			t.Errorf("diff %q -> %q:\n%s\nwant:\n%s", tt.old, tt.new, got, tt.want)
		}
	}
}

// Applying the diff to the old text gives the new text, for random edits.
func TestDiffApplies(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := range 300 {
		old := randomLines(r, r.IntN(40))
		new := mutate(r, old)
		d := unifiedDiff("f", strings.Join(old, ""), strings.Join(new, ""))
		if got := applyDiff(t, old, d); got != strings.Join(new, "") {
			t.Fatalf("case %d: applying\n%s\nto %q gives %q, want %q", i, d, old, got, strings.Join(new, ""))
		}
	}
}

func randomLines(r *rand.Rand, n int) []string {
	var out []string
	for range n {
		out = append(out, fmt.Sprintf("l%d\n", r.IntN(6)))
	}
	return out
}

func mutate(r *rand.Rand, lines []string) []string {
	out := append([]string(nil), lines...)
	for range r.IntN(6) {
		switch i := r.IntN(len(out) + 1); r.IntN(3) {
		case 0:
			out = append(out[:i], append(randomLines(r, 1+r.IntN(3)), out[i:]...)...)
		case 1:
			if i < len(out) {
				out = append(out[:i], out[i+1:]...)
			}
		default:
			if i < len(out) {
				out[i] = "changed\n"
			}
		}
	}
	return out
}

// applyDiff applies a unified diff without trailing-newline markers.
func applyDiff(t *testing.T, old []string, d string) string {
	t.Helper()
	if d == "" {
		return strings.Join(old, "")
	}
	lines := strings.SplitAfter(d, "\n")
	var out []string
	pos := 0
	for i := 2; i < len(lines); i++ {
		l := lines[i]
		switch {
		case l == "":
		case strings.HasPrefix(l, "@@"):
			var aStart int
			spec := strings.Fields(l)[1][1:]
			aStart, _ = strconv.Atoi(strings.Split(spec, ",")[0])
			if !strings.Contains(spec, ",") || !strings.HasSuffix(spec, ",0") {
				aStart--
			}
			out = append(out, old[pos:aStart]...)
			pos = aStart
		case l[0] == ' ':
			out = append(out, l[1:])
			pos++
		case l[0] == '-':
			pos++
		case l[0] == '+':
			out = append(out, l[1:])
		default:
			t.Fatalf("bad diff line %q", l)
		}
	}
	return strings.Join(append(out, old[pos:]...), "")
}

func TestGlobAndGrep(t *testing.T) {
	for _, rg := range []string{"", "-"} {
		name := "ripgrep"
		if rg == "-" {
			name = "built-in"
		} else if lookProgram("", "rg") == "" {
			t.Log("ripgrep isn't installed; testing only the built-in search")
			continue
		}
		t.Run(name, func(t *testing.T) {
			s, root := session(t, Options{Ripgrep: rg})
			ws := filepath.Join(root, "workspace")
			write(t, filepath.Join(ws, "main.go"), "package main\n\nfunc main() {}\n")
			write(t, filepath.Join(ws, "pkg", "util.go"), "package pkg\n\n// Helper helps.\nfunc Helper() {}\n")
			write(t, filepath.Join(ws, "pkg", "deep", "x.txt"), "TODO: helper\n")
			write(t, filepath.Join(ws, ".git", "config"), "func ignored\n")

			globs := map[string]string{
				"*.go":          "main.go\npkg/util.go\n",
				"**/*.go":       "main.go\npkg/util.go\n",
				"pkg/*.go":      "pkg/util.go\n",
				"pkg/**/*.txt":  "pkg/deep/x.txt\n",
				"*.rs":          "no files match",
				"pkg/deep/*.go": "no files match",
			}
			for pattern, want := range globs {
				if r := call(t, s, Glob, map[string]any{"pattern": pattern}); r.Output != want {
					t.Errorf("glob %s = %q, want %q", pattern, r.Output, want)
				}
			}

			greps := []struct {
				input map[string]any
				want  []string
				not   []string
			}{
				{map[string]any{"pattern": "^func "}, []string{"main.go:3:func main() {}", "pkg/util.go:4:func Helper() {}"}, []string{".git"}},
				{map[string]any{"pattern": "helper", "ignore_case": true, "glob": "*.go"}, []string{"pkg/util.go:3:", "pkg/util.go:4:"}, []string{"x.txt"}},
				{map[string]any{"pattern": "helper", "path": "pkg/deep"}, []string{"pkg/deep/x.txt:1:TODO: helper"}, nil},
				{map[string]any{"pattern": "nothing here"}, []string{"no matches"}, nil},
			}
			for _, g := range greps {
				r := call(t, s, Grep, g.input)
				for _, w := range g.want {
					if !strings.Contains(r.Output, w) {
						t.Errorf("grep %v lacks %q:\n%s", g.input, w, r.Output)
					}
				}
				for _, n := range g.not {
					if strings.Contains(r.Output, n) {
						t.Errorf("grep %v has %q:\n%s", g.input, n, r.Output)
					}
				}
			}
		})
	}
}

func TestShell(t *testing.T) {
	t.Setenv("ACTLET_TOKEN", "secret-token")
	s, root := session(t, Options{})
	if !s.Has(Shell) {
		t.Skip("no shell")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the cases use POSIX shell syntax")
	}
	tests := []struct {
		name    string
		input   map[string]any
		want    []string
		isError bool
	}{
		{"ok", map[string]any{"command": "echo out; echo err >&2"}, []string{"exit code 0\n", "stdout:\nout\n", "stderr:\nerr\n"}, false},
		{"exit code", map[string]any{"command": "exit 3"}, []string{"exit code 3"}, true},
		{"workspace", map[string]any{"command": "pwd"}, []string{"workspace"}, false},
		{"no token", map[string]any{"command": "echo token=${ACTLET_TOKEN:-none}"}, []string{"token=none"}, false},
		{"timeout", map[string]any{"command": "sleep 30", "timeout_seconds": 1}, []string{"timed out after 1s"}, true},
		{"big output", map[string]any{"command": "head -c 100000 /dev/zero | tr '\\0' x; echo; echo END"}, []string{"bytes omitted", "END"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := call(t, s, Shell, tt.input)
			if r.IsError != tt.isError {
				t.Errorf("IsError = %v", r.IsError)
			}
			for _, w := range tt.want {
				if !strings.Contains(r.Output, w) {
					t.Errorf("output lacks %q:\n%s", w, r.Output)
				}
			}
			if len(r.Output) > 70<<10 {
				t.Errorf("output of %d bytes", len(r.Output))
			}
		})
	}
	_ = root
}

// Interrupting a command ends its whole process group, background
// children included.
func TestShellInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no process groups")
	}
	s, root := session(t, Options{})
	marker := filepath.Join(root, "workspace", "child.pid")
	input, _ := json.Marshal(map[string]any{"command": "sleep 60 & echo $! > child.pid; wait"})
	c, err := s.Prepare(Shell, input)
	if err != nil {
		t.Fatal(err)
	}
	soft, cancel := context.WithCancel(t.Context())
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	start := time.Now()
	r := c.Run(RunContext{Soft: soft, Hard: t.Context()})
	if !r.IsError || !strings.Contains(r.Output, "interrupted") || time.Since(start) > 10*time.Second {
		t.Fatalf("after %v: %+v", time.Since(start), r)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(read(t, marker)))
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("background child %d still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPython(t *testing.T) {
	s, root := session(t, Options{})
	if !s.Has(Python) {
		t.Skip("no Python interpreter")
	}
	write(t, filepath.Join(root, "workspace", "helper.py"), "def divide(a, b):\n    return a / b\n")
	write(t, filepath.Join(root, "workspace", "main.py"), "import sys\nfrom helper import divide\nprint(sys.argv[1:])\ndivide(1, 0)\n")
	tests := []struct {
		name    string
		input   map[string]any
		want    []string
		isError bool
	}{
		{"ok", map[string]any{"code": "import sys\nprint('hi', file=sys.stderr)\nprint(6*7)"}, []string{"exit code 0", "stdout:\n42\n", "stderr:\nhi\n"}, false},
		{"exception", map[string]any{"path": "main.py", "args": []string{"a", "b"}},
			[]string{"exit code 1", "['a', 'b']", "exception: ZeroDivisionError: division by zero", "main.py:4 in <module>\n    divide(1, 0)", "helper.py:2 in divide\n    return a / b"}, true},
		{"syntax error", map[string]any{"code": "x = (1,\nprint(x"}, []string{"exception: SyntaxError", "<code>:"}, true},
		{"missing module", map[string]any{"code": "import surely_not_installed_xyz"}, []string{"ModuleNotFoundError", `hint: the module "surely_not_installed_xyz" isn't installed`}, true},
		{"exit", map[string]any{"code": "raise SystemExit(4)"}, []string{"exit code 4"}, true},
		{"code and path", map[string]any{"code": "1", "path": "main.py"}, []string{"either code or path"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := call(t, s, Python, tt.input)
			if r.IsError != tt.isError {
				t.Errorf("IsError = %v", r.IsError)
			}
			for _, w := range tt.want {
				if !strings.Contains(r.Output, w) {
					t.Errorf("output lacks %q:\n%s", w, r.Output)
				}
			}
			if strings.Contains(r.Output, "runpy") || strings.Contains(r.Output, "_ACTLET") {
				t.Errorf("output shows the bootstrap:\n%s", r.Output)
			}
		})
	}
}

func TestGrantsAndUnknownTools(t *testing.T) {
	s, _ := session(t, Options{Grants: []Grant{{Name: ReadFile}, {Name: Shell, Settings: map[string]string{"timeout": "5s"}}}, Python: "-"})
	var names []string
	for _, sp := range s.Specs() {
		names = append(names, sp.Name)
		var schema map[string]any
		if err := json.Unmarshal(sp.Schema, &schema); err != nil {
			t.Errorf("%s schema: %v", sp.Name, err)
		}
	}
	if got := strings.Join(names, ","); got != "read_file,shell" {
		t.Errorf("tools %s", got)
	}
	if _, err := s.Prepare(WriteFile, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("ungranted tool: %v", err)
	}
	if _, err := New(Options{Workspace: t.TempDir(), Grants: []Grant{{Name: Shell, Settings: map[string]string{"colour": "red"}}}}); err == nil {
		t.Error("accepted an unknown shell setting")
	}
	all, _ := session(t, Options{Python: "-"})
	if all.Has(Python) || !all.Has(Grep) {
		t.Error("python offered without an interpreter, or grep missing")
	}
}
