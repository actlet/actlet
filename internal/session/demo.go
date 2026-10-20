package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/actlet/actlet/internal/sessionfs"
)

// DemoKind is the kind of the built-in demonstration task. It stands in for
// real agent work: it runs a number of timed steps, emitting activity, and
// writes a deliverable when it completes. Each step behaves like a tool:
// asked to stop it winds down after a delay, and killing ends it at once.
const DemoKind = "demo"

// demoDefinition is the definition of a demo task.
type demoDefinition struct {
	// Steps is the number of steps. Default 3.
	Steps uint64 `json:"steps"`
	// Step is how long each step takes. Default 1s.
	Step duration `json:"step"`
	// Ticks is how many progress events each step emits.
	Ticks int `json:"ticks"`
	// TermDelay is how long a step takes to wind down when asked to stop.
	TermDelay duration `json:"term_delay"`
	// Output is the deliverable's file name in output/. Default result.txt.
	Output string `json:"output"`
}

// demoInput is the input of a demo task.
type demoInput struct {
	Message string `json:"message"`
}

type duration time.Duration

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"1s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if v < 0 {
		return errors.New("negative duration")
	}
	*d = duration(v)
	return nil
}

type demoTask struct {
	id        string
	steps     uint64
	step      time.Duration
	ticks     int
	termDelay time.Duration
	output    string
	message   string
}

func newDemoTask(t *sessionfs.Task) (*demoTask, error) {
	if t.Kind != DemoKind {
		return nil, fmt.Errorf("unknown task kind %q: this build runs %q tasks", t.Kind, DemoKind)
	}
	def := demoDefinition{Steps: 3, Step: duration(time.Second), Output: "result.txt"}
	if err := decodeStrict(t.Definition, &def); err != nil {
		return nil, fmt.Errorf("task definition: %w", err)
	}
	var in demoInput
	if err := decodeStrict(t.Input, &in); err != nil {
		return nil, fmt.Errorf("task input: %w", err)
	}
	if def.Output == "" || filepath.Base(def.Output) != def.Output || def.Output == "." || def.Output == ".." {
		return nil, fmt.Errorf("task definition: output must be a plain file name, not %q", def.Output)
	}
	return &demoTask{
		id: t.ID, steps: def.Steps, step: time.Duration(def.Step), ticks: def.Ticks,
		termDelay: time.Duration(def.TermDelay), output: def.Output, message: in.Message,
	}, nil
}

func decodeStrict(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

type stepResult string

const (
	stepDone        stepResult = "done"
	stepInterrupted stepResult = "interrupted"
	stepKilled      stepResult = "killed"
)

// runStep runs one step. Cancelling soft asks it to stop, which takes
// termDelay; cancelling hard ends it at once.
func (d *demoTask) runStep(soft, hard context.Context, step uint64, emit func(string, ...any)) stepResult {
	end := time.NewTimer(d.step)
	defer end.Stop()
	var tick <-chan time.Time
	if d.ticks > 0 {
		t := time.NewTicker(max(d.step/time.Duration(d.ticks+1), time.Millisecond))
		defer t.Stop()
		tick = t.C
	}
	n := 0
	for {
		select {
		case <-end.C:
			return stepDone
		case <-tick:
			n++
			if n <= d.ticks {
				emit("step %d: working (%d/%d)", step, n, d.ticks)
			}
		case <-hard.Done():
			return stepKilled
		case <-soft.Done():
			emit("step %d: asked to stop, winding down", step)
			wind := time.NewTimer(d.termDelay)
			defer wind.Stop()
			select {
			case <-wind.C:
				return stepInterrupted
			case <-hard.Done():
				return stepKilled
			}
		}
	}
}

// deliver writes the deliverable to the output directory.
func (d *demoTask) deliver(outputDir string) (string, error) {
	path := filepath.Join(outputDir, d.output)
	content := fmt.Sprintf("task %s completed %d steps\nmessage: %s\n", d.id, d.steps, d.message)
	return path, sessionfs.WriteFileAtomic(path, []byte(content), 0o644)
}
