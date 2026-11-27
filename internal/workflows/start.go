package workflows

import (
	"context"
	"errors"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/investigation"

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

// AttemptStarter enqueues and dispatches durable investigation attempts.
type AttemptStarter struct {
	Client client.Client
	Queue  string
	Store  investigation.Store
}

func AttemptQueue(configured, workspace string) string {
	if configured == "" {
		configured = "knowledge-curator"
	}
	return configured + "-v2-" + workspace
}

func (t AttemptStarter) StartInvestigation(ctx context.Context, gapID string) error {
	_, err := t.Store.Enqueue(ctx, gapID)
	if errors.Is(err, investigation.ErrTerminal) {
		return nil
	}
	return err
}

// Dispatch is safe across replicas because Temporal accepts each attempt ID once.
func (t AttemptStarter) Dispatch(ctx context.Context) error {
	inputs, err := t.Store.Starts(ctx)
	if err != nil {
		return err
	}
	for _, in := range inputs {
		attempt, err := t.Store.Get(ctx, in)
		if err != nil {
			return err
		}
		_, err = t.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID: investigation.WorkflowID(in), TaskQueue: t.Queue,
			WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		}, InvestigateGapWorkflowV2, InvestigationRun{Input: in, ReviewOnly: attempt.State == "review" || attempt.State == "parked"})
		if err = ignoreAlreadyStarted(err); err != nil {
			return err
		}
		if err = t.Store.Started(ctx, in); err != nil {
			return err
		}
	}
	return nil
}
