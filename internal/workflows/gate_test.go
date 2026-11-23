package workflows

import (
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
)

func TestRegressedDetectsACriticalFailure(t *testing.T) {
	before := evaluations.Report{Results: []evaluations.Result{{ID: "keep", Critical: true, Passed: true}, {ID: "already", Critical: true, Passed: false}}}
	after := evaluations.Report{Results: []evaluations.Result{{ID: "keep", Critical: true, Passed: false}, {ID: "already", Critical: true, Passed: true}}}
	if !regressed(before, after) {
		t.Fatal("a passing critical case was allowed to fail")
	}
	after.Results[0].Passed = true
	if regressed(before, after) {
		t.Fatal("an already failing critical case was treated as a regression")
	}
}
