//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/actlet/actlet/internal/agent"
	"github.com/actlet/actlet/internal/session"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A skill from the image and one from the profile are both listed in the
// agent's prompt, loaded with the skill tool, and their scripts run.
func TestSkills(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	image := t.TempDir()
	writeFiles(t, image, map[string]string{
		"greet/SKILL.md":         "---\nname: greet\ndescription: Greet people from the image.\n---\nIMG-INSTRUCTIONS: run scripts/hello.sh with sh.\n",
		"greet/scripts/hello.sh": "echo IMG-OK\n",
	})
	src := t.TempDir()
	writeFiles(t, src, map[string]string{
		"count/SKILL.md":         "---\nname: count\ndescription: Count things, from the profile.\n---\nPROF-INSTRUCTIONS: run scripts/count.sh.\n",
		"count/scripts/count.sh": "#!/bin/sh\necho PROF-OK\n",
	})
	if err := os.Chmod(filepath.Join(src, "count", "scripts", "count.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	fromProfile, err := sdk.LoadSkill(filepath.Join(src, "count"))
	if err != nil {
		t.Fatal(err)
	}
	dir := sessionDir(t, nil)
	script := fmt.Sprintf(`
turns:
  - tool_calls:
      - name: skill
        input: {name: greet}
  - expect:
      - contains: IMG-INSTRUCTIONS
    tool_calls:
      - name: shell
        input: {command: "sh %s"}
  - expect:
      - contains: IMG-OK
    tool_calls:
      - name: skill
        input: {name: count}
  - expect:
      - contains: PROF-INSTRUCTIONS
    tool_calls:
      - name: shell
        input: {command: "%s"}
  - expect:
      - contains: PROF-OK
    text: Greeted and counted.
`, filepath.Join(image, "greet", "scripts", "hello.sh"), filepath.Join(dir, ".actlet", "skills", "count", "scripts", "count.sh"))
	g := newAgentGateway(t, script, func(g *agentGateway) {
		g.skills = []*gatewayv0.Skill{fromProfile}
		g.skillsDir = image
	})
	if code := exitCode(t, g.worker(dir, "")); code != session.ExitCompleted {
		t.Fatalf("exit %d, want 0", code)
	}
	g.mu.Lock()
	system := g.requests[0].GetSystem()
	g.mu.Unlock()
	for _, want := range []string{"## Skills", "- count: Count things, from the profile.", "- greet: Greet people from the image."} {
		if !strings.Contains(system, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, system)
		}
	}
	// The calls are journaled and in the activity, for replays.
	if n := len(toolOutcomes(t, dir, agent.RootWorker)); n != 4 {
		t.Errorf("%d tool outcomes", n)
	}
	if n := g.eventCount(func(ev *gatewayv0.ActivityEvent) bool {
		f := ev.GetToolFinished()
		return f.GetTool() == agent.SkillTool && !f.GetIsError()
	}); n != 2 {
		t.Errorf("%d skill calls in the activity", n)
	}
}
