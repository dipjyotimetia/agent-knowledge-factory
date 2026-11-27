// Package traces imports sanitized agent traces.
package traces

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/policy"
)

const MaxBytes = 1 << 20
const maxTurns = 200

var ErrInvalid = errors.New("invalid trace document")

type Turn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type Transcript struct {
	Turns []Turn `json:"turns"`
}

// ImportJSON reads one transcript or an array of flat GenAI attribute maps.
// Walk tokens in source order and reject duplicate keys before interpreting
// their values. The accepted grammar has fixed depth.
func ImportJSON(raw []byte) (Transcript, error) {
	if len(raw) > MaxBytes || !utf8.Valid(raw) {
		return Transcript{}, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var out Transcript
	switch next(d) {
	case json.Delim('{'):
		if next(d) != "turns" || next(d) != json.Delim('[') {
			return Transcript{}, ErrInvalid
		}
		for d.More() {
			fields := map[string]string{}
			if len(out.Turns) >= maxTurns || !readObject(d, func(key, value string) bool {
				switch key {
				case "role", "text":
					fields[key] = value
					return true
				default:
					return sensitiveKey(key)
				}
			}) {
				return Transcript{}, ErrInvalid
			}
			text, ok := fields["text"]
			if !ok || !validRole(fields["role"]) {
				return Transcript{}, ErrInvalid
			}
			out.Turns = append(out.Turns, Turn{Role: fields["role"], Text: scrub(text)})
		}
		if next(d) != json.Delim(']') || next(d) != json.Delim('}') {
			return Transcript{}, ErrInvalid
		}
	case json.Delim('['):
		for objects := 0; d.More(); objects++ {
			if objects >= maxTurns || !readObject(d, func(key, value string) bool {
				role := attributeRole(key)
				if role == "" {
					return sensitiveKey(key) || (scrub(key) == key && scrub(value) == value)
				}
				if len(out.Turns) >= maxTurns {
					return false
				}
				out.Turns = append(out.Turns, Turn{Role: role, Text: scrub(value)})
				return true
			}) {
				return Transcript{}, ErrInvalid
			}
		}
		if next(d) != json.Delim(']') {
			return Transcript{}, ErrInvalid
		}
	default:
		return Transcript{}, ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF || len(out.Turns) == 0 {
		return Transcript{}, ErrInvalid
	}
	return out, nil
}

func next(d *json.Decoder) json.Token {
	token, _ := d.Token()
	return token
}

func readObject(d *json.Decoder, field func(string, string) bool) bool {
	if next(d) != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		key, keyOK := next(d).(string)
		value, valueOK := next(d).(string)
		if !keyOK || !valueOK || len(key) > 200 || seen[key] || len(seen) >= maxTurns ||
			strings.ContainsAny(key+value, "\x00\ufffd") || !field(key, value) {
			return false
		}
		seen[key] = true
	}
	return next(d) == json.Delim('}')
}

func validRole(role string) bool {
	switch role {
	case "human", "user", "assistant", "system", "tool", "developer":
		return true
	default:
		return false
	}
}

var attribute = regexp.MustCompile(`^gen_ai\.(prompt|completion)(\.[0-9]{1,3}(\.content)?)?$`)

func attributeRole(key string) string {
	if key == "gen_ai.human" {
		return "human"
	}
	if !attribute.MatchString(key) {
		return ""
	}
	if strings.HasPrefix(key, "gen_ai.prompt") {
		return "user"
	}
	return "assistant"
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	switch key {
	case "authorization", "proxy_authorization", "cookie", "set_cookie", "api_key", "x_api_key",
		"password", "token", "secret", "email", "phone", "address", "name", "headers":
		return true
	}
	return strings.HasPrefix(key, "http.request.header.") || strings.HasPrefix(key, "http.response.header.")
}

// HumanAsks returns the sanitized human and user turns.
func HumanAsks(doc Transcript) []string {
	var out []string
	for _, turn := range doc.Turns {
		if turn.Role == "human" || turn.Role == "user" {
			if text := strings.TrimSpace(turn.Text); text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

var (
	email       = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	phone       = regexp.MustCompile(`\b\d{3}[-. ]\d{3}[-. ]\d{4}\b`)
	credentials = regexp.MustCompile(`(?i)\b(?:authorization|proxy-authorization|cookie|set-cookie|password|token|secret|(?:x-)?api[-_ ]?key)["']?\s*[:=]\s*\S|\bbearer\s+[a-z0-9._~+/-]{8,}`)
)

func scrub(text string) string {
	if credentials.MatchString(text) {
		return "[redacted]"
	}
	text = policy.Redact(text)
	text = email.ReplaceAllString(text, "[redacted-email]")
	return phone.ReplaceAllString(text, "[redacted-phone]")
}
