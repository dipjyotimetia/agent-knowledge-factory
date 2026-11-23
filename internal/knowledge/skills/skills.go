// Package skills reads Agent Skills directories.
// Scripts under scripts/ are inventoried and hashed. This package never executes them.
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Severity is a diagnostic level.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Diagnostic is one validator finding.
type Diagnostic struct {
	Path     string
	Severity Severity
	Message  string
}

// Script is an untrusted file under scripts/. Digest is sha256 of the bytes.
type Script struct {
	Path   string
	Digest string
	Bytes  int
}

// Skill is one SKILL.md directory.
type Skill struct {
	Dir           string
	Name          string
	Description   string
	License       string
	Compatibility string
	Metadata      map[string]string
	AllowedTools  string
	Body          string
	Scripts       []Script
	References    []string
	Assets        []string
	Diagnostics   []Diagnostic
}

// Valid reports whether the skill has no error diagnostics.
func (s *Skill) Valid() bool {
	return s != nil && !hasError(s.Diagnostics)
}

func hasError(list []Diagnostic) bool {
	for _, d := range list {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var markdownLink = regexp.MustCompile(`\[[^\]\n]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
var rootedRef = regexp.MustCompile(`(?:^|[\s` + "`" + `])((?:scripts|references|assets)/[A-Za-z0-9_./-]+)`)
var drivePath = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// Load reads one skill directory. It returns the skill even when it is invalid
// so callers can show the diagnostics. A missing SKILL.md is an error.
func Load(dir string) (*Skill, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("skill: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("skill: %s is not a directory", dir)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return nil, fmt.Errorf("skill: %w", err)
	}
	skill := &Skill{Dir: dir, Metadata: map[string]string{}}
	fm, body, has := splitFrontmatter(string(raw))
	if !has {
		skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
			Path: "SKILL.md", Severity: SeverityError, Message: "SKILL.md is missing YAML frontmatter",
		})
		return skill, nil
	}
	skill.Body = body
	meta, diags := parseFrontmatter(fm)
	skill.Diagnostics = append(skill.Diagnostics, diags...)
	if meta == nil {
		return skill, nil
	}
	skill.Name = stringField(meta, "name")
	skill.Description = stringField(meta, "description")
	skill.License = stringField(meta, "license")
	skill.Compatibility = stringField(meta, "compatibility")
	skill.AllowedTools = stringField(meta, "allowed-tools")
	validateName(skill, filepath.Base(dir))
	validateDescription(skill)
	validateCompatibility(skill, meta)
	validateMetadata(skill, meta["metadata"])
	validateBodyPaths(skill)
	if err := inventory(skill, "scripts", func(rel string, data []byte) {
		sum := sha256.Sum256(data)
		skill.Scripts = append(skill.Scripts, Script{
			Path:   rel,
			Digest: hex.EncodeToString(sum[:]),
			Bytes:  len(data),
		})
	}); err != nil {
		return nil, err
	}
	if err := inventory(skill, "references", func(rel string, _ []byte) {
		skill.References = append(skill.References, rel)
	}); err != nil {
		return nil, err
	}
	if err := inventory(skill, "assets", func(rel string, _ []byte) {
		skill.Assets = append(skill.Assets, rel)
	}); err != nil {
		return nil, err
	}
	return skill, nil
}

// LoadTree returns every directory under root that contains SKILL.md.
func LoadTree(root string) ([]*Skill, error) {
	var out []*Skill
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "SKILL.md" {
			return nil
		}
		skill, err := Load(filepath.Dir(path))
		if err != nil {
			return err
		}
		out = append(out, skill)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func validateName(skill *Skill, dirName string) {
	if skill.Name == "" {
		skill.Diagnostics = append(skill.Diagnostics, Diagnostic{Path: "SKILL.md", Severity: SeverityError, Message: "name is required"})
		return
	}
	if utf8.RuneCountInString(skill.Name) > 64 || !namePattern.MatchString(skill.Name) {
		skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
			Path: "SKILL.md", Severity: SeverityError,
			Message: "name must be 1-64 lowercase letters, digits, or single hyphens",
		})
	}
	if skill.Name != dirName {
		skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
			Path: "SKILL.md", Severity: SeverityError,
			Message: fmt.Sprintf("name %q must match directory %q", skill.Name, dirName),
		})
	}
}

func validateDescription(skill *Skill) {
	n := utf8.RuneCountInString(skill.Description)
	if strings.TrimSpace(skill.Description) == "" || n > 1024 {
		skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
			Path: "SKILL.md", Severity: SeverityError,
			Message: "description is required and must be 1-1024 characters",
		})
	}
}

func validateCompatibility(skill *Skill, meta map[string]any) {
	if _, ok := meta["compatibility"]; !ok {
		return
	}
	n := utf8.RuneCountInString(skill.Compatibility)
	if n < 1 || n > 500 {
		skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
			Path: "SKILL.md", Severity: SeverityError,
			Message: "compatibility must be 1-500 characters when present",
		})
	}
}

func validateMetadata(skill *Skill, raw any) {
	if raw == nil {
		return
	}
	m, ok := raw.(map[string]any)
	if !ok {
		skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
			Path: "SKILL.md", Severity: SeverityError, Message: "metadata must be a string map",
		})
		return
	}
	for key, value := range m {
		s, ok := value.(string)
		if !ok {
			skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
				Path: "SKILL.md", Severity: SeverityError,
				Message: fmt.Sprintf("metadata.%s must be a string", key),
			})
			continue
		}
		skill.Metadata[key] = s
	}
}

func validateBodyPaths(skill *Skill) {
	check := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || strings.Contains(ref, "://") {
			return
		}
		if strings.HasPrefix(ref, "/") || drivePath.MatchString(ref) {
			skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
				Path: "SKILL.md", Severity: SeverityError,
				Message: "path " + ref + " must be relative to the skill root",
			})
			return
		}
		cleaned := filepath.Clean(filepath.Join(skill.Dir, filepath.FromSlash(ref)))
		rel, err := filepath.Rel(skill.Dir, cleaned)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
				Path: "SKILL.md", Severity: SeverityError,
				Message: "path " + ref + " escapes the skill directory",
			})
		}
	}
	for _, match := range markdownLink.FindAllStringSubmatch(skill.Body, -1) {
		check(match[1])
	}
	for _, match := range rootedRef.FindAllStringSubmatch(skill.Body, -1) {
		check(match[1])
	}
}

func inventory(skill *Skill, sub string, visit func(rel string, data []byte)) error {
	dir := filepath.Join(skill.Dir, sub)
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		// Read bytes only. There is no exec, no interpreter, and no script hook.
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(skill.Dir, path)
		if err != nil {
			return err
		}
		visit(filepath.ToSlash(rel), data)
		return nil
	})
}

func splitFrontmatter(text string) (string, string, bool) {
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return "", text, false
	}
	rest := text[4:]
	if strings.HasPrefix(text, "---\r\n") {
		rest = text[5:]
	}
	end := strings.Index(rest, "\n---\n")
	endLen := 5
	if end < 0 {
		end = strings.Index(rest, "\n---\r\n")
		endLen = 6
	}
	if end < 0 {
		return "", text, false
	}
	return rest[:end], rest[end+endLen:], true
}

func parseFrontmatter(fm string) (map[string]any, []Diagnostic) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &node); err != nil {
		return nil, []Diagnostic{{Path: "SKILL.md", Severity: SeverityError, Message: "frontmatter is not YAML: " + err.Error()}}
	}
	if len(node.Content) == 0 || node.Content[0].Kind != yaml.MappingNode {
		return nil, []Diagnostic{{Path: "SKILL.md", Severity: SeverityError, Message: "frontmatter is not a mapping"}}
	}
	var meta map[string]any
	if err := node.Content[0].Decode(&meta); err != nil {
		return nil, []Diagnostic{{Path: "SKILL.md", Severity: SeverityError, Message: err.Error()}}
	}
	return meta, nil
}

func stringField(meta map[string]any, key string) string {
	v, ok := meta[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprint(v)
	}
	return s
}
