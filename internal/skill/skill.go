// Package skill reads and writes skills: Claude-style skill folders with a
// SKILL.md, whose front matter names and describes the skill and whose
// body holds its instructions, and optional scripts and resources.
package skill

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// File is the file that makes a folder a skill.
const File = "SKILL.md"

// Limits.
const (
	// MaxSkills bounds the skills a session has.
	MaxSkills = 100
	// MaxDescription bounds a skill's description.
	MaxDescription = 1024
	// MaxFile bounds a file read from a skill.
	MaxFile = 1 << 20
	// MaxFiles bounds the files of a skill in a profile, or listed, and
	// MaxProfile the size of all the profile's skills' files.
	MaxFiles   = 200
	MaxProfile = 2 << 20
)

// Name is what a skill may be called.
var Name = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Skill is a skill in a folder.
type Skill struct {
	Name, Description string
	Dir               string
	// Source says where it comes from, such as "image" or "profile".
	Source string
}

// meta is SKILL.md's front matter. Other fields, such as a license, are
// allowed and ignored.
type meta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Read reads the skill in a folder.
func Read(dir string) (Skill, error) {
	data, err := ReadFile(dir, File)
	if err != nil {
		return Skill{}, err
	}
	m, _, err := split(data)
	if err != nil {
		return Skill{}, fmt.Errorf("%s: %w", File, err)
	}
	s := Skill{Name: m.Name, Description: strings.Join(strings.Fields(m.Description), " "), Dir: dir}
	folder := filepath.Base(dir)
	switch {
	case s.Name == "":
		s.Name = folder
	case s.Name != folder:
		return Skill{}, fmt.Errorf("%s names the skill %q, but its folder is %q", File, s.Name, folder)
	}
	switch {
	case !Name.MatchString(s.Name):
		return Skill{}, fmt.Errorf("name %q: use 1 to 64 lowercase letters, digits and hyphens", s.Name)
	case s.Description == "":
		return Skill{}, fmt.Errorf("%s has no description", File)
	case len(s.Description) > MaxDescription:
		return Skill{}, fmt.Errorf("the description is longer than %d bytes", MaxDescription)
	}
	return s, nil
}

// Instructions returns the body of a skill's SKILL.md.
func (s Skill) Instructions() (string, error) {
	data, err := ReadFile(s.Dir, File)
	if err != nil {
		return "", err
	}
	_, body, err := split(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", File, err)
	}
	return strings.TrimSpace(string(body)), nil
}

// split splits SKILL.md into its front matter and its body.
func split(data []byte) (meta, []byte, error) {
	var m meta
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	rest, ok := bytes.CutPrefix(data, []byte("---\n"))
	if !ok {
		rest, ok = bytes.CutPrefix(data, []byte("---\r\n"))
	}
	if !ok {
		return m, nil, errors.New("it doesn't start with front matter between --- lines")
	}
	front, body, ok := bytes.Cut(rest, []byte("\n---"))
	if !ok {
		return m, nil, errors.New("its front matter doesn't end with a --- line")
	}
	if err := yaml.Unmarshal(front, &m); err != nil {
		return m, nil, fmt.Errorf("front matter: %w", err)
	}
	_, body, _ = bytes.Cut(body, []byte("\n"))
	return m, body, nil
}

// Discover finds the skills in directories: each folder in them with a
// SKILL.md. A skill in a later directory replaces one of the same name in
// an earlier one. Directories that don't exist are skipped, and skills
// that can't be read are left out, with a warning each.
func Discover(dirs ...string) (skills []Skill, warnings []string) {
	byName := map[string]int{}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skills in %s: %v", dir, err))
			continue
		}
		for _, e := range entries {
			sub := filepath.Join(dir, e.Name())
			if fi, err := os.Stat(sub); err != nil || !fi.IsDir() {
				continue
			}
			s, err := Read(sub)
			if errors.Is(err, fs.ErrNotExist) {
				continue // a folder without SKILL.md
			}
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("skill %s: %v", sub, err))
				continue
			}
			if i, ok := byName[s.Name]; ok {
				skills[i] = s
				continue
			}
			if len(skills) == MaxSkills {
				warnings = append(warnings, fmt.Sprintf("skill %s: more than %d skills", sub, MaxSkills))
				continue
			}
			byName[s.Name] = len(skills)
			skills = append(skills, s)
		}
	}
	slices.SortFunc(skills, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
	return skills, warnings
}

// CheckPath checks a path in a skill's folder: relative, with forward
// slashes, and inside the folder. It returns it cleaned.
func CheckPath(p string) (string, error) {
	clean := path.Clean(p)
	if p == "" || strings.Contains(p, `\`) || path.IsAbs(p) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("file %q: use a relative path inside the skill's folder", p)
	}
	return clean, nil
}

// ReadFile reads a file in a skill's folder, which it may not leave, not
// even by a symbolic link.
func ReadFile(dir, p string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(filepath.FromSlash(p))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%s is a folder", p)
	}
	if fi.Size() > MaxFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", p, MaxFile)
	}
	return io.ReadAll(io.LimitReader(f, MaxFile))
}

// Files lists up to max files in a skill's folder, other than SKILL.md,
// with forward slashes, leaving out hidden ones.
func Files(dir string, limit int) []string {
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == File {
			return nil
		}
		if len(files) == limit {
			return fs.SkipAll
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files
}

// Load reads a skill folder whole, to send in a profile.
func Load(dir string) (*gatewayv0.Skill, error) {
	s, err := Read(dir)
	if err != nil {
		return nil, fmt.Errorf("skill %s: %w", dir, err)
	}
	out := &gatewayv0.Skill{Name: s.Name}
	files := append([]string{File}, Files(dir, MaxFiles+1)...)
	if len(files) > MaxFiles {
		return nil, fmt.Errorf("skill %s: more than %d files", s.Name, MaxFiles)
	}
	for _, f := range files {
		data, err := ReadFile(dir, f)
		if err != nil {
			return nil, fmt.Errorf("skill %s: %w", s.Name, err)
		}
		fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f)))
		if err != nil {
			return nil, err
		}
		out.Files = append(out.Files, &gatewayv0.SkillFile{Path: f, Content: data, Executable: fi.Mode()&0o111 != 0})
	}
	return out, nil
}

// Size is the size of a skill's files.
func Size(s *gatewayv0.Skill) int {
	n := 0
	for _, f := range s.GetFiles() {
		n += len(f.GetContent())
	}
	return n
}

// Check checks a skill from a profile: its name, its files' paths, and
// that it has a SKILL.md.
func Check(s *gatewayv0.Skill) error {
	if !Name.MatchString(s.GetName()) {
		return fmt.Errorf("skill name %q: use 1 to 64 lowercase letters, digits and hyphens", s.GetName())
	}
	if len(s.GetFiles()) > MaxFiles {
		return fmt.Errorf("skill %s: more than %d files", s.GetName(), MaxFiles)
	}
	seen := map[string]bool{}
	for _, f := range s.GetFiles() {
		p, err := CheckPath(f.GetPath())
		if err != nil {
			return fmt.Errorf("skill %s: %w", s.GetName(), err)
		}
		if seen[p] {
			return fmt.Errorf("skill %s: file %q twice", s.GetName(), p)
		}
		seen[p] = true
	}
	if !seen[File] {
		return fmt.Errorf("skill %s: no %s", s.GetName(), File)
	}
	return nil
}

// Write writes a skill from a profile into a folder of its name in dir,
// replacing what was there.
func Write(dir string, s *gatewayv0.Skill) error {
	if err := Check(s); err != nil {
		return err
	}
	folder := filepath.Join(dir, s.GetName())
	if err := os.RemoveAll(folder); err != nil {
		return err
	}
	for _, f := range s.GetFiles() {
		p, _ := CheckPath(f.GetPath())
		target := filepath.Join(folder, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if f.GetExecutable() {
			mode = 0o755
		}
		if err := os.WriteFile(target, f.GetContent(), mode); err != nil {
			return err
		}
	}
	return nil
}
