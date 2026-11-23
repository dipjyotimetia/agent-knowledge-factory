// Package traces imports sanitized agent traces. It accepts a small JSON
// transcript and the OpenTelemetry GenAI attributes used on a span.
// Secret-shaped values and obvious PII are redacted before anything else reads them.
package traces

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/policy"
)

// Turn is one message after redaction.
type Turn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Transcript is a redacted conversation.
type Transcript struct {
	Turns []Turn `json:"turns"`
}

// ImportJSON reads either {"turns":[...]} or a list of OTel-style attribute maps
// with gen_ai.prompt.* and gen_ai.completion.* keys.
func ImportJSON(raw []byte) (Transcript, error) {
	var wrapped Transcript
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Turns) > 0 {
		return redact(wrapped), nil
	}
	var attrs []map[string]string
	if err := json.Unmarshal(raw, &attrs); err != nil {
		return Transcript{}, err
	}
	var out Transcript
	for _, attr := range attrs {
		for key, value := range attr {
			switch {
			case strings.HasPrefix(key, "gen_ai.prompt"):
				out.Turns = append(out.Turns, Turn{Role: "user", Text: value})
			case strings.HasPrefix(key, "gen_ai.completion"):
				out.Turns = append(out.Turns, Turn{Role: "assistant", Text: value})
			case key == "gen_ai.human":
				out.Turns = append(out.Turns, Turn{Role: "human", Text: value})
			}
		}
	}
	return redact(out), nil
}

// HumanAsks returns redacted human or user turns. The curator can mine these.
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

func redact(doc Transcript) Transcript {
	for i := range doc.Turns {
		doc.Turns[i].Text = scrub(doc.Turns[i].Text)
	}
	return doc
}

var (
	email = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	phone = regexp.MustCompile(`\b\d{3}[-. ]\d{3}[-. ]\d{4}\b`)
)

func scrub(text string) string {
	text = policy.Redact(text)
	text = email.ReplaceAllString(text, "[redacted-email]")
	text = phone.ReplaceAllString(text, "[redacted-phone]")
	return text
}
