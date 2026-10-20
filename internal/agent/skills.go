package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/actlet/actlet/internal/skill"
	"github.com/actlet/actlet/internal/tools"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// The prompt lists the skills' descriptions, and the skill tool loads a
// skill when the agent needs it.

// SkillTool loads a skill. It is offered whenever there are skills.
const SkillTool = "skill"

// Limits of the skill tool.
const (
	maxSkillOutput  = 64 << 10
	maxSkillListing = 200
)

// skillsPrompt lists the skills for the system prompt.
func skillsPrompt(skills []skill.Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Skills\n\nSkills hold instructions, scripts and resources for particular kinds of work. When one fits what you are doing, load it with the skill tool before you start, and follow it.\n\n")
	for _, s := range skills {
		fmt.Fprintf(&sb, "- %s: %s\n", s.Name, s.Description)
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

func (b *box) skillTool() *standardTool {
	skills := b.env.Settings.Skills
	var names []string
	for _, s := range skills {
		names = append(names, s.Name)
	}
	enum, _ := json.Marshal(names)
	return &standardTool{
		spec: &gatewayv0.ToolSpec{
			Name: SkillTool,
			Description: "Load a skill listed under Skills in your instructions: its instructions, and the files that come " +
				"with it. With file, read one of those files instead.",
			InputSchema: `{"type":"object","properties":{` +
				`"name":{"type":"string","enum":` + string(enum) + `},` +
				`"file":{"type":"string","description":"a file of the skill, such as reference/forms.md"}},` +
				`"required":["name"],"additionalProperties":false}`,
		},
		policy: "retry",
		run: func(_ *worker, _ string, input json.RawMessage) standardResult {
			var in struct {
				Name string `json:"name"`
				File string `json:"file"`
			}
			if err := decodeStrict(input, &in); err != nil {
				return refused("invalid input: %v", err)
			}
			i := slices.IndexFunc(skills, func(s skill.Skill) bool { return s.Name == in.Name })
			if i < 0 {
				return refused("no skill %q; the skills are %s", in.Name, strings.Join(names, ", "))
			}
			s := skills[i]
			if in.File == "" || in.File == skill.File {
				return loadSkill(s)
			}
			p, err := skill.CheckPath(in.File)
			if err != nil {
				return refused("%v", err)
			}
			data, err := skill.ReadFile(s.Dir, p)
			if err != nil {
				return refused("skill %s: %v", s.Name, err)
			}
			if !utf8.Valid(data) {
				return refused("skill %s: %s isn't text; use it with the shell or python tools, at %s", s.Name, p,
					filepath.Join(s.Dir, filepath.FromSlash(p)))
			}
			return standardResult{Result: tools.Result{Output: cut(string(data), maxSkillOutput)}}
		},
	}
}

// loadSkill returns a skill's instructions, where it is and its files.
func loadSkill(s skill.Skill) standardResult {
	body, err := s.Instructions()
	if err != nil {
		return refused("skill %s: %v", s.Name, err)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Skill %s, in %s. Its paths are relative to that folder: run its scripts with the shell or python tools, "+
		"and read its other files with this tool's file input.\n", s.Name, s.Dir)
	if files := skill.Files(s.Dir, maxSkillListing); len(files) > 0 {
		sb.WriteString("\nFiles:\n")
		for _, f := range files {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
	}
	sb.WriteString("\n")
	sb.WriteString(body)
	return standardResult{Result: tools.Result{Output: cut(sb.String(), maxSkillOutput)}}
}
