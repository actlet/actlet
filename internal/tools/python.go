package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// pythonBootstrap runs the script and, if it raises, writes the exception
// and its traceback as JSON to the file named by the report variable.
const pythonBootstrap = `
import json, os, runpy, sys, traceback
_report = os.environ.pop("_ACTLET_PYTHON_REPORT", None)
_path = sys.argv[1]
sys.argv = sys.argv[1:]
sys.path.insert(0, os.path.dirname(os.path.abspath(_path)))
def _write(e):
    frames = []
    for f in traceback.extract_tb(e.__traceback__):
        if f.filename == "<string>" or os.path.basename(f.filename) == "runpy.py" or "<frozen" in f.filename:
            continue
        frames.append({"file": f.filename, "line": f.lineno, "function": f.name, "code": f.line or ""})
    d = {"type": type(e).__name__, "message": str(e), "frames": frames}
    if isinstance(e, SyntaxError):
        d.update(message=e.msg or "", file=e.filename or "", line=e.lineno or 0, offset=e.offset or 0, text=(e.text or "").rstrip("\n"))
    if isinstance(e, ImportError) and e.name:
        d["module"] = e.name
    with open(_report, "w") as fh:
        json.dump(d, fh)
try:
    runpy.run_path(_path, run_name="__main__")
except SystemExit:
    raise
except BaseException as e:
    if _report:
        _write(e)
    else:
        traceback.print_exc()
    sys.exit(1)
`

// pythonError is the exception report the bootstrap writes.
type pythonError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Frames  []struct {
		File     string `json:"file"`
		Line     int    `json:"line"`
		Function string `json:"function"`
		Code     string `json:"code"`
	} `json:"frames"`
	// SyntaxError
	File   string `json:"file"`
	Line   int    `json:"line"`
	Offset int    `json:"offset"`
	Text   string `json:"text"`
	// ImportError
	Module string `json:"module"`
}

func (e *Env) pythonTool() *tool {
	return &tool{
		spec: Spec{
			Name: Python,
			Description: fmt.Sprintf("Run a Python script with %s, given as code or as a file path, in workspace/. "+
				"Returns the exit code, stdout and stderr, and for an uncaught exception its type, message and "+
				"traceback with file and line. Prefer this to running python through the shell. Times out like the "+
				"shell tool; a script is never run again automatically.", e.python),
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"code":{"type":"string","description":"the script's source"},` +
				`"path":{"type":"string","description":"a script file to run instead of code"},` +
				`"args":{"type":"array","items":{"type":"string"},"description":"command-line arguments"},` +
				`"timeout_seconds":{"type":"integer","minimum":1},` + writesSchema + `},` +
				`"additionalProperties":false}`),
			Policy: PolicyNever,
		},
		writes: e.programWrites,
		prepare: func(in json.RawMessage) (*Call, error) {
			var args struct {
				Code    *string  `json:"code"`
				Path    string   `json:"path"`
				Args    []string `json:"args"`
				Timeout int      `json:"timeout_seconds"`
				Writes  []string `json:"writes"`
			}
			if err := decode(in, &args); err != nil {
				return nil, err
			}
			if (args.Code == nil) == (args.Path == "") {
				return nil, errors.New("give either code or path")
			}
			timeout, err := e.timeout(args.Timeout)
			if err != nil {
				return nil, err
			}
			writes, err := e.writeSet(args.Writes)
			if err != nil {
				return nil, err
			}
			script := ""
			if args.Path != "" {
				if script, err = e.resolve(args.Path); err != nil {
					return nil, err
				}
				if _, err := os.Stat(script); err != nil {
					return nil, err
				}
			}
			return &Call{writes: writes, writesAll: writes == nil, run: func(ctx RunContext) Result {
				shown := e.display(script)
				if args.Code != nil {
					dir, err := os.MkdirTemp(e.tempDir, "python-")
					if err != nil {
						return errorResult("%v", err)
					}
					defer func() { _ = os.RemoveAll(dir) }()
					script, shown = filepath.Join(dir, "script.py"), "<code>"
					if err := os.WriteFile(script, []byte(*args.Code), 0o600); err != nil {
						return errorResult("%v", err)
					}
				}
				report, err := os.CreateTemp(e.tempDir, "python-report-")
				if err != nil {
					return errorResult("%v", err)
				}
				_ = report.Close()
				defer func() { _ = os.Remove(report.Name()) }()

				argv := append([]string{e.python, "-c", pythonBootstrap, script}, args.Args...)
				run := e.runProgram(ctx, argv, []string{"_ACTLET_PYTHON_REPORT=" + report.Name()}, timeout)
				res := run.result()
				if data, err := os.ReadFile(report.Name()); err == nil && len(data) > 0 {
					var pe pythonError
					if json.Unmarshal(data, &pe) == nil {
						res.Output += formatPythonError(&pe, script, shown)
					}
				}
				return res
			}}, nil
		},
	}
}

// formatPythonError describes an exception for the model.
func formatPythonError(pe *pythonError, script, shown string) string {
	name := func(file string) string {
		if file == script {
			return shown
		}
		return file
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "exception: %s: %s\n", pe.Type, pe.Message)
	if pe.Text != "" {
		fmt.Fprintf(&sb, "  at %s:%d\n    %s\n", name(pe.File), pe.Line, pe.Text)
		if pe.Offset > 0 {
			fmt.Fprintf(&sb, "    %s^\n", strings.Repeat(" ", pe.Offset-1))
		}
	}
	if len(pe.Frames) > 0 {
		sb.WriteString("traceback, most recent call last:\n")
		frames := pe.Frames
		if len(frames) > 20 {
			fmt.Fprintf(&sb, "  [%d outer frames omitted]\n", len(frames)-20)
			frames = frames[len(frames)-20:]
		}
		for _, f := range frames {
			fmt.Fprintf(&sb, "  %s:%d in %s\n", name(f.File), f.Line, f.Function)
			if f.Code != "" {
				fmt.Fprintf(&sb, "    %s\n", f.Code)
			}
		}
	}
	switch {
	case pe.Type == "ModuleNotFoundError" && pe.Module != "":
		fmt.Fprintf(&sb, "hint: the module %q isn't installed. Check the tools manifest for what is; install it only if that's allowed here.\n", pe.Module)
	case pe.Type == "FileNotFoundError":
		sb.WriteString("hint: scripts run in workspace/; the task's input files are in ../inputs/ and deliverables go to ../output/.\n")
	}
	return sb.String()
}
