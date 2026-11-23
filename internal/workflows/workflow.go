package workflows

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Input starts curation for one gap.
type Input struct {
	GapID string
}

// Result is the durable outcome. Published is true only after the merge check.
type Result struct {
	Decision  string
	State     string
	Published bool
	Revision  string
	PRURL     string
}

// TriageGapWorkflow proposes a change and waits for a human review signal.
// Model calls and GitHub calls stay in activities.
func TriageGapWorkflow(ctx workflow.Context, in Input) (Result, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumAttempts: 3},
	})
	_ = workflow.GetVersion(ctx, "review-wait", workflow.DefaultVersion, 1)
	var steps *Steps
	var prepared Prepared
	if err := workflow.ExecuteActivity(ctx, steps.Prepare, in.GapID).Get(ctx, &prepared); err != nil {
		return Result{}, err
	}
	if prepared.Decision != "update" && prepared.Decision != "create-skill" {
		return Result{Decision: prepared.Decision, State: prepared.State}, nil
	}
	var opened Opened
	if err := workflow.ExecuteActivity(ctx, steps.Open, prepared).Get(ctx, &opened); err != nil {
		return Result{}, err
	}
	var signal ReviewSignal
	for {
		workflow.GetSignalChannel(ctx, SignalReview).Receive(ctx, &signal)
		if signal.HeadChanged && !signal.Merged && !signal.Rejected {
			if err := workflow.ExecuteActivity(ctx, steps.Revalidate, opened).Get(ctx, nil); err != nil {
				return Result{}, err
			}
			continue
		}
		break
	}
	var published Published
	if err := workflow.ExecuteActivity(ctx, steps.Publish, opened, signal).Get(ctx, &published); err != nil {
		return Result{}, err
	}
	return Result{
		Decision:  prepared.Decision,
		State:     published.State,
		Published: published.Published,
		Revision:  published.Revision,
		PRURL:     opened.URL,
	}, nil
}
