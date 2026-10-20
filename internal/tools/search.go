package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/actlet/actlet/internal/supervise"
)

// Limits of the search tools.
const (
	maxGlobResults = 1000
	maxGrepMatches = 500
	maxGrepOutput  = 64 << 10
	maxGrepFile    = 16 << 20 // files the built-in grep searches
	searchTimeout  = time.Minute
)

func (e *Env) globTool() *tool {
	desc := "Find files by name pattern, such as \"**/*.go\" or \"src/*.json\". `**` matches any number of directories, " +
		"and a pattern without a slash matches file names at any depth. Results are sorted paths."
	return &tool{
		spec: Spec{
			Name:        Glob,
			Description: desc,
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"pattern":{"type":"string","description":"glob pattern"},` +
				`"path":{"type":"string","description":"directory to search (default workspace/)"}},` +
				`"required":["pattern"],"additionalProperties":false}`),
			Policy: PolicyRetry,
		},
		prepare: func(in json.RawMessage) (*Call, error) {
			var args struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if err := decode(in, &args); err != nil {
				return nil, err
			}
			if args.Pattern == "" {
				return nil, errors.New("pattern is required")
			}
			if _, err := path.Match(strings.ReplaceAll(args.Pattern, "**", "*"), ""); err != nil {
				return nil, fmt.Errorf("bad pattern: %w", err)
			}
			dir, err := e.resolve(firstNonEmpty(args.Path, "."))
			if err != nil {
				return nil, err
			}
			return &Call{run: func(ctx RunContext) Result { return e.glob(ctx.Hard, dir, args.Pattern) }}, nil
		},
	}
}

func (e *Env) glob(ctx context.Context, dir, pattern string) Result {
	var paths []string
	var err error
	if e.ripgrep != "" {
		paths, err = e.rgFiles(ctx, dir, pattern)
	} else {
		paths, err = walkGlob(ctx, dir, pattern)
	}
	if err != nil {
		return errorResult("%v", err)
	}
	if len(paths) == 0 {
		return Result{Output: "no files match"}
	}
	sort.Strings(paths)
	more := ""
	if len(paths) > maxGlobResults {
		more = fmt.Sprintf("[%d more; narrow the pattern]\n", len(paths)-maxGlobResults)
		paths = paths[:maxGlobResults]
	}
	for i, p := range paths {
		paths[i] = e.display(filepath.Join(dir, filepath.FromSlash(p)))
	}
	return Result{Output: strings.Join(paths, "\n") + "\n" + more}
}

func (e *Env) rgFiles(ctx context.Context, dir, pattern string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.ripgrep, "--files", "--hidden", "--glob", "!.git", "--glob", pattern)
	cmd.Dir = dir
	cmd.Env = toolEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := supervise.Start(cmd)
	if err == nil {
		err = cmd.Wait()
		supervise.Done(cmd)
	}
	out := stdout.Bytes()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		ee.Stderr = stderr.Bytes()
	}
	if err != nil && !isExit(err, 1) { // 1: nothing found
		return nil, rgError(err)
	}
	var paths []string
	for l := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			paths = append(paths, filepath.ToSlash(strings.TrimPrefix(l, "./")))
		}
	}
	return paths, nil
}

func isExit(err error, code int) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == code
}

func rgError(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("ripgrep: %s", strings.TrimSpace(string(ee.Stderr)))
	}
	return fmt.Errorf("ripgrep: %w", err)
}

// walkGlob is the built-in glob, for when ripgrep isn't installed.
func walkGlob(ctx context.Context, dir, pattern string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil // unreadable: skip
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		if matchGlob(pattern, filepath.ToSlash(rel)) {
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	return paths, err
}

// matchGlob matches a slash-separated relative path against a pattern in
// which `**` matches any number of directories. A pattern without a slash
// matches the base name.
func matchGlob(pattern, name string) bool {
	pattern = strings.TrimPrefix(pattern, "./")
	if !strings.Contains(pattern, "/") {
		ok, _ := path.Match(pattern, path.Base(name))
		return ok
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

func (e *Env) grepTool() *tool {
	return &tool{
		spec: Spec{
			Name: Grep,
			Description: "Search file contents with a regular expression. Prints matching lines as path:line:text. " +
				"Filter files with glob, such as \"*.go\".",
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"pattern":{"type":"string","description":"regular expression"},` +
				`"path":{"type":"string","description":"file or directory to search (default workspace/)"},` +
				`"glob":{"type":"string","description":"only search files matching this glob"},` +
				`"ignore_case":{"type":"boolean"}},` +
				`"required":["pattern"],"additionalProperties":false}`),
			Policy: PolicyRetry,
		},
		prepare: func(in json.RawMessage) (*Call, error) {
			var args struct {
				Pattern    string `json:"pattern"`
				Path       string `json:"path"`
				Glob       string `json:"glob"`
				IgnoreCase bool   `json:"ignore_case"`
			}
			if err := decode(in, &args); err != nil {
				return nil, err
			}
			if args.Pattern == "" {
				return nil, errors.New("pattern is required")
			}
			expr := args.Pattern
			if args.IgnoreCase {
				expr = "(?i)" + expr
			}
			re, err := regexp.Compile(expr)
			if err != nil && e.ripgrep == "" {
				return nil, fmt.Errorf("bad pattern: %w", err)
			}
			target, err := e.resolve(firstNonEmpty(args.Path, "."))
			if err != nil {
				return nil, err
			}
			return &Call{run: func(ctx RunContext) Result {
				if e.ripgrep != "" {
					return e.rgGrep(ctx.Hard, target, args.Pattern, args.Glob, args.IgnoreCase)
				}
				return e.walkGrep(ctx.Hard, target, re, args.Glob)
			}}, nil
		},
	}
}

func (e *Env) rgGrep(ctx context.Context, target, pattern, glob string, ignoreCase bool) Result {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	args := []string{"--line-number", "--no-heading", "--with-filename", "--null", "--color", "never", "--hidden", "--glob", "!.git", "--max-columns", "500"}
	if ignoreCase {
		args = append(args, "--ignore-case")
	}
	if glob != "" {
		args = append(args, "--glob", glob)
	}
	args = append(args, "--regexp", pattern, "--", target)
	cmd := exec.CommandContext(ctx, e.ripgrep, args...)
	cmd.Env = toolEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return errorResult("%v", err)
	}
	if err := supervise.Start(cmd); err != nil {
		return errorResult("ripgrep: %v", err)
	}
	defer supervise.Done(cmd)
	g := newGrepOutput()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() && !g.full() {
		line := sc.Text()
		// path NUL line:text; show the path relative to the workspace.
		if p, rest, ok := strings.Cut(line, "\x00"); ok {
			line = e.display(p) + ":" + rest
		}
		g.add(line)
	}
	if g.full() {
		cancel()
	}
	_, _ = io.Copy(io.Discard, out)
	err = cmd.Wait()
	if err != nil && !isExit(err, 1) && !g.full() {
		return errorResult("ripgrep: %s", strings.TrimSpace(firstNonEmpty(stderr.String(), err.Error())))
	}
	return g.result()
}

// walkGrep is the built-in grep, for when ripgrep isn't installed.
func (e *Env) walkGrep(ctx context.Context, target string, re *regexp.Regexp, glob string) Result {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	g := newGrepOutput()
	err := filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == target {
				return err
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if g.full() {
			return filepath.SkipAll
		}
		if glob != "" && p != target {
			rel, _ := filepath.Rel(target, p)
			if !matchGlob(glob, filepath.ToSlash(rel)) {
				return nil
			}
		}
		grepFile(p, e.display(p), re, g)
		return nil
	})
	if err != nil {
		return errorResult("%v", err)
	}
	return g.result()
}

func grepFile(p, shown string, re *regexp.Regexp, g *grepOutput) {
	f, err := os.Open(p)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() || fi.Size() > maxGrepFile {
		return
	}
	data, err := io.ReadAll(f)
	if err != nil || bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 {
		return // binary
	}
	for i, line := range strings.Split(string(data), "\n") {
		if g.full() {
			return
		}
		if re.MatchString(line) {
			if len(line) > 500 {
				line = validPrefix(line[:500]) + " [line cut]"
			}
			g.add(fmt.Sprintf("%s:%d:%s", shown, i+1, strings.TrimRight(line, "\r")))
		}
	}
}

type grepOutput struct {
	sb strings.Builder
	n  int
}

func newGrepOutput() *grepOutput { return &grepOutput{} }

func (g *grepOutput) full() bool { return g.n >= maxGrepMatches || g.sb.Len() >= maxGrepOutput }

func (g *grepOutput) add(line string) {
	if g.full() {
		return
	}
	g.sb.WriteString(line)
	g.sb.WriteByte('\n')
	g.n++
}

func (g *grepOutput) result() Result {
	if g.n == 0 {
		return Result{Output: "no matches"}
	}
	out := g.sb.String()
	if g.full() {
		out += fmt.Sprintf("[stopped after %d matches; narrow the search]\n", g.n)
	}
	return Result{Output: out}
}
