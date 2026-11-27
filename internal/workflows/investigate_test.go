package workflows

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func Test23DrainContinuesAfterBoundedPages(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	activities := &InvestigationActivities{}
	calls := 0
	env.OnActivity(activities.DrainInvestigationPage, mock.Anything, mock.Anything).
		Return(func(_ context.Context, in DrainInvestigationsInput) (string, error) {
			if in.WorkspaceID != "fixture" {
				t.Errorf("workspace=%q", in.WorkspaceID)
			}
			calls++
			return fmt.Sprintf("page-%d", calls), nil
		})
	env.ExecuteWorkflow(DrainInvestigationsWorkflowV2, DrainInvestigationsInput{WorkspaceID: "fixture"})
	if err := env.GetWorkflowError(); !workflow.IsContinueAsNewError(err) {
		t.Fatalf("expected continue as new, got %v", err)
	}
	if calls != 10 {
		t.Fatalf("drained %d pages in one history", calls)
	}
}

func TestContinueAsNewCarriesBufferedRejection(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	activities := &InvestigationActivities{}
	env.OnActivity(activities.CheckInvestigationReview, mock.Anything, mock.Anything, false).
		Return(ReviewState{State: "review"}, nil).After(time.Minute).Once()
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalAttemptReview, false)
		env.SignalWorkflow(SignalAttemptReview, true)
		env.SetContinueAsNewSuggested(true)
	}, time.Second)
	env.ExecuteWorkflow(InvestigateGapWorkflowV2, InvestigationRun{ReviewOnly: true})
	var continued *workflow.ContinueAsNewError
	if err := env.GetWorkflowError(); !errors.As(err, &continued) {
		t.Fatalf("expected continue as new: %v", err)
	}
	var next InvestigationRun
	if err := converter.GetDefaultDataConverter().FromPayloads(continued.Input, &next); err != nil {
		t.Fatal(err)
	}
	if !next.RejectRequested {
		t.Fatal("buffered rejection lost on continuation")
	}
}
