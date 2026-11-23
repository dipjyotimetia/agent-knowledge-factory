package curator

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ClaimPattern is one fact shape the corpus scan compares across documents.
type ClaimPattern struct {
	Name    string
	Pattern string
}

// DefaultClaimPatterns are the corpus checks. They are not limited to one rule id.
func DefaultClaimPatterns() []ClaimPattern {
	return []ClaimPattern{
		{Name: "dedupe-key", Pattern: "deduplicate by `([A-Za-z0-9_]+)`"},
		{Name: "status-value", Pattern: "status is `([A-Za-z0-9_]+)`"},
	}
}

// Conflict is one pattern with more than one value in the corpus.
type Conflict struct {
	Name   string
	Values map[string][]string
}

// ScanClaims walks every document. Two different captures for one pattern are a conflict.
func ScanClaims(files map[string]string, patterns []ClaimPattern) ([]Conflict, error) {
	var out []Conflict
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern.Pattern)
		if err != nil {
			return nil, err
		}
		found := map[string][]string{}
		for path, body := range files {
			for _, match := range re.FindAllStringSubmatch(body, -1) {
				if len(match) < 2 {
					continue
				}
				found[match[1]] = append(found[match[1]], path)
			}
		}
		if len(found) > 1 {
			out = append(out, Conflict{Name: pattern.Name, Values: found})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Summary renders a conflict for a gap report.
func (c Conflict) Summary() string {
	var parts []string
	for value, paths := range c.Values {
		parts = append(parts, fmt.Sprintf("%s in %s", value, strings.Join(paths, ",")))
	}
	sort.Strings(parts)
	return c.Name + ": " + strings.Join(parts, "; ")
}
