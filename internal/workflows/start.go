package workflows

import (
	"context"
	"errors"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// WorkflowID is stable for one investigation so a retry does not start a second run.
func WorkflowID(workspace, gapID string) string {
	return "curate-" + workspace + "-" + gapID
}

// Starter starts TriageGapWorkflow once per investigation.
type Starter interface {
	StartInvestigation(ctx context.Context, gapID string) error
}

// TemporalStarter talks to a Temporal frontend.
type TemporalStarter struct {
	Client    client.Client
	Queue     string
	Workspace string
}

// StartInvestigation is a no-op when that workflow is already running.
func (t TemporalStarter) StartInvestigation(ctx context.Context, gapID string) error {
	if t.Client == nil {
		return errors.New("temporal client is not configured")
	}
	queue := t.Queue
	if queue == "" {
		queue = "knowledge-curator"
	}
	_, err := t.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                    WorkflowID(t.Workspace, gapID),
		TaskQueue:             queue,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	}, TriageGapWorkflow, Input{GapID: gapID})
	return ignoreAlreadyStarted(err)
}

func ignoreAlreadyStarted(err error) error {
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &already) {
		return nil
	}
	return err
}

// DialStarter connects to Temporal. The caller closes the client on process exit.
func DialStarter(cfg TemporalConfig, queue, workspace string) (TemporalStarter, client.Client, error) {
	c, err := Dial(cfg)
	if err != nil {
		return TemporalStarter{}, nil, err
	}
	return TemporalStarter{Client: c, Queue: queue, Workspace: workspace}, c, nil
}
