//go:build ignore

// generate writes the compatibility fixtures of the current format
// version into test/compat/<version>, from the demo session: the session
// folder's journal, the gateway's recording, its JSON Lines export and
// view, and the demo's profile, as YAML and JSON, and task.json. It
// needs the binaries: make build, then
//
//	go run test/compat/generate.go v0
//
// Fixtures of a released version are never regenerated: they are what
// later versions must still read.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/actlet/actlet/sdk"
)

func main() {
	if len(os.Args) != 2 {
		fail(fmt.Errorf("usage: go run test/compat/generate.go <version>"))
	}
	out := filepath.Join("test", "compat", os.Args[1])
	if err := os.MkdirAll(out, 0o755); err != nil {
		fail(err)
	}
	tmp, err := os.MkdirTemp("", "actlet-compat-")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(tmp)
	demo := filepath.Join("examples", "demo")
	session, data := filepath.Join(tmp, "session"), filepath.Join(tmp, "gateway")
	for _, d := range []string{filepath.Join(session, "inputs"), data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fail(err)
		}
	}
	inputs, _ := filepath.Glob(filepath.Join(demo, "inputs", "*"))
	for _, in := range inputs {
		copyFile(in, filepath.Join(session, "inputs", filepath.Base(in)))
	}
	// The demo, without its pauses.
	model, err := os.ReadFile(filepath.Join(demo, "model.yaml"))
	if err != nil {
		fail(err)
	}
	model = regexp.MustCompile(`delay: \d+s`).ReplaceAll(model, []byte("delay: 200ms"))
	modelPath := filepath.Join(tmp, "model.yaml")
	if err := os.WriteFile(modelPath, model, 0o600); err != nil {
		fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const addr = "127.0.0.1:7498"
	gw := exec.CommandContext(ctx, "bin/actlet-devgw", "-listen", addr, "-token", "compat", "-data", data,
		"-task", filepath.Join(demo, "task.json"), "-profile", filepath.Join(demo, "profile.yaml"), "-model", modelPath)
	stdin, err := gw.StdinPipe()
	if err != nil {
		fail(err)
	}
	if err := gw.Start(); err != nil {
		fail(err)
	}
	defer func() { _ = gw.Process.Kill(); _ = gw.Wait() }()
	time.Sleep(time.Second)
	go func() {
		time.Sleep(time.Second)
		fmt.Fprintln(stdin, "steer Please keep the report short, and quote every figure in EUR.")
	}()
	run := exec.CommandContext(ctx, "bin/actlet", "run", "-session", session, "-log-level", "warn")
	run.Env = append(os.Environ(), "ACTLET_GATEWAY="+addr, "ACTLET_TOKEN=compat", "ACTLET_TOOLS_MANIFEST=", "ACTLET_SKILLS=")
	run.Stdout, run.Stderr = os.Stderr, os.Stderr
	if err := run.Run(); err != nil {
		fail(fmt.Errorf("the demo run: %w", err))
	}
	time.Sleep(500 * time.Millisecond) // the last activity reaches the recording
	_ = gw.Process.Kill()
	_ = gw.Wait()

	copyFile(filepath.Join(session, ".actlet", "session.actlet"), filepath.Join(out, "session.actlet"))
	recs, _ := filepath.Glob(filepath.Join(data, "*.actrec"))
	if len(recs) != 1 {
		fail(fmt.Errorf("%d recordings", len(recs)))
	}
	copyFile(recs[0], filepath.Join(out, "recording.actrec"))
	cli := func(name string, args ...string) {
		b, err := exec.Command("bin/actlet-cli", args...).Output()
		if err != nil {
			fail(fmt.Errorf("actlet-cli %v: %w", args, err))
		}
		if err := os.WriteFile(filepath.Join(out, name), b, 0o644); err != nil {
			fail(err)
		}
	}
	cli("events.jsonl", "replay", "export", "--jsonl", filepath.Join(out, "recording.actrec"))
	cli("view.json", "replay", "view", "--json", filepath.Join(out, "recording.actrec"))

	copyFile(filepath.Join(demo, "task.json"), filepath.Join(out, "task.json"))
	copyFile(filepath.Join(demo, "profile.yaml"), filepath.Join(out, "profile.yaml"))
	p, err := sdk.LoadProfile(filepath.Join(demo, "profile.yaml"))
	if err != nil {
		fail(err)
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(filepath.Join(out, "profile.json"), append(b, '\n'), 0o644); err != nil {
		fail(err)
	}
	fmt.Println("wrote", out)
}

func copyFile(from, to string) {
	b, err := os.ReadFile(from)
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "generate:", err)
	os.Exit(1)
}
