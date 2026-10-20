// Package sessionfs manages the session folder: the one directory that holds
// everything a session needs to run, survive and hand over results.
//
//	task.json   the task definition and input
//	inputs/     input files, placed by the orchestrator; read-only to the agent
//	workspace/  the agent's working directory
//	output/     deliverables the orchestrator collects
//	.actlet/    the journal, the status marker and the activity spill
package sessionfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Folder is a session folder.
type Folder struct {
	Dir string
}

// Open returns the session folder at dir, creating it and its
// subdirectories if needed.
func Open(dir string) (Folder, error) {
	f := Folder{Dir: dir}
	for _, d := range []string{dir, f.Inputs(), f.Workspace(), f.Output(), f.State()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return Folder{}, fmt.Errorf("session folder: %w", err)
		}
	}
	return f, nil
}

// TaskFile returns the path of task.json.
func (f Folder) TaskFile() string { return filepath.Join(f.Dir, "task.json") }

// Inputs returns the input files directory.
func (f Folder) Inputs() string { return filepath.Join(f.Dir, "inputs") }

// Workspace returns the working directory.
func (f Folder) Workspace() string { return filepath.Join(f.Dir, "workspace") }

// Output returns the deliverables directory.
func (f Folder) Output() string { return filepath.Join(f.Dir, "output") }

// State returns the directory of Actlet's own files.
func (f Folder) State() string { return filepath.Join(f.Dir, ".actlet") }

// Journal returns the path of the session journal.
func (f Folder) Journal() string { return filepath.Join(f.State(), "session.actlet") }

// StatusFile returns the path of the status marker.
func (f Folder) StatusFile() string { return filepath.Join(f.State(), "status") }

// TaskSchema is the task.json schema version this build reads and writes.
const TaskSchema = "v0"

// Task is the content of task.json.
type Task struct {
	Schema string `json:"schema"`
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	// Definition says what to do; its schema depends on Kind.
	Definition json.RawMessage `json:"definition,omitempty"`
	// Input is the task input; its schema depends on Kind.
	Input json.RawMessage `json:"input,omitempty"`
}

// Validate checks the fields every task needs.
func (t *Task) Validate() error {
	switch {
	case t.Schema != TaskSchema:
		return fmt.Errorf("task schema %q, this build reads %q", t.Schema, TaskSchema)
	case t.ID == "":
		return errors.New("task has no id")
	case t.Kind == "":
		return errors.New("task has no kind")
	}
	for name, raw := range map[string]json.RawMessage{"definition": t.Definition, "input": t.Input} {
		if len(raw) > 0 && !json.Valid(raw) {
			return fmt.Errorf("task %s is not valid JSON", name)
		}
	}
	return nil
}

// ReadTask reads task.json. It returns an error wrapping os.ErrNotExist if
// there is none.
func (f Folder) ReadTask() (*Task, error) {
	data, err := os.ReadFile(f.TaskFile())
	if err != nil {
		return nil, err
	}
	var t Task
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("task.json: %w", err)
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("task.json: %w", err)
	}
	return &t, nil
}

// TaskJSON returns task.json as it is on disk.
func (f Folder) TaskJSON() ([]byte, error) { return os.ReadFile(f.TaskFile()) }

// WriteTask writes task.json atomically.
func (f Folder) WriteTask(t *Task) error {
	if err := t.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(f.TaskFile(), append(data, '\n'), 0o644)
}

// Status is the session state recorded in the status marker, for whoever
// looks at the folder from outside. The journal remains authoritative.
type Status string

const (
	StatusRunning   Status = "running"
	StatusPaused    Status = "paused"
	StatusStopped   Status = "stopped"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// WriteStatus records the session state in the status marker.
func (f Folder) WriteStatus(s Status) error {
	return WriteFileAtomic(f.StatusFile(), []byte(string(s)+"\n"), 0o644)
}

// ReadStatus returns the state in the status marker, or "" if there is none.
func (f Folder) ReadStatus() (Status, error) {
	data, err := os.ReadFile(f.StatusFile())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return Status(strings.TrimSpace(string(data))), err
}

// WriteFileAtomic writes a file so that readers see either the old or the
// new content, never a mix, and the new content survives a crash once it
// returns.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
