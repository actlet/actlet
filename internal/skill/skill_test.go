package skill

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// write writes files under dir.
func write(t *testing.T, dir string, files map[string]string) {
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

const pdf = "---\nname: pdf\ndescription: >\n  Fill in PDF forms.\n  Use it for any PDF.\nlicense: MIT\n---\n# PDF\n\nRun scripts/fill.py.\n"

func TestRead(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"pdf/SKILL.md":        pdf,
		"pdf/scripts/fill.py": "print('filled')\n",
		"pdf/.git/HEAD":       "ref",
		"unnamed/SKILL.md":    "\xef\xbb\xbf---\r\ndescription: Named after its folder.\r\n---\r\nBody.\r\n",
		"mismatch/SKILL.md":   "---\nname: other\ndescription: x\n---\n",
		"nodesc/SKILL.md":     "---\nname: nodesc\n---\n",
		"nofront/SKILL.md":    "# Just text\n",
		"unclosed/SKILL.md":   "---\nname: unclosed\n",
		"Bad_Name/SKILL.md":   "---\ndescription: x\n---\n",
		"long/SKILL.md":       "---\ndescription: " + strings.Repeat("x", MaxDescription+1) + "\n---\n",
	})
	s, err := Read(filepath.Join(dir, "pdf"))
	if err != nil || s.Name != "pdf" || s.Description != "Fill in PDF forms. Use it for any PDF." {
		t.Fatalf("%+v, %v", s, err)
	}
	if body, err := s.Instructions(); err != nil || body != "# PDF\n\nRun scripts/fill.py." {
		t.Errorf("instructions %q, %v", body, err)
	}
	if files := Files(s.Dir, 10); !slices.Equal(files, []string{"scripts/fill.py"}) {
		t.Errorf("files %v", files)
	}
	if s, err := Read(filepath.Join(dir, "unnamed")); err != nil || s.Name != "unnamed" || s.Description != "Named after its folder." {
		t.Errorf("unnamed: %+v, %v", s, err)
	}
	for _, name := range []string{"mismatch", "nodesc", "nofront", "unclosed", "Bad_Name", "long"} {
		if _, err := Read(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}

// Later directories replace earlier skills of the same name; broken
// skills are warned about and left out.
func TestDiscover(t *testing.T) {
	image, profile := t.TempDir(), t.TempDir()
	write(t, image, map[string]string{
		"pdf/SKILL.md":    pdf,
		"csv/SKILL.md":    "---\ndescription: Image CSV.\n---\n",
		"broken/SKILL.md": "no front matter",
		"notes/README":    "not a skill",
		"loose.md":        "a file",
	})
	write(t, profile, map[string]string{"csv/SKILL.md": "---\ndescription: Profile CSV.\n---\n"})
	skills, warnings := Discover(image, filepath.Join(image, "missing"), "", profile)
	if len(skills) != 2 || skills[0].Name != "csv" || skills[0].Description != "Profile CSV." || skills[1].Name != "pdf" {
		t.Errorf("skills %+v", skills)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "broken") {
		t.Errorf("warnings %v", warnings)
	}
}

func TestCheckPath(t *testing.T) {
	for p, ok := range map[string]bool{
		"SKILL.md": true, "scripts/a.py": true, "a/../b": true,
		"": false, ".": false, "..": false, "../x": false, "a/../../x": false, "/etc/passwd": false, `a\b`: false,
	} {
		if _, err := CheckPath(p); (err == nil) != ok {
			t.Errorf("%q: %v", p, err)
		}
	}
}

// A skill's files can't lead out of its folder, not by a symbolic link.
func TestReadFileStaysInside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links")
	}
	dir := t.TempDir()
	write(t, dir, map[string]string{"secret": "s3cret", "s/SKILL.md": pdf})
	if err := os.Symlink(filepath.Join(dir, "secret"), filepath.Join(dir, "s", "link")); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadFile(filepath.Join(dir, "s"), "link"); err == nil {
		t.Errorf("read %q through a link", data)
	}
	if _, err := ReadFile(filepath.Join(dir, "s"), "../secret"); err == nil {
		t.Error("read outside")
	}
}

// A skill loaded from a folder and written elsewhere comes out the same,
// scripts executable.
func TestLoadAndWrite(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "pdf"), map[string]string{"SKILL.md": pdf, "scripts/fill.sh": "echo filled\n", "ref/forms.md": "forms"})
	if err := os.Chmod(filepath.Join(src, "pdf", "scripts", "fill.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Load(filepath.Join(src, "pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if s.GetName() != "pdf" || len(s.GetFiles()) != 3 || s.GetFiles()[0].GetPath() != File || Size(s) != len(pdf)+len("echo filled\n")+len("forms") {
		t.Fatalf("loaded %v", s)
	}
	dst := t.TempDir()
	write(t, dst, map[string]string{"pdf/stale.txt": "from before"})
	if err := Write(dst, s); err != nil {
		t.Fatal(err)
	}
	got, err := Read(filepath.Join(dst, "pdf"))
	if err != nil || got.Description != "Fill in PDF forms. Use it for any PDF." {
		t.Errorf("written: %+v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "pdf", "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("a stale file stayed: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dst, "pdf", "scripts", "fill.sh"))
		if err != nil || fi.Mode()&0o100 == 0 {
			t.Errorf("script mode %v, %v", fi.Mode(), err)
		}
	}

	for name, bad := range map[string]*gatewayv0.Skill{
		"bad name":    {Name: "Bad", Files: []*gatewayv0.SkillFile{{Path: File}}},
		"no SKILL.md": {Name: "x", Files: []*gatewayv0.SkillFile{{Path: "a.md"}}},
		"escape":      {Name: "x", Files: []*gatewayv0.SkillFile{{Path: File}, {Path: "../evil"}}},
		"twice":       {Name: "x", Files: []*gatewayv0.SkillFile{{Path: File}, {Path: "./SKILL.md"}}},
	} {
		if err := Write(dst, bad); err == nil {
			t.Errorf("%s: written", name)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "evil")); !os.IsNotExist(err) {
		t.Errorf("wrote outside: %v", err)
	}
}
