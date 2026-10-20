package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		wantCode int
		want     string
	}{
		{nil, 2, "Usage:"},
		{[]string{"help"}, 0, "replay"},
		{[]string{"version"}, 0, "actlet-cli dev"},
		{[]string{"bogus"}, 2, `unknown command "bogus"`},
		{[]string{"replay"}, 2, "inspect"},
		{[]string{"replay", "help"}, 0, "inspect"},
		{[]string{"replay", "bogus"}, 2, `unknown command "bogus"`},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code != tc.wantCode {
			t.Errorf("%q: exit %d, want %d", tc.args, code, tc.wantCode)
		}
		if out := stdout.String() + stderr.String(); !strings.Contains(out, tc.want) {
			t.Errorf("%q: output %q, want %q", tc.args, out, tc.want)
		}
	}
}
