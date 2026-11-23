// Package evaluations scores knowledge against golden cases. Golden answers are
// not passed to the curator.
package evaluations

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
)

// Case is one golden task. Answer and Sentinel are not curator inputs.
type Case struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Critical       bool   `json:"critical"`
	ExpectsChange  bool   `json:"expects_change"`
	RetrievalQuery string `json:"retrieval_query"`
	Extract        string `json:"extract"`
	Answer         string `json:"answer"`
	Decision       string `json:"decision"`
	Sentinel       string `json:"sentinel"`
}

// Hit is the text the evaluator is allowed to read from retrieval.
type Hit struct {
	URI  string
	Body string
}

// SearchFunc returns the best published documents for a query.
type SearchFunc func(ctx context.Context, query string) ([]Hit, error)

// Report is one run of the dataset.
type Report struct {
	Passed              int
	Failed              int
	CriticalRegressions int
	Results             []Result
}

// Result is one case.
type Result struct {
	ID       string `json:"id"`
	Critical bool   `json:"critical"`
	Passed   bool   `json:"passed"`
	Detail   string `json:"detail"`
}

// Load reads the case file.
func Load(path string) ([]Case, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path is the operator case file, not request input
	if err != nil {
		return nil, err
	}
	var cases []Case
	if err := json.Unmarshal(raw, &cases); err != nil {
		return nil, err
	}
	if len(cases) < 20 {
		return nil, fmt.Errorf("evaluation set has %d cases, want at least 20", len(cases))
	}
	return cases, nil
}

// RunRetrieval scores documentation-gap cases. Decision cases are skipped.
func RunRetrieval(ctx context.Context, cases []Case, search SearchFunc) (Report, error) {
	var report Report
	for _, c := range cases {
		if c.Kind != "documentation-gap" {
			continue
		}
		hits, err := search(ctx, c.RetrievalQuery)
		if err != nil {
			return Report{}, err
		}
		got := ""
		if len(hits) > 0 {
			got = extract(c.Extract, hits[0].Body)
		}
		passed := got == c.Answer || (c.Extract == "contains" && strings.Contains(got, c.Answer))
		if c.Extract == "contains" {
			passed = len(hits) > 0 && strings.Contains(hits[0].Body, c.Answer)
		}
		result := Result{ID: c.ID, Critical: c.Critical, Passed: passed, Detail: got}
		if !passed && c.Critical {
			report.CriticalRegressions++
		}
		if passed {
			report.Passed++
		} else {
			report.Failed++
		}
		report.Results = append(report.Results, result)
	}
	return report, nil
}

// RunDecisions scores contradiction and negative cases with the deterministic curator.
// The golden decision is compared after the call. It is not placed in the request.
func RunDecisions(ctx context.Context, cases []Case) (Report, error) {
	var report Report
	for _, c := range cases {
		if c.Kind == "documentation-gap" {
			continue
		}
		req := decisionRequest(c)
		prop, err := curator.Propose(ctx, req)
		if err != nil {
			return Report{}, err
		}
		passed := prop.Decision == c.Decision
		if passed {
			report.Passed++
		} else {
			report.Failed++
			if c.Critical {
				report.CriticalRegressions++
			}
		}
		report.Results = append(report.Results, Result{ID: c.ID, Critical: c.Critical, Passed: passed, Detail: prop.Decision})
	}
	return report, nil
}

// CuratorPrompt is the text a model would see. It must not contain golden answers.
func CuratorPrompt(c Case, evidence []curator.Evidence) string {
	var b strings.Builder
	b.WriteString("Task query: " + c.RetrievalQuery + "\n")
	b.WriteString("Category: " + c.Kind + "\n")
	for _, ev := range evidence {
		b.WriteString(curator.Untrusted(ev.Content))
	}
	return b.String()
}

func extract(kind, body string) string {
	if kind != "dedupe-key" {
		return body
	}
	match := dedupeKey.FindStringSubmatch(body)
	if match == nil {
		return ""
	}
	return match[1]
}

var dedupeKey = regexp.MustCompile("deduplicate by `([A-Za-z0-9_]+)`")

func decisionRequest(c Case) curator.Request {
	switch c.ID {
	case "execution-failure":
		return curator.Request{Category: "execution-failure", Rules: []curator.Rule{curator.BankingDedupeRule()}, Evidence: contractPair("eventId", "eventId"), Files: knowledgeFiles("movementId")}
	case "already-correct":
		return curator.Request{Category: "stale-instruction", Rules: []curator.Rule{curator.BankingDedupeRule()}, Evidence: contractPair("eventId", "eventId"), Files: knowledgeFiles("eventId")}
	case "secondary-only", "abstain-pii", "unknown-topic", "wiki-vs-schema":
		return curator.Request{Category: "stale-instruction", Rules: []curator.Rule{curator.BankingDedupeRule()}, Evidence: []curator.Evidence{{
			ID: "wiki", Location: "github://example/wiki.md", Revision: strings.Repeat("a", 40), Digest: "sha256:wiki",
			Classification: "secondary", Content: "deduplicate by `movementId`",
		}}, Files: knowledgeFiles("movementId")}
	default:
		return curator.Request{Category: "contradictory-sources", Rules: []curator.Rule{curator.BankingDedupeRule()}, Evidence: contractPair("eventId", "movementId"), Files: knowledgeFiles("movementId")}
	}
}

func contractPair(a, b string) []curator.Evidence {
	return []curator.Evidence{
		{ID: "readme-a", Location: "github://acme/contracts/avro/README.md", Revision: strings.Repeat("a", 40), Digest: "sha256:a", Classification: "contract", Content: "deduplicate by `" + a + "`"},
		{ID: "readme-b", Location: "github://acme/contracts/avro/README.md", Revision: strings.Repeat("b", 40), Digest: "sha256:b", Classification: "contract", Content: "deduplicate by `" + b + "`"},
		{ID: "schema", Location: "github://acme/contracts/avro/movement-lifecycle-event.avsc", Revision: strings.Repeat("c", 40), Digest: "sha256:c", Classification: "contract", Content: `"name": "` + a + `"`},
	}
}

func knowledgeFiles(key string) map[string]string {
	return map[string]string{"skills/movement-event-dedupe/SKILL.md": "External consumers must deduplicate by `" + key + "`."}
}
