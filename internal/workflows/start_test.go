package workflows

import (
	"errors"
	"testing"

	"go.temporal.io/api/serviceerror"
)

func TestWorkflowIDIsStable(t *testing.T) {
	if WorkflowID("banking-demo", "gap") != "curate-banking-demo-gap" {
		t.Fatal(WorkflowID("banking-demo", "gap"))
	}
}

func TestAlreadyStartedIsIdempotent(t *testing.T) {
	if err := ignoreAlreadyStarted(&serviceerror.WorkflowExecutionAlreadyStarted{}); err != nil {
		t.Fatal(err)
	}
	if err := ignoreAlreadyStarted(errors.New("down")); err == nil {
		t.Fatal("unexpected nil")
	}
}
