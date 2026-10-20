package buildinfo

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = oldVersion, oldCommit, oldDate })
	Version, Commit, Date = "v1.2.3", "abc1234", "2026-01-02T03:04:05Z"

	var stdout, stderr bytes.Buffer
	if code := RunVersion("tool", nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"tool v1.2.3", "commit:   abc1234", "built:    2026-01-02T03:04:05Z", "platform: "} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
		}
	}
	for args, want := range map[string]int{"-h": 0, "-x": 2, "extra": 2} {
		stdout.Reset()
		if code := RunVersion("tool", []string{args}, &stdout, &stderr); code != want {
			t.Errorf("version %s: exit %d, want %d", args, code, want)
		}
	}
}
