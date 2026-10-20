package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/actlet/actlet/internal/sessionfs"
)

// Limits of the file tools.
const (
	maxReadFile     = 256 << 10 // bytes returned by one read
	defaultReadLine = 2000      // lines returned by one read
	maxLineLen      = 2000
	maxEditFile     = 8 << 20 // files written or edited
)

func (e *Env) readFileTool() *tool {
	return &tool{
		spec: Spec{
			Name: ReadFile,
			Description: "Read a text file. Lines come numbered; read a large file in parts with offset and limit. " +
				"Relative paths are relative to workspace/.",
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"file to read"},` +
				`"offset":{"type":"integer","minimum":1,"description":"first line to read, from 1"},` +
				`"limit":{"type":"integer","minimum":1,"description":"number of lines to read (default 2000)"}},` +
				`"required":["path"],"additionalProperties":false}`),
			Policy: PolicyRetry,
		},
		prepare: func(in json.RawMessage) (*Call, error) {
			var args struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := decode(in, &args); err != nil {
				return nil, err
			}
			path, err := e.resolve(args.Path)
			if err != nil {
				return nil, err
			}
			if args.Offset < 0 || args.Limit < 0 {
				return nil, errors.New("offset and limit must be positive")
			}
			return &Call{run: func(RunContext) Result { return e.readFile(path, max(args.Offset, 1), args.Limit) }}, nil
		},
	}
}

func (e *Env) readFile(path string, offset, limit int) Result {
	if limit == 0 {
		limit = defaultReadLine
	}
	f, err := os.Open(path)
	if err != nil {
		return errorResult("%v", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return errorResult("%v", err)
	}
	if fi.IsDir() {
		return errorResult("%s is a directory; use glob to list files", e.display(path))
	}
	head := make([]byte, 8192)
	n, _ := io.ReadFull(f, head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return errorResult("%s is a binary file of %d bytes; inspect it with the shell", e.display(path), fi.Size())
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return errorResult("%v", err)
	}

	var out strings.Builder
	data, err := io.ReadAll(io.LimitReader(f, 64<<20))
	if err != nil {
		return errorResult("%v", err)
	}
	lines := splitLines(string(data))
	if len(lines) == 0 {
		return Result{Output: fmt.Sprintf("%s is empty", e.display(path))}
	}
	if offset > len(lines) {
		return errorResult("%s has %d lines; offset %d is past the end", e.display(path), len(lines), offset)
	}
	last := offset - 1
	for i := offset - 1; i < len(lines) && i < offset-1+limit; i++ {
		line := strings.TrimRight(lines[i], "\r\n")
		if len(line) > maxLineLen {
			line = validPrefix(line[:maxLineLen]) + " [line cut]"
		}
		entry := fmt.Sprintf("%6d\t%s\n", i+1, line)
		if out.Len()+len(entry) > maxReadFile {
			break
		}
		out.WriteString(entry)
		last = i + 1
	}
	if last < len(lines) {
		fmt.Fprintf(&out, "[lines %d-%d of %d; read on with offset %d]\n", offset, last, len(lines), last+1)
	}
	return Result{Output: out.String()}
}

// fileCheck is the recovery data of a write or edit: the file's content
// hash before and after the change. An empty hash means no file.
type fileCheck struct {
	Path   string `json:"path"`
	Before string `json:"before"`
	After  string `json:"after"`
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// currentHash returns the hash of the file, or "" if it doesn't exist.
func currentHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return hashOf(data), nil
}

// reconcileFile decides about an interrupted write or edit from the file's
// content now.
func reconcileFile(e *Env, check []byte) Recovery {
	var c fileCheck
	if err := json.Unmarshal(check, &c); err != nil {
		return Recovery{Result: UnknownOutcome("file")}
	}
	now, err := currentHash(c.Path)
	switch {
	case err != nil:
		return Recovery{Result: UnknownOutcome("file")}
	case now == c.After:
		return Recovery{Result: Result{Output: fmt.Sprintf("%s was changed as requested (completed before the worker restarted)", e.display(c.Path))}}
	case now == c.Before:
		return Recovery{Rerun: true}
	}
	return Recovery{Result: Result{
		Output:  fmt.Sprintf("outcome_unknown: %s changed in an unexpected way while the worker was down; read it before changing it again", e.display(c.Path)),
		IsError: true, Unknown: true,
	}}
}

// planChange plans replacing path's content: it checks the path may be
// written and returns the recovery data and the call.
func (e *Env) planChange(path string, old []byte, existed bool, new []byte) (*Call, error) {
	if err := e.checkWritable(path); err != nil {
		return nil, err
	}
	if len(new) > maxEditFile {
		return nil, fmt.Errorf("the new content is %d bytes; the limit is %d", len(new), maxEditFile)
	}
	c := fileCheck{Path: path, After: hashOf(new)}
	if existed {
		c.Before = hashOf(old)
	}
	check, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return &Call{Check: check, run: func(RunContext) Result {
		// The file must still be what was planned against.
		if now, err := currentHash(path); err != nil {
			return errorResult("%v", err)
		} else if now != c.Before {
			return errorResult("%s changed since this call was prepared; read it again", e.display(path))
		}
		perm := fs.FileMode(0o644)
		if fi, err := os.Stat(path); err == nil {
			perm = fi.Mode().Perm()
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return errorResult("%v", err)
		}
		if err := sessionfs.WriteFileAtomic(path, new, perm); err != nil {
			return errorResult("%v", err)
		}
		shown := e.display(path)
		ch := Change{Path: shown, Diff: unifiedDiff(shown, string(old), string(new)), Created: !existed}
		verb := "updated"
		if !existed {
			verb = "created"
		}
		return Result{Output: fmt.Sprintf("%s %s (%d bytes)", verb, shown, len(new)), Changes: []Change{ch}}
	}}, nil
}

// readForChange reads a file about to be changed; a missing file is empty.
func readForChange(path string) (data []byte, existed bool, err error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if fi.IsDir() {
		return nil, false, fmt.Errorf("%s is a directory", path)
	}
	if fi.Size() > maxEditFile {
		return nil, false, fmt.Errorf("%s is %d bytes; the file tools change files up to %d bytes, use the shell for larger ones", path, fi.Size(), maxEditFile)
	}
	data, err = os.ReadFile(path)
	return data, err == nil, err
}

func (e *Env) writeFileTool() *tool {
	return &tool{
		spec: Spec{
			Name: WriteFile,
			Description: "Create or overwrite a file with the given content. Missing directories are created. " +
				"inputs/ is read-only; put deliverables in output/.",
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"file to write"},` +
				`"content":{"type":"string","description":"the whole new content"}},` +
				`"required":["path","content"],"additionalProperties":false}`),
			Policy: PolicyReconcile,
		},
		prepare: func(in json.RawMessage) (*Call, error) {
			var args struct {
				Path    string  `json:"path"`
				Content *string `json:"content"`
			}
			if err := decode(in, &args); err != nil {
				return nil, err
			}
			if args.Content == nil {
				return nil, errors.New("content is required")
			}
			path, err := e.resolve(args.Path)
			if err != nil {
				return nil, err
			}
			old, existed, err := readForChange(path)
			if err != nil {
				return nil, err
			}
			return e.planChange(path, old, existed, []byte(*args.Content))
		},
		reconcile: func(_ json.RawMessage, check []byte) Recovery { return reconcileFile(e, check) },
	}
}

func (e *Env) editFileTool() *tool {
	return &tool{
		spec: Spec{
			Name: EditFile,
			Description: "Replace text in a file. old_string must match exactly, including whitespace, and only once " +
				"unless replace_all is set; include surrounding lines to make it unique. Read the file first.",
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"file to edit"},` +
				`"old_string":{"type":"string","description":"text to replace"},` +
				`"new_string":{"type":"string","description":"replacement text"},` +
				`"replace_all":{"type":"boolean","description":"replace every occurrence"}},` +
				`"required":["path","old_string","new_string"],"additionalProperties":false}`),
			Policy: PolicyReconcile,
		},
		prepare: func(in json.RawMessage) (*Call, error) {
			var args struct {
				Path       string `json:"path"`
				Old        string `json:"old_string"`
				New        string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := decode(in, &args); err != nil {
				return nil, err
			}
			path, err := e.resolve(args.Path)
			if err != nil {
				return nil, err
			}
			if args.Old == "" {
				return nil, errors.New("old_string is empty; use write_file to create a file")
			}
			if args.Old == args.New {
				return nil, errors.New("old_string and new_string are the same")
			}
			old, existed, err := readForChange(path)
			if err != nil {
				return nil, err
			}
			if !existed {
				return nil, fmt.Errorf("%s doesn't exist; use write_file to create it", e.display(path))
			}
			if !utf8.Valid(old) {
				return nil, fmt.Errorf("%s isn't UTF-8 text; use the shell to change it", e.display(path))
			}
			n := strings.Count(string(old), args.Old)
			switch {
			case n == 0:
				return nil, fmt.Errorf("old_string doesn't occur in %s; read the file and copy the text exactly", e.display(path))
			case n > 1 && !args.ReplaceAll:
				return nil, fmt.Errorf("old_string occurs %d times in %s; add surrounding lines to make it unique, or set replace_all", n, e.display(path))
			}
			updated := strings.ReplaceAll(string(old), args.Old, args.New)
			call, err := e.planChange(path, old, true, []byte(updated))
			if err != nil {
				return nil, err
			}
			run := call.run
			call.run = func(ctx RunContext) Result {
				r := run(ctx)
				if !r.IsError {
					r.Output = fmt.Sprintf("replaced %d occurrence(s) in %s\n%s", n, e.display(path), r.Changes[0].Diff)
				}
				return r
			}
			return call, nil
		},
		reconcile: func(_ json.RawMessage, check []byte) Recovery { return reconcileFile(e, check) },
	}
}
