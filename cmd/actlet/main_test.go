package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no args prints usage", args: nil, wantCode: 2, wantStderr: "Usage:"},
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "Commands:"},
		{name: "help flag", args: []string{"--help"}, wantCode: 0, wantStdout: "Commands:"},
		{name: "unknown command", args: []string{"bogus"}, wantCode: 2, wantStderr: `unknown command "bogus"`},
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "actlet dev"},
		{name: "version help", args: []string{"version", "-h"}, wantCode: 0, wantStderr: "Usage: actlet version"},
		{name: "version bad flag", args: []string{"version", "-x"}, wantCode: 2, wantStderr: "flag provided but not defined"},
		{name: "version extra arg", args: []string{"version", "extra"}, wantCode: 2, wantStderr: `unexpected argument "extra"`},
		{name: "help lists exit codes", args: []string{"help"}, wantCode: 0, wantStdout: "3  the session paused"},
		{name: "token and token file", args: []string{"run", "-gateway", "g:1", "-token", "t", "-token-file", "t.txt"}, wantCode: 2, wantStderr: "not both"},
		{name: "token file in the session", args: []string{"run", "-gateway", "g:1", "-session", "s", "-token-file", "s/.actlet/token"}, wantCode: 2, wantStderr: "must not be inside the session folder"},
		{name: "journal moved", args: []string{"journal", "inspect", "x"}, wantCode: 2, wantStderr: "moved to actlet-cli"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestWithin(t *testing.T) {
	dir := filepath.Join("a", "s")
	for path, want := range map[string]bool{
		dir:                                 true,
		filepath.Join(dir, "token"):         true,
		filepath.Join(dir, "..", "s", "x"):  true,
		filepath.Join("a", "s2", "token"):   false,
		filepath.Join("a", "token"):         false,
		filepath.Join("a", "..token", "x"):  false,
		filepath.Join(dir, "..", "..", "x"): false,
	} {
		if got := within(path, dir); got != want {
			t.Errorf("within(%q, %q) = %v", path, dir, got)
		}
	}
}
