package workflows

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/investigation"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publication"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

type InvestigationActivities struct {
	Store       investigation.Store
	Steps       Steps
	Starter     AttemptStarter
	Reminder    time.Duration
	ParkAfter   time.Duration
	PauseIntake bool
}

func (a *InvestigationActivities) PrepareInvestigation(ctx context.Context, in investigation.Input) (bool, error) {
	var actionableResult bool
	err := a.executeStep(ctx, in, "prepare", func(ctx context.Context, lease investigation.Lease) error {
		if lease.Done {
			attempt, err := a.Store.Get(ctx, in)
			actionableResult = attempt.ProposalID != ""
			return err
		}
		load := a.Steps.FetchEvidence
		if load == nil {
			load = func(context.Context) ([]curator.Evidence, error) { return a.Steps.Evidence, nil }
		}
		evidence, err := a.Store.Evidence(ctx, in, load)
		if err != nil {
			return err
		}
		steps := a.attemptSteps(in)
		steps.Evidence = evidence
		if steps.Advisor != nil {
			steps.Advisor = reservedModel{store: a.Store, in: in, next: steps.Advisor, limit: steps.MaxModelCalls}
		}
		prepared, err := steps.Prepare(ctx, in.GapID)
		if err != nil {
			return err
		}
		actionableResult = actionable(prepared.Decision)
		state, reason := "running", "prepared"
		if !actionableResult {
			state, reason = "completed", "no-proposal"
		}
		return a.Store.Finish(ctx, lease, state, reason, prepared.PropID)
	})
	return actionableResult, err
}

func (a *InvestigationActivities) OpenInvestigation(ctx context.Context, in investigation.Input) error {
	return a.executeStep(ctx, in, "open", func(ctx context.Context, lease investigation.Lease) error {
		if lease.Done {
			return nil
		}
		steps := a.attemptSteps(in)
		gap, err := steps.Gaps.Status(ctx, in.WorkspaceID, in.GapID)
		if err != nil {
			return err
		}
		prepared, err := steps.loadPrepared(ctx, gap)
		if err != nil {
			return err
		}
		if prepared.PropID != in.AttemptID || len(prepared.Proposal.Claims) == 0 {
			return investigation.ErrMismatch
		}
		if err := a.ensurePull(ctx, steps, prepared, in); err != nil {
			if errors.Is(err, publish.ErrPRBaseAdvanced) {
				return a.Store.RequeueStaleBase(ctx, lease)
			}
			return err
		}
		return a.Store.Finish(ctx, lease, "review", "awaiting-review", prepared.PropID)
	})
}

func (a *InvestigationActivities) ensurePull(ctx context.Context, steps Steps, prepared Prepared, in investigation.Input) error {
	edits, diff, err := proposalEdits(prepared)
	if err != nil {
		return err
	}
	full := maps.Clone(prepared.Files)
	maps.Copy(full, edits)
	req := publish.Request{ExpectedBase: prepared.BaseSHA, TreeDigest: publish.TreeDigest(full), Branch: "curator/" + in.AttemptID,
		Files: edits, Title: "Knowledge: " + prepared.Proposal.Claims[0].Claim, Body: prBody(prepared.Proposal, diff)}
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	digest := publication.Digest(raw)
	prior, err := a.Store.PriorPR(ctx, in, digest, req.Branch)
	if err != nil {
		return err
	}
	if !prior {
		if err = steps.Publication.PinProposal(ctx, prepared.PropID, prepared.BaseSHA); err != nil {
			return err
		}
		receipt, err := steps.Publication.EvaluateCandidate(ctx, prepared.PropID, prepared.BaseSHA, full)
		if errors.Is(err, publication.ErrStale) {
			return publish.ErrPRBaseAdvanced
		}
		if err != nil {
			return err
		}
		if err = a.Store.ReservePR(ctx, in, digest, req.Branch, receipt.CandidateID); err != nil {
			return err
		}
	}
	result, err := steps.Publisher.CreatePullRequest(ctx, req)
	if err != nil {
		return err
	}
	if err = steps.Publication.RecordMapping(ctx, prepared.PropID, prepared.BaseSHA, result); err != nil {
		return err
	}
	if err = a.Store.CompletePR(ctx, in); err != nil {
		return err
	}
	current, err := steps.Gaps.Status(ctx, in.WorkspaceID, in.GapID)
	if err != nil {
		return err
	}
	if domain.CanTransition(current.State, domain.StateAwaitingReview) {
		_, err = steps.Gaps.Transition(ctx, in.WorkspaceID, in.GapID, domain.StateAwaitingReview, "curator", "PR awaiting review")
	}
	return err
}

func (a *InvestigationActivities) attemptSteps(in investigation.Input) Steps {
	steps := a.Steps
	steps.attemptID = in.AttemptID
	steps.loadCorpus = func(ctx context.Context) (string, map[string]string, error) {
		sha, err := a.Store.BaseSHA(ctx, in)
		if err != nil {
			return "", nil, err
		}
		files, err := steps.Publication.Forge.Tree(ctx, sha)
		return sha, files, err
	}
	steps.attemptFeedback = func(ctx context.Context) ([]string, error) { return a.Store.Feedback(ctx, in) }
	return steps
}

func (a *InvestigationActivities) executeStep(ctx context.Context, in investigation.Input, kind string, run func(context.Context, investigation.Lease) error) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return safeActivityError(err)
	}
	lease, err := a.acquireLease(ctx, in, kind, publication.Digest(raw))
	if err != nil {
		return safeActivityError(err)
	}
	if lease.Done {
		return safeActivityError(run(ctx, lease))
	}
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				if err := a.Store.Renew(heartbeatCtx, lease); err != nil {
					cancel()
					return
				}
				activity.RecordHeartbeat(heartbeatCtx, in.AttemptID)
			}
		}
	}()
	safe := safeActivityError(run(a.Store.Fence(heartbeatCtx, lease), lease))
	var appErr *temporal.ApplicationError
	if ctx.Err() == nil && errors.As(safe, &appErr) {
		_ = a.Store.Release(ctx, lease, appErr.Type(), appErr.NonRetryable())
		if !appErr.NonRetryable() && activity.IsActivity(ctx) {
			delay := investigationRetryDelay(activity.GetInfo(ctx).Attempt)
			return temporal.NewApplicationErrorWithOptions(appErr.Message(), appErr.Type(),
				temporal.ApplicationErrorOptions{NextRetryDelay: delay})
		}
	}
	return safe
}

func (a *InvestigationActivities) acquireLease(ctx context.Context, in investigation.Input, kind, digest string) (investigation.Lease, error) {
	for {
		lease, err := a.Store.Acquire(ctx, in, kind, digest)
		if !errors.Is(err, investigation.ErrBusy) {
			return lease, err
		}
		if activity.IsActivity(ctx) {
			activity.RecordHeartbeat(ctx, in.AttemptID)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return investigation.Lease{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func investigationRetryDelay(attempt int32) time.Duration {
	delay := time.Second * time.Duration(1<<min(3, max(0, attempt-1)))
	var bytes [2]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		delay += time.Duration(binary.LittleEndian.Uint16(bytes[:])%500) * time.Millisecond
	}
	return delay
}

type reservedModel struct {
	store investigation.Store
	in    investigation.Input
	next  curator.Modeler
	limit int
}

func (m reservedModel) Propose(ctx context.Context, req curator.Request) (curator.Proposal, error) {
	raw, err := json.Marshal(struct {
		Summary, Observed, Expected string
		Evidence                    []curator.Evidence
		Feedback                    []string
	}{
		req.Summary, req.Observed, req.Expected, req.Evidence, req.Feedback})
	if err != nil {
		return curator.Proposal{}, err
	}
	digest := publication.Digest(raw)
	if result, ok, err := m.store.ModelResult(ctx, m.in, digest); ok || err != nil {
		return result, err
	}
	if err = m.store.ReserveModel(ctx, m.in, digest, m.limit); err != nil {
		return curator.Proposal{}, err
	}
	prop, err := m.next.Propose(ctx, req)
	if err != nil {
		return prop, err
	}
	prop.SourceRevisions = nil
	for _, ev := range req.Evidence {
		if ev.Classification == "contract" || ev.Classification == "policy" {
			prop.SourceRevisions = append(prop.SourceRevisions, curator.Source{Location: ev.Location, Revision: ev.Revision, Digest: ev.Digest})
		}
	}
	if err := curator.Validate(req, prop); err != nil {
		return curator.Proposal{}, publication.ErrRejected
	}
	return prop, m.store.CompleteModel(ctx, m.in, digest, prop)
}

func safeActivityError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	switch {
	case errors.Is(err, investigation.ErrMismatch), errors.Is(err, publication.ErrIdentity):
		return temporal.NewNonRetryableApplicationError("investigation identity rejected", "identity", nil)
	case errors.Is(err, investigation.ErrTerminal), errors.Is(err, investigation.ErrBudget),
		errors.Is(err, publication.ErrRejected), errors.Is(err, publish.ErrStaleBase), errors.Is(err, publish.ErrUnsafeTree), errors.Is(err, publish.ErrPRMismatch):
		return temporal.NewNonRetryableApplicationError("investigation validation rejected", "validation", nil)
	default:
		var netErr net.Error
		if errors.As(err, &netErr) || errors.Is(err, investigation.ErrStale) || errors.Is(err, investigation.ErrBusy) {
			return temporal.NewApplicationError("retryable investigation operation", "retryable")
		}
		var status *publish.HTTPError
		if errors.As(err, &status) && status.Status >= 400 && status.Status < 500 &&
			status.Status != http.StatusTooManyRequests && status.Status != http.StatusRequestTimeout && status.Status != http.StatusConflict {
			return temporal.NewNonRetryableApplicationError("forge request rejected", "forge-rejected", nil)
		}
		if strings.Contains(err.Error(), "status 403") || strings.HasSuffix(err.Error(), ": 403") {
			return temporal.NewNonRetryableApplicationError("forge or model permission denied", "permission", nil)
		}
		return temporal.NewApplicationError("investigation operation incomplete", "retryable")
	}
}

func (a *InvestigationActivities) CancelInvestigation(ctx context.Context, in investigation.Input) error {
	return a.endInvestigation(ctx, in, "cancelled")
}

func (a *InvestigationActivities) FailInvestigation(ctx context.Context, in investigation.Input) error {
	return a.endInvestigation(ctx, in, "failed")
}

func (a *InvestigationActivities) endInvestigation(ctx context.Context, in investigation.Input, state string) error {
	return safeActivityError(a.Store.End(ctx, in, state))
}

func (a *InvestigationActivities) CheckInvestigationReview(ctx context.Context, in investigation.Input, reject bool) (ReviewState, error) {
	review, err := a.Store.ReviewClock(ctx, in)
	out := ReviewState{State: review.State, ReviewAt: review.At, Reminded: review.Reminded}
	if err != nil {
		return out, safeActivityError(err)
	}
	if out.State != "review" && out.State != "parked" {
		return out, nil
	}
	reminder, park := a.Reminder, a.ParkAfter
	if reminder <= 0 {
		reminder = 7 * 24 * time.Hour
	}
	if park <= 0 {
		park = 30 * 24 * time.Hour
	}
	elapsed := time.Since(out.ReviewAt)
	if out.State == "review" && elapsed >= park {
		err = a.Store.Transition(ctx, in, out.State, "parked", "review-expired")
		if err != nil {
			return out, safeActivityError(err)
		}
		out.State = "parked"
	}
	status, err := a.Steps.Publication.ReviewPR(ctx, review.ProposalID)
	if err != nil {
		return out, safeActivityError(err)
	}
	if status.MergeSHA != "" {
		_, err = a.Steps.Publication.RequestRelease(ctx, publication.Request{Trigger: "pr-merge", PRNumber: status.Number})
		if err != nil {
			return out, safeActivityError(err)
		}
		err = a.Store.Transition(ctx, in, out.State, "completed", "merge-observed")
		out.State = "completed"
		return out, safeActivityError(err)
	}
	if status.State == "closed" || reject {
		current, err := a.Steps.Gaps.Status(ctx, in.WorkspaceID, in.GapID)
		if err != nil {
			return out, safeActivityError(err)
		}
		if domain.CanTransition(current.State, domain.StateRejected) {
			if _, err = a.Steps.Gaps.Transition(ctx, in.WorkspaceID, in.GapID, domain.StateRejected, "reviewer", "review declined"); err != nil {
				return out, safeActivityError(err)
			}
		}
		err = a.Store.Transition(ctx, in, out.State, "completed", "review-declined")
		out.State = "completed"
		return out, safeActivityError(err)
	}
	if out.State == "parked" {
		return out, nil
	}

	if elapsed >= reminder && !out.Reminded {
		return out, safeActivityError(a.Store.Remind(ctx, in))
	}
	return out, nil
}

func (a *InvestigationActivities) DrainInvestigationPage(ctx context.Context, in DrainInvestigationsInput) (string, error) {
	if in.WorkspaceID != a.Store.Workspace {
		return "", safeActivityError(investigation.ErrMismatch)
	}
	if a.PauseIntake {
		return "", nil
	}
	if in.After == "" {
		_, files, err := a.Steps.Publication.ProposalBase(ctx)
		if err != nil && !errors.Is(err, publication.ErrHeld) {
			return "", safeActivityError(err)
		}
		if err == nil {
			if err := a.Steps.raiseContradictions(ctx, files); err != nil {
				return "", safeActivityError(err)
			}
		}
	}
	ids, err := a.Store.Page(ctx, in.After)
	if err != nil {
		return "", safeActivityError(err)
	}
	for _, id := range ids {
		if _, err = a.Store.Enqueue(ctx, id); err != nil && !errors.Is(err, investigation.ErrTerminal) {
			return "", safeActivityError(err)
		}
		activity.RecordHeartbeat(ctx, id)
	}
	if err = a.Starter.Dispatch(ctx); err != nil {
		return "", safeActivityError(err)
	}
	if len(ids) < 50 {
		return "", nil
	}
	return ids[len(ids)-1], nil
}

func (a *InvestigationActivities) RetainWorkspace(ctx context.Context, workspace string) error {
	if workspace != a.Store.Workspace {
		return safeActivityError(investigation.ErrMismatch)
	}
	if err := a.Store.DB.EnforceRetention(ctx, workspace); err != nil {
		return safeActivityError(fmt.Errorf("retention: %w", err))
	}
	return nil
}
