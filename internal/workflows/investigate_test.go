package workflows

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/mock"
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
