package workflows_test

import (
	"testing"
	"time"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
)

func TestWorkflowDoesNotPublishWithoutTheSignal(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	var steps *workflows.Steps
	env.OnActivity(steps.Prepare, mock.Anything, "gap-1").Return(workflows.Prepared{
		GapID: "gap-1", Decision: "insufficient-evidence", State: "insufficient-evidence",
	}, nil)
	env.ExecuteWorkflow(workflows.TriageGapWorkflow, workflows.Input{GapID: "gap-1"})
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result workflows.Result
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Published || result.Decision != "insufficient-evidence" {
		t.Fatalf("result = %+v", result)
	}
}

func TestWorkflowPublishesOnlyAfterMergeSignal(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	var steps *workflows.Steps
	env.OnActivity(steps.Prepare, mock.Anything, "gap-1").Return(workflows.Prepared{
		GapID: "gap-1", Decision: "update", State: "proposal-ready", PropID: "proposal-1",
	}, nil)
	env.OnActivity(steps.Open, mock.Anything, mock.Anything).Return(workflows.Opened{
		Prepared: workflows.Prepared{GapID: "gap-1", Decision: "update"},
		Number:   4, URL: "https://example.invalid/pull/4", HeadSHA: "head", Branch: "curator/gap-1",
	}, nil)
	env.OnActivity(steps.Publish, mock.Anything, mock.Anything, mock.Anything).Return(workflows.Published{
		Published: true, Revision: "merge-sha", State: "published",
	}, nil)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflows.SignalReview, workflows.ReviewSignal{Merged: true, MergeSHA: "merge-sha"})
	}, time.Second)
	env.ExecuteWorkflow(workflows.TriageGapWorkflow, workflows.Input{GapID: "gap-1"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result workflows.Result
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Published || result.Revision != "merge-sha" || result.PRURL == "" {
		t.Fatalf("result = %+v", result)
	}
}
