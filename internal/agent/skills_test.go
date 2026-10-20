package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/actlet/actlet/internal/skill"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// The prompt lists the skills; the skill tool loads one, then one of its
// files, and refuses paths out of its folder.
func TestSkillTool(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"csv-report/SKILL.md":          "---\nname: csv-report\ndescription: Write reports from CSV files.\n---\n# CSV reports\n\nRun scripts/report.sh on the file.\n",
		"csv-report/scripts/report.sh": "echo report\n",
		"csv-report/reference.md":      "Columns: region, total.\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	skills, warnings := skill.Discover(dir)
	if len(skills) != 1 || len(warnings) != 0 {
		t.Fatalf("skills %v, warnings %v", skills, warnings)
	}
	call := func(input string) [][2]string { return [][2]string{{SkillTool, input}} }
	s := newSandbox(t,
		turn{calls: call(`{"name":"csv-report"}`), check: func(req *gatewayv0.InferenceRequest) error {
			if !strings.Contains(req.GetSystem(), "## Skills") || !strings.Contains(req.GetSystem(), "- csv-report: Write reports from CSV files.") {
				return fmt.Errorf("system prompt %q", req.GetSystem())
			}
			if !slices.ContainsFunc(req.GetTools(), func(sp *gatewayv0.ToolSpec) bool {
				return sp.GetName() == SkillTool && strings.Contains(sp.GetInputSchema(), `"enum":["csv-report"]`)
			}) {
				return fmt.Errorf("no skill tool in %v", req.GetTools())
			}
			return nil
		}},
		turn{check: expectAll("Run scripts/report.sh on the file.", "- scripts/report.sh", "- reference.md", "Skill csv-report, in "+filepath.Join(dir, "csv-report")), calls: call(`{"name":"csv-report","file":"reference.md"}`)},
		turn{check: expect("Columns: region, total."), calls: call(`{"name":"csv-report","file":"../csv-report/../../etc/passwd"}`)},
		turn{check: expect("relative path inside"), calls: call(`{"name":"pdf"}`)},
		turn{check: expect(`no skill "pdf"`), text: "done"},
	)
	s.settings.Skills = skills
	s.mustFinish()
	if n := s.count(KindToolOutcome); n != 4 {
		t.Errorf("%d tool outcomes", n)
	}
	infos, _, err := Tools(s.settings, s.folder)
	if err != nil || !slices.ContainsFunc(infos, func(i *gatewayv0.ToolInfo) bool { return i.GetName() == SkillTool && i.GetPolicy() == "retry" }) {
		t.Errorf("tools %v, %v", infos, err)
	}
}

// expectAll checks that the previous turn's one tool result contains each
// text.
func expectAll(want ...string) func(*gatewayv0.InferenceRequest) error {
	return func(req *gatewayv0.InferenceRequest) error {
		got := lastResults(req)
		if len(got) != 1 {
			return fmt.Errorf("%d tool results, want 1", len(got))
		}
		for _, w := range want {
			if !strings.Contains(got[0].GetContent(), w) {
				return fmt.Errorf("tool result %q lacks %q", got[0].GetContent(), w)
			}
		}
		return nil
	}
}
