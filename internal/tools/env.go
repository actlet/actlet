package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Options configure a tool set.
type Options struct {
	// Workspace is the working directory; relative paths resolve against
	// it.
	Workspace string
	// ReadOnly are directories and files no tool may write, such as the
	// session's inputs/ and Actlet's own state.
	ReadOnly []string
	// TempDir holds temporary files, such as Python scripts given as
	// source.
	TempDir string

	// Grants lists the tools the profile allows, with their settings. Nil
	// allows every tool with default settings.
	Grants []Grant

	// Shell, Python and Ripgrep override the programs found on PATH. "-"
	// means none.
	Shell, Python, Ripgrep string
}

// Grant allows a tool, with tool-specific settings.
type Grant struct {
	Name     string
	Settings map[string]string
}

// Env is where tools run.
type Env struct {
	workspace string
	readOnly  []string
	tempDir   string
	shell     []string // the shell and its "run this" flag
	python    string
	ripgrep   string
	shellCfg  shellConfig
}

// Names of the local tools.
const (
	ReadFile  = "read_file"
	WriteFile = "write_file"
	EditFile  = "edit_file"
	Glob      = "glob"
	Grep      = "grep"
	Shell     = "shell"
	Python    = "python"
)

// New returns the tools available with the options and in this
// environment. The python tool is offered only if an interpreter exists.
func New(o Options) (*Set, error) {
	ws, err := filepath.Abs(o.Workspace)
	if err != nil {
		return nil, err
	}
	e := &Env{workspace: ws, tempDir: o.TempDir, shellCfg: defaultShellConfig()}
	for _, p := range o.ReadOnly {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		real, err := realPath(abs)
		if err != nil {
			return nil, err
		}
		e.readOnly = append(e.readOnly, real)
	}
	if e.tempDir == "" {
		e.tempDir = os.TempDir()
	}

	s := &Set{env: e, tools: map[string]*tool{}}
	granted := func(name string) (map[string]string, bool) {
		if o.Grants == nil {
			return nil, true
		}
		for _, g := range o.Grants {
			if g.Name == name {
				return g.Settings, true
			}
		}
		return nil, false
	}

	e.ripgrep = lookProgram(o.Ripgrep, "rg")
	for _, t := range []*tool{e.readFileTool(), e.writeFileTool(), e.editFileTool(), e.globTool(), e.grepTool()} {
		if _, ok := granted(t.spec.Name); ok {
			s.tools[t.spec.Name] = t
		}
	}
	if settings, ok := granted(Shell); ok {
		if err := e.shellCfg.apply(settings); err != nil {
			return nil, fmt.Errorf("shell tool settings: %w", err)
		}
		e.shell, err = findShell(o.Shell, settings["shell"])
		if err != nil {
			s.notes = append(s.notes, err.Error())
		} else {
			if filepath.Base(e.shell[0]) != "bash" && filepath.Base(e.shell[0]) != "bash.exe" {
				s.notes = append(s.notes, fmt.Sprintf("bash isn't installed; the shell tool uses %s", e.shell[0]))
			}
			s.tools[Shell] = e.shellTool()
		}
	}
	if settings, ok := granted(Python); ok {
		e.python = lookProgram(firstNonEmpty(o.Python, settings["interpreter"]), "python3", "python")
		if e.python != "" {
			s.tools[Python] = e.pythonTool()
		}
	}
	return s, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// lookProgram finds the override, or the first of the names on PATH. It
// returns "" if there is none.
func lookProgram(override string, names ...string) string {
	if override == "-" {
		return ""
	}
	if override != "" {
		names = []string{override}
	}
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

func findShell(override, setting string) ([]string, error) {
	if override == "-" {
		return nil, errors.New("no shell: the shell tool is unavailable")
	}
	if p := lookProgram(firstNonEmpty(override, setting), "bash", "sh"); p != "" {
		return []string{p, "-c"}, nil
	}
	if runtime.GOOS == "windows" {
		if p := lookProgram("", "cmd"); p != "" {
			return []string{p, "/C"}, nil
		}
	}
	return nil, errors.New("neither bash nor sh is installed: the shell tool is unavailable")
}

// resolve returns the absolute path for p, relative to the workspace.
func (e *Env) resolve(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.workspace, p)
	}
	return filepath.Clean(p), nil
}

// checkWritable refuses paths in a read-only area, also when reached
// through a symbolic link.
func (e *Env) checkWritable(path string) error {
	real, err := realPath(path)
	if err != nil {
		return err
	}
	for _, ro := range e.readOnly {
		if within(real, ro) {
			return fmt.Errorf("%s is read-only: inputs/ holds the task's input files, and .actlet/ is Actlet's own; write results to output/ and work in workspace/", path)
		}
	}
	return nil
}

// realPath resolves symbolic links in path, including in the parts that
// don't exist yet, such as a dangling link.
func realPath(path string) (string, error) {
	return realPathDepth(path, 0)
}

func realPathDepth(path string, depth int) (string, error) {
	if depth > 40 {
		return "", fmt.Errorf("%s: too many levels of symbolic links", path)
	}
	rest := ""
	cur := path
	for {
		r, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(r, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if fi, lerr := os.Lstat(cur); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(cur)
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(cur), target)
			}
			r, err := realPathDepth(target, depth+1)
			if err != nil {
				return "", err
			}
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return path, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		// Usually case-insensitive filesystems.
		path, dir = strings.ToLower(path), strings.ToLower(dir)
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// display shows a path relative to the workspace when it is inside it.
func (e *Env) display(path string) string {
	if rel, err := filepath.Rel(e.workspace, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	if rel, err := filepath.Rel(filepath.Dir(e.workspace), path); err == nil && !strings.HasPrefix(rel, "..") {
		return "../" + filepath.ToSlash(rel)
	}
	return path
}

// toolEnv is the environment of programs the tools run: the worker's own,
// without Actlet's settings, which may hold the gateway token.
func toolEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(strings.ToUpper(kv), "ACTLET_")
	})
}

// shellConfig are the shell tool's settings.
type shellConfig struct {
	timeout    time.Duration // default per command
	maxTimeout time.Duration
	outputCap  int // per stream
	termGrace  time.Duration
}

func defaultShellConfig() shellConfig {
	return shellConfig{timeout: 2 * time.Minute, maxTimeout: 10 * time.Minute, outputCap: 32 << 10, termGrace: 5 * time.Second}
}

func (c *shellConfig) apply(settings map[string]string) error {
	for k, v := range settings {
		var err error
		switch k {
		case "shell":
		case "timeout":
			c.timeout, err = time.ParseDuration(v)
		case "max_timeout":
			c.maxTimeout, err = time.ParseDuration(v)
		case "output_cap":
			c.outputCap, err = strconv.Atoi(v)
		default:
			return fmt.Errorf("unknown setting %q", k)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	if c.timeout <= 0 || c.maxTimeout < c.timeout || c.outputCap < 1024 {
		return fmt.Errorf("timeout must be positive and at most max_timeout, and output_cap at least 1024")
	}
	return nil
}
