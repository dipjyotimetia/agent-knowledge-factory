// Package policy holds input gates that do not depend on a database.
package policy

import "regexp"

// Finding is a secret or policy match. The value itself is not retained.
type Finding struct {
	Rule  string
	Field string
}

var secretRules = []struct {
	name string
	re   *regexp.Regexp
}{
	{"aws-access-key", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"aws-secret", regexp.MustCompile(`(?i)aws_secret_access_key\s*[:=]\s*\S+`)},
	{"github-token", regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`)},
	{"github-pat", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`)},
	{"private-key", regexp.MustCompile(`-----BEGIN (?:RSA |OPENSSH |EC )?PRIVATE KEY-----`)},
	{"connection-string", regexp.MustCompile(`(?i)postgres(?:ql)?://\S+:\S+@`)},
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"assigned-secret", regexp.MustCompile(`(?i)(?:api[_-]?key|secret|password|token)\s*[:=]\s*\S{8,}`)},
}

// ScanSecrets returns one finding per rule that matches. Callers must not
// write the input back into an audit record.
func ScanSecrets(field, text string) []Finding {
	var out []Finding
	for _, rule := range secretRules {
		if rule.re.MatchString(text) {
			out = append(out, Finding{Rule: rule.name, Field: field})
		}
	}
	return out
}
