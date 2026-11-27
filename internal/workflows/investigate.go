package workflows

import (
	"errors"
	"time"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/investigation"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	SignalResume        = "resume-investigation"
	SignalAttemptReview = "investigation-review"
)

type InvestigationRun struct {
	Input           investigation.Input
	ReviewOnly      bool
	RejectRequested bool
}

type ReviewState struct {
	State    string
	ReviewAt time.Time
	Reminded bool
}

func activityOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 3 * time.Minute, HeartbeatTimeout: 35 * time.Second, WaitForCancellation: true,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 15 * time.Second, MaximumAttempts: 4},
	})
}

// InvestigateGapWorkflowV2 keeps proposal, evidence and tree bytes in activities.
func InvestigateGapWorkflowV2(ctx workflow.Context, run InvestigationRun) error {
	ctx = activityOptions(ctx)
	var activities *InvestigationActivities
	defer func() {
		if errors.Is(ctx.Err(), workflow.ErrCanceled) {
			cleanup, cancel := workflow.NewDisconnectedContext(ctx)
			defer cancel()
			if err := workflow.ExecuteActivity(cleanup, activities.CancelInvestigation, run.Input).Get(cleanup, nil); err != nil {
				workflow.GetLogger(ctx).Error("attempt cancellation was not persisted", "error", err)
			}
		}
	}()
	if !run.ReviewOnly {
		var actionable bool
		if err := workflow.ExecuteActivity(ctx, activities.PrepareInvestigation, run.Input).Get(ctx, &actionable); err != nil {
			return failInvestigation(ctx, run.Input, err)
		}
		if !actionable {
			return nil
		}
		if err := workflow.ExecuteActivity(ctx, activities.OpenInvestigation, run.Input).Get(ctx, nil); err != nil {
			return failInvestigation(ctx, run.Input, err)
		}
	}
	for checks := 0; ctx.Err() == nil; checks++ {
		if checks >= 48 || workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
			run.ReviewOnly = true
			signal := workflow.GetSignalChannel(ctx, SignalAttemptReview)
			var reject bool
			for signal.ReceiveAsync(&reject) {
				run.RejectRequested = run.RejectRequested || reject
			}
			return workflow.NewContinueAsNewError(ctx, InvestigateGapWorkflowV2, run)
		}
		var review ReviewState
		if err := workflow.ExecuteActivity(ctx, activities.CheckInvestigationReview, run.Input, run.RejectRequested).Get(ctx, &review); err == nil {
			run.RejectRequested = false
			if review.State == "completed" || review.State == "cancelled" || review.State == "failed" {
				return nil
			}
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
		waiter, cancel := workflow.WithCancel(ctx)
		timer := workflow.NewTimer(waiter, time.Hour)
		selector := workflow.NewSelector(ctx)
		selector.AddFuture(timer, func(workflow.Future) {})
		selector.AddReceive(workflow.GetSignalChannel(ctx, SignalResume), func(c workflow.ReceiveChannel, _ bool) {
			var wake bool
			c.Receive(ctx, &wake)
		})
		selector.AddReceive(workflow.GetSignalChannel(ctx, SignalAttemptReview), func(c workflow.ReceiveChannel, _ bool) {
			var reject bool
			c.Receive(ctx, &reject)
			run.RejectRequested = run.RejectRequested || reject
		})
		selector.Select(ctx)
		cancel()
	}
	return ctx.Err()
}

func failInvestigation(ctx workflow.Context, in investigation.Input, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var activities *InvestigationActivities
	if err := workflow.ExecuteActivity(ctx, activities.FailInvestigation, in).Get(ctx, nil); err != nil {
		return err
	}
	return cause
}

type DrainInvestigationsInput struct {
	WorkspaceID string
	After       string
}

// DrainInvestigationsWorkflowV2 pages canonical gaps into the same queue as intake.
func DrainInvestigationsWorkflowV2(ctx workflow.Context, in DrainInvestigationsInput) error {
	ctx = activityOptions(ctx)
	var activities *InvestigationActivities
	for range 10 {
		var next string
		if err := workflow.ExecuteActivity(ctx, activities.DrainInvestigationPage, in).Get(ctx, &next); err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		in.After = next
		if workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
			break
		}
	}
	return workflow.NewContinueAsNewError(ctx, DrainInvestigationsWorkflowV2, in)
}

// RetainWorkspaceWorkflowV2 has its own idempotent activity and schedule.
func RetainWorkspaceWorkflowV2(ctx workflow.Context, workspace string) error {
	ctx = activityOptions(ctx)
	var activities *InvestigationActivities
	return workflow.ExecuteActivity(ctx, activities.RetainWorkspace, workspace).Get(ctx, nil)
}
