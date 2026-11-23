// Package okf reads and validates an Open Knowledge Format v0.3 bundle.
// It does not execute attesters, executors, or any other file in the bundle.
package okf

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Severity is a diagnostic level. Error means the bundle is not conformant.
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

func (d Diagnostic) String() string {
	return fmt.Sprintf("%s: %s: %s", d.Path, d.Severity, d.Message)
}

// Timestamp is an OKF instant. Date-only values are UTC midnight.
type Timestamp struct {
	Time  time.Time
	Raw   string
	Valid bool
}

// ActorEvent is a generated or verified record.
type ActorEvent struct {
	By string
	At Timestamp
}

// Source is one provenance entry. Ignored is set when the required resource is missing.
type Source struct {
	ID           string
	Resource     string
	Title        string
	Author       string
	UsageCount   *int
	LastModified Timestamp
	Ignored      bool
}

// Bundle is a directory of concepts plus the diagnostics raised while reading it.
type Bundle struct {
	Root        string
	Version     string
	Title       string
	Description string
	License     string
	URIPrefix   string
	Concepts    []Concept
	Diagnostics []Diagnostic
}

// Conformant reports whether any error-level diagnostic was raised.
func (b *Bundle) Conformant() bool {
	for _, d := range b.Diagnostics {
		if d.Severity == SeverityError {
			return false
		}
	}
	return true
}

// Concept is one OKF document. Index and log files are never concepts.
type Concept struct {
	ID             string
	Path           string
	Type           string
	Title          string
	Description    string
	Body           string
	Status         string
	StatusUnknown  bool
	TrustTier      string
	SupersededBy   string
	Stale          bool
	StaleMalformed bool
	Tags           []string
	Resource       string
	Sources        []Source
	Headings       []string
	Links          []Link
	Extensions     map[string]any
	Generated      *ActorEvent
	Verified       []ActorEvent
	Runtime        string
	Attestable     bool
	AttesterPath   string
}

// Link is a Markdown link resolved against the bundle.
type Link struct {
	Raw    string
	Target string
	Kind   string // url, bundle, external
}

// Options controls reading.
type Options struct {
	Now time.Time
}

// Load reads a bundle. An IO error opening the root is returned as an error.
// Malformed documents are reported as diagnostics; callers use Conformant.
func Load(root string, opt Options) (*Bundle, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("okf bundle: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("okf bundle: %s is not a directory", root)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if opt.Now.IsZero() {
		opt.Now = time.Now().UTC()
	}
	bundle := &Bundle{Root: root, URIPrefix: "kb://bundle"}
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			if entry.IsDir() && path != root {
				return filepath.SkipDir
			}
			if !entry.IsDir() {
				return nil
			}
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if link, err := isEscapingSymlink(root, path); err != nil {
			return err
		} else if link {
			bundle.Diagnostics = append(bundle.Diagnostics, Diagnostic{
				Path: rel, Severity: SeverityError,
				Message: "symlink resolves outside the bundle and was not read",
			})
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(name), ".md") {
			return nil
		}
		file, err := base.Open(rel)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(file, 8<<20))
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if bytes.Contains(body, []byte{0}) {
			bundle.Diagnostics = append(bundle.Diagnostics, Diagnostic{
				Path: rel, Severity: SeverityError, Message: "file is not UTF-8 text",
			})
			return nil
		}
		switch name {
		case "index.md":
			bundle.readIndex(rel, path, body)
		case "log.md":
			bundle.readLog(rel, body)
		default:
			bundle.readConcept(rel, path, body, opt.Now)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return bundle, nil
}

func (b *Bundle) readIndex(rel, path string, raw []byte) {
	fm, body, has := splitFrontmatter(string(raw))
	if rel != "index.md" {
		if has {
			b.Diagnostics = append(b.Diagnostics, Diagnostic{
				Path: rel, Severity: SeverityError,
				Message: "a subdirectory index.md must not have frontmatter",
			})
		}
		return
	}
	if !has {
		return
	}
	meta, diags := parseFrontmatter(fm)
	for _, d := range diags {
		d.Path = rel
		b.Diagnostics = append(b.Diagnostics, d)
	}
	if meta == nil {
		return
	}
	b.Version = scalarString(meta["okf_version"])
	b.Title = scalarString(meta["title"])
	b.Description = scalarString(meta["description"])
	b.License = scalarString(meta["license"])
	if prefix := scalarString(meta["x-akf-uri-prefix"]); prefix != "" {
		b.URIPrefix = strings.TrimRight(prefix, "/")
	}
	_ = body
	_ = path
}

func (b *Bundle) readLog(rel string, raw []byte) {
	if _, _, has := splitFrontmatter(string(raw)); has {
		b.Diagnostics = append(b.Diagnostics, Diagnostic{
			Path: rel, Severity: SeverityError, Message: "log.md must not have frontmatter",
		})
		return
	}
	dateHeading := regexp.MustCompile(`^##\s+(\S+)\s*$`)
	for _, line := range strings.Split(string(raw), "\n") {
		m := dateHeading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if _, err := time.Parse("2006-01-02", m[1]); err != nil {
			b.Diagnostics = append(b.Diagnostics, Diagnostic{
				Path: rel, Severity: SeverityError,
				Message: fmt.Sprintf("log date heading %q is not YYYY-MM-DD", m[1]),
			})
		}
	}
}

func (b *Bundle) readConcept(rel, path string, raw []byte, now time.Time) {
	text := string(raw)
	fm, body, has := splitFrontmatter(text)
	if !has {
		if strings.HasPrefix(text, "---\n") || strings.HasPrefix(text, "---\r\n") {
			b.Diagnostics = append(b.Diagnostics, Diagnostic{
				Path: rel, Severity: SeverityError, Message: "frontmatter is not closed",
			})
			return
		}
		if !allowedUntyped(filepath.Base(rel)) {
			b.Diagnostics = append(b.Diagnostics, Diagnostic{
				Path: rel, Severity: SeverityWarning, Message: "markdown file has no frontmatter and is not a concept",
			})
		}
		return
	}
	meta, diags := parseFrontmatter(fm)
	for _, d := range diags {
		d.Path = rel
		b.Diagnostics = append(b.Diagnostics, d)
	}
	if meta == nil {
		return
	}
	typ := scalarString(meta["type"])
	if typ == "" {
		// SKILL.md is an Agent Skills file. The skills package validates it.
		// OKF explicitly allows it in a bundle and does not treat it as a concept.
		if filepath.Base(rel) != "SKILL.md" {
			b.Diagnostics = append(b.Diagnostics, Diagnostic{
				Path: rel, Severity: SeverityWarning, Message: "frontmatter has no type; file is not a concept",
			})
		}
		return
	}
	concept := Concept{
		ID:          strings.TrimSuffix(rel, ".md"),
		Path:        rel,
		Type:        typ,
		Title:       scalarString(meta["title"]),
		Description: scalarString(meta["description"]),
		Body:        body,
		Resource:    scalarString(meta["resource"]),
		Tags:        stringList(meta["tags"]),
		Headings:    headings(body),
		Links:       links(b.Root, path, body),
		Extensions:  extensions(meta),
		Runtime:     scalarString(meta["runtime"]),
		Status:      "stable",
	}
	if concept.Title == "" {
		concept.Title = filepath.Base(concept.ID)
	}
	applyLifecycle(&concept, meta, rel, &b.Diagnostics)
	applyTrust(&concept, meta, rel, &b.Diagnostics)
	applySources(&concept, meta, rel, &b.Diagnostics)
	applyStale(&concept, meta, rel, now, &b.Diagnostics)
	if concept.Type == "Attested Computation" {
		applyAttestation(&concept, meta, rel, &b.Diagnostics)
	}
	b.Concepts = append(b.Concepts, concept)
}

func allowedUntyped(name string) bool {
	switch name {
	case "README.md", "AGENTS.md", "CONTRIBUTING.md", "SKILL.md":
		return true
	default:
		return false
	}
}

func applyLifecycle(c *Concept, meta map[string]any, rel string, diags *[]Diagnostic) {
	raw, ok := meta["status"]
	if !ok || raw == nil || raw == "" {
		c.Status = "stable"
	} else {
		status := scalarString(raw)
		switch status {
		case "draft", "stable", "deprecated":
			c.Status = status
		default:
			c.Status = status
			c.StatusUnknown = true
			*diags = append(*diags, Diagnostic{
				Path: rel, Severity: SeverityWarning,
				Message: fmt.Sprintf("status %q is not draft, stable, or deprecated; excluded from current knowledge", status),
			})
		}
	}
	if superseded, ok := meta["superseded_by"]; ok && scalarString(superseded) != "" {
		c.SupersededBy = scalarString(superseded)
		if c.Status != "deprecated" {
			*diags = append(*diags, Diagnostic{
				Path: rel, Severity: SeverityWarning,
				Message: "superseded_by is set without status deprecated",
			})
		}
	}
}

func applyTrust(c *Concept, meta map[string]any, rel string, diags *[]Diagnostic) {
	if raw, ok := meta["generated"]; ok && !isEmpty(raw) {
		event, ok := actorEvent(raw)
		if !ok || event.By == "" {
			*diags = append(*diags, Diagnostic{
				Path: rel, Severity: SeverityWarning, Message: "generated is missing by; ignored",
			})
		} else {
			c.Generated = &event
		}
	}
	if raw, ok := meta["verified"]; ok && !isEmpty(raw) {
		for _, item := range asList(raw) {
			event, ok := actorEvent(item)
			if !ok || event.By == "" || !event.At.Valid {
				*diags = append(*diags, Diagnostic{
					Path: rel, Severity: SeverityWarning, Message: "verified event missing by or at; ignored",
				})
				continue
			}
			c.Verified = append(c.Verified, event)
		}
	}
	c.TrustTier = trustTier(c.Generated, c.Verified)
}

func trustTier(generated *ActorEvent, verified []ActorEvent) string {
	var generatedAt time.Time
	if generated != nil && generated.At.Valid {
		generatedAt = generated.At.Time
	}
	human := false
	machine := false
	for _, event := range verified {
		if !event.At.Valid {
			continue
		}
		if generated != nil && generated.At.Valid && event.At.Time.Before(generatedAt) {
			continue
		}
		if strings.HasPrefix(event.By, "human:") {
			human = true
		} else {
			machine = true
		}
	}
	switch {
	case human:
		return "human-reviewed"
	case machine:
		return "machine-confirmed"
	default:
		return "unverified"
	}
}

func applySources(c *Concept, meta map[string]any, rel string, diags *[]Diagnostic) {
	raw, ok := meta["sources"]
	if !ok || isEmpty(raw) {
		return
	}
	for _, item := range asList(raw) {
		entry, ok := item.(map[string]any)
		if !ok {
			*diags = append(*diags, Diagnostic{Path: rel, Severity: SeverityWarning, Message: "source entry is not a mapping; ignored"})
			continue
		}
		resource := scalarString(entry["resource"])
		src := Source{
			ID:       scalarString(entry["id"]),
			Resource: resource,
			Title:    scalarString(entry["title"]),
			Author:   scalarString(entry["author"]),
		}
		if resource == "" {
			src.Ignored = true
			*diags = append(*diags, Diagnostic{Path: rel, Severity: SeverityWarning, Message: "source entry missing resource; ignored"})
		}
		if n, ok := asInt(entry["usage_count"]); ok {
			src.UsageCount = &n
		}
		if lm, ok := entry["last_modified"]; ok && !isEmpty(lm) {
			src.LastModified = parseTimestamp(lm)
			if !src.LastModified.Valid {
				*diags = append(*diags, Diagnostic{Path: rel, Severity: SeverityWarning, Message: "source last_modified is not an RFC3339 instant or a date"})
			}
		}
		c.Sources = append(c.Sources, src)
	}
}

func applyStale(c *Concept, meta map[string]any, rel string, now time.Time, diags *[]Diagnostic) {
	raw, ok := meta["stale_after"]
	if !ok || isEmpty(raw) {
		return
	}
	ts := parseTimestamp(raw)
	if !ts.Valid {
		c.StaleMalformed = true
		c.Stale = true
		*diags = append(*diags, Diagnostic{
			Path: rel, Severity: SeverityWarning,
			Message: "stale_after is malformed and is not treated as fresh",
		})
		return
	}
	if !now.Before(ts.Time) {
		c.Stale = true
	}
}

func applyAttestation(c *Concept, meta map[string]any, rel string, diags *[]Diagnostic) {
	attester, _ := meta["attester"].(map[string]any)
	if attester != nil {
		c.AttesterPath = scalarString(attester["resource"])
	}
	if c.Runtime == "" || c.AttesterPath == "" {
		c.Attestable = false
		*diags = append(*diags, Diagnostic{
			Path: rel, Severity: SeverityWarning,
			Message: "attested computation has no runtime or attester; value must not be shown as attested",
		})
		return
	}
	c.Attestable = true
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
	fm := rest[:end]
	body := rest[end+endLen:]
	return fm, body, true
}

func parseFrontmatter(fm string) (map[string]any, []Diagnostic) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &node); err != nil {
		return nil, []Diagnostic{{Severity: SeverityError, Message: "frontmatter is not YAML: " + err.Error()}}
	}
	if len(node.Content) == 0 {
		return map[string]any{}, nil
	}
	root := node.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, []Diagnostic{{Severity: SeverityError, Message: "frontmatter is not a mapping"}}
	}
	if dups := duplicateKeys(root, ""); len(dups) > 0 {
		out := make([]Diagnostic, 0, len(dups))
		for _, key := range dups {
			out = append(out, Diagnostic{Severity: SeverityError, Message: "duplicate frontmatter key " + key})
		}
		return nil, out
	}
	if tags := customTags(root); len(tags) > 0 {
		out := make([]Diagnostic, 0, len(tags))
		for _, tag := range tags {
			out = append(out, Diagnostic{Severity: SeverityError, Message: "custom YAML tag " + tag})
		}
		return nil, out
	}
	var meta map[string]any
	if err := root.Decode(&meta); err != nil {
		return nil, []Diagnostic{{Severity: SeverityError, Message: "frontmatter decode: " + err.Error()}}
	}
	if meta == nil {
		meta = map[string]any{}
	}
	return meta, nil
}

func duplicateKeys(node *yaml.Node, prefix string) []string {
	if node.Kind != yaml.MappingNode {
		var found []string
		for _, child := range node.Content {
			found = append(found, duplicateKeys(child, prefix)...)
		}
		return found
	}
	seen := map[string]struct{}{}
	var found []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		full := key
		if prefix != "" {
			full = prefix + "." + key
		}
		if _, ok := seen[key]; ok {
			found = append(found, full)
		}
		seen[key] = struct{}{}
		found = append(found, duplicateKeys(node.Content[i+1], full)...)
	}
	return found
}

func customTags(node *yaml.Node) []string {
	var found []string
	if node.Tag != "" && !strings.HasPrefix(node.Tag, "!!") && node.Tag != "!" {
		found = append(found, node.Tag)
	}
	for _, child := range node.Content {
		found = append(found, customTags(child)...)
	}
	return found
}

var knownKeys = map[string]struct{}{
	"type": {}, "title": {}, "description": {}, "resource": {}, "tags": {},
	"sources": {}, "usage_window": {}, "generated": {}, "verified": {},
	"status": {}, "superseded_by": {}, "stale_after": {},
	"runtime": {}, "parameters": {}, "computation": {}, "executor": {}, "attester": {},
	"okf_version": {}, "license": {},
}

func extensions(meta map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range meta {
		if _, known := knownKeys[key]; known {
			continue
		}
		out[key] = value
	}
	return out
}

func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

func stringList(v any) []string {
	if isEmpty(v) {
		return nil
	}
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, item := range t {
			if s := scalarString(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		if s := scalarString(v); s != "" {
			return []string{s}
		}
		return nil
	}
}

func asList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case nil:
		return nil
	default:
		return []any{t}
	}
}

func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case string:
		n, err := strconv.Atoi(t)
		return n, err == nil
	default:
		return 0, false
	}
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	default:
		return false
	}
}

func actorEvent(v any) (ActorEvent, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return ActorEvent{}, false
	}
	event := ActorEvent{By: scalarString(m["by"])}
	if raw, ok := m["at"]; ok && !isEmpty(raw) {
		event.At = parseTimestamp(raw)
	}
	return event, true
}

func parseTimestamp(v any) Timestamp {
	switch t := v.(type) {
	case time.Time:
		return Timestamp{Time: t.UTC(), Raw: t.UTC().Format(time.RFC3339), Valid: true}
	case string:
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			return Timestamp{Time: ts.UTC(), Raw: t, Valid: true}
		}
		if d, err := time.Parse("2006-01-02", t); err == nil {
			midnight := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
			return Timestamp{Time: midnight, Raw: t, Valid: true}
		}
		return Timestamp{Raw: t}
	default:
		return Timestamp{Raw: scalarString(v)}
	}
}

var headingRE = regexp.MustCompile(`(?m)^(#{1,6})\s+(.+?)\s*$`)
var inlineLinkRE = regexp.MustCompile(`\[[^\]\n]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

func headings(body string) []string {
	var out []string
	for _, match := range headingRE.FindAllStringSubmatch(body, -1) {
		out = append(out, strings.TrimSpace(match[2]))
	}
	return out
}

func links(root, fromFile, body string) []Link {
	var out []Link
	for _, match := range inlineLinkRE.FindAllStringSubmatch(body, -1) {
		raw := match[1]
		target, kind := resolvePath(root, fromFile, raw)
		out = append(out, Link{Raw: raw, Target: target, Kind: kind})
	}
	return out
}

func resolvePath(root, fromFile, ref string) (string, string) {
	ref = strings.TrimSpace(ref)
	if hash := strings.Index(ref, "#"); hash >= 0 {
		ref = ref[:hash]
	}
	if ref == "" {
		return "", "fragment"
	}
	if strings.Contains(ref, "://") {
		return ref, "url"
	}
	var joined string
	if strings.HasPrefix(ref, "/") {
		joined = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(ref, "/")))
	} else {
		joined = filepath.Join(filepath.Dir(fromFile), filepath.FromSlash(ref))
	}
	cleaned := filepath.Clean(joined)
	rel, err := filepath.Rel(root, cleaned)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return cleaned, "external"
	}
	return filepath.ToSlash(rel), "bundle"
}

func isEscapingSymlink(root, path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return false, nil
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return true, nil
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return true, nil
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}
