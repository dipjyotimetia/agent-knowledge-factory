// Package workflows turns a gap into a pull request and publishes only after
// a verified merge commit. The sync runner and the Temporal workflow call the
// same steps.
package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SignalReview is the durable signal sent by a verified webhook.
const SignalReview = "review"

// ReviewSignal is what a human did. Merged is not trusted until DefaultHead matches.
type ReviewSignal struct {
	Merged   bool
	Rejected bool
	MergeSHA string
	Reason   string
}

// Steps are the side effects of curation.
type Steps struct {
	Workspace     string
	KnowledgeRoot string
	Gaps          *gaps.Service
	DB            interface {
		WithWorkspace(ctx context.Context, workspaceID string, fn func(context.Context, pgx.Tx) error) error
	}
	Index     *index.Service
	Publisher publish.Publisher
	Evidence  []curator.Evidence
	Rules     []curator.Rule
}

// Prepared is a validated proposal that has not been published.
type Prepared struct {
	GapID    string
	Decision string
	State    string
	Proposal curator.Proposal
	Files    map[string]string
	PropID   string
}

// Opened is a pull request waiting for a human.
type Opened struct {
	Prepared
	Number  int
	URL     string
	HeadSHA string
	Branch  string
}

// Published is the result of the merge check.
type Published struct {
	Published bool
	Revision  string
	State     string
}

// Prepare gathers evidence, proposes, and moves the gap. It does not open a PR
// unless the decision is update; the caller does that in Open.
func (s *Steps) Prepare(ctx context.Context, gapID string) (Prepared, error) {
	gap, err := s.Gaps.Status(ctx, s.Workspace, gapID)
	if err != nil {
		return Prepared{}, err
	}
	if gap.CanonicalID != "" {
		gapID = gap.CanonicalID
		gap, err = s.Gaps.Status(ctx, s.Workspace, gapID)
		if err != nil {
			return Prepared{}, err
		}
	}
	if domain.CanTransition(gap.State, domain.StateTriaged) {
		if _, err := s.Gaps.Transition(ctx, s.Workspace, gapID, domain.StateTriaged, "curator", "normalize"); err != nil {
			return Prepared{}, err
		}
		gap.State = domain.StateTriaged
	}
	if domain.CanTransition(gap.State, domain.StateInvestigating) {
		if _, err := s.Gaps.Transition(ctx, s.Workspace, gapID, domain.StateInvestigating, "curator", "collect evidence"); err != nil {
			return Prepared{}, err
		}
	}
	switch gap.State {
	case domain.StateInsufficientEvidence, domain.StateRejected, domain.StateCancelled, domain.StateFailed:
		return Prepared{GapID: gap.ID, Decision: "insufficient-evidence", State: gap.State}, nil
	case domain.StateProposalReady, domain.StateAwaitingReview, domain.StateAccepted, domain.StatePublished, domain.StateVerified:
		return s.loadPrepared(ctx, gap)
	}
	files, err := readTree(s.KnowledgeRoot)
	if err != nil {
		return Prepared{}, err
	}
	prop, err := curator.Propose(ctx, curator.Request{
		GapID:    gapID,
		Category: gap.Report.Category,
		Summary:  gap.Report.Summary,
		Observed: gap.Report.Observed,
		Expected: gap.Report.Expected,
		Files:    files,
		Evidence: s.Evidence,
		Rules:    s.Rules,
	})
	if err != nil {
		return Prepared{}, err
	}
	prepared := Prepared{GapID: gapID, Decision: prop.Decision, Proposal: prop, Files: files}
	if prop.Decision != "update" {
		reason := prop.Decision
		if len(prop.Conflicts) > 0 {
			reason = strings.Join(prop.Conflicts, "; ")
		}
		next, err := s.Gaps.Transition(ctx, s.Workspace, gapID, domain.StateInsufficientEvidence, "curator", reason)
		if err != nil {
			return Prepared{}, err
		}
		prepared.State = next.State
		return prepared, nil
	}
	prepared.PropID = proposalID(s.Workspace, gapID)
	if err := s.upsertProposal(ctx, prepared); err != nil {
		return Prepared{}, err
	}
	current, err := s.Gaps.Status(ctx, s.Workspace, gapID)
	if err != nil {
		return Prepared{}, err
	}
	if domain.CanTransition(current.State, domain.StateProposalReady) {
		if _, err := s.Gaps.Transition(ctx, s.Workspace, gapID, domain.StateProposalReady, "curator", "evidence grounded"); err != nil {
			return Prepared{}, err
		}
		current.State = domain.StateProposalReady
	}
	prepared.State = current.State
	return prepared, nil
}

// Open creates the pull request and waits for review. It does not merge.
func (s *Steps) Open(ctx context.Context, prepared Prepared) (Opened, error) {
	if prepared.Decision != "update" {
		return Opened{Prepared: prepared}, nil
	}
	if existing, ok, err := s.loadOpened(ctx, prepared); err != nil {
		return Opened{}, err
	} else if ok {
		return existing, nil
	}
	files := map[string]string{}
	var diff strings.Builder
	for _, edit := range prepared.Proposal.Edits {
		body, ok := prepared.Files[edit.Path]
		if !ok {
			return Opened{}, fmt.Errorf("missing %s", edit.Path)
		}
		files[edit.Path] = strings.Replace(body, edit.Old, edit.New, 1)
		fmt.Fprintf(&diff, "--- a/%s\n+++ b/%s\n-%s\n+%s\n", edit.Path, edit.Path, edit.Old, edit.New)
	}
	branch := "curator/" + prepared.GapID
	result, err := s.Publisher.CreatePullRequest(ctx, publish.Request{
		Title:  "Knowledge: " + prepared.Proposal.Claims[0].Claim,
		Body:   prBody(prepared.Proposal, diff.String()),
		Branch: branch,
		Files:  files,
	})
	if err != nil {
		return Opened{}, err
	}
	if err := s.recordPullRequest(ctx, prepared.PropID, result); err != nil {
		return Opened{}, err
	}
	current, err := s.Gaps.Status(ctx, s.Workspace, prepared.GapID)
	if err != nil {
		return Opened{}, err
	}
	if domain.CanTransition(current.State, domain.StateAwaitingReview) {
		if _, err := s.Gaps.Transition(ctx, s.Workspace, prepared.GapID, domain.StateAwaitingReview, "curator", result.URL); err != nil {
			return Opened{}, err
		}
	}
	opened := Opened{Prepared: prepared, Number: result.Number, URL: result.URL, HeadSHA: result.HeadSHA, Branch: branch}
	opened.Files = files
	return opened, nil
}

// Publish reindexes only when the default branch head is the claimed merge SHA.
func (s *Steps) Publish(ctx context.Context, opened Opened, signal ReviewSignal) (Published, error) {
	current, err := s.Gaps.Status(ctx, s.Workspace, opened.GapID)
	if err != nil {
		return Published{}, err
	}
	if current.State == domain.StatePublished || current.State == domain.StateVerified {
		return Published{Published: true, Revision: signal.MergeSHA, State: current.State}, nil
	}
	if signal.Rejected || !signal.Merged {
		reason := signal.Reason
		if reason == "" {
			reason = "review rejected"
		}
		next, err := s.Gaps.Transition(ctx, s.Workspace, opened.GapID, domain.StateRejected, "reviewer", reason)
		if err != nil {
			return Published{}, err
		}
		return Published{State: next.State}, nil
	}
	head, err := s.Publisher.DefaultHead(ctx)
	if err != nil {
		return Published{}, err
	}
	if head == "" || head != signal.MergeSHA {
		return Published{}, publish.ErrNotMerged
	}
	merged, err := s.Publisher.Tree(ctx, signal.MergeSHA)
	if err != nil {
		return Published{}, err
	}
	root, err := materialize(s.KnowledgeRoot, merged)
	if err != nil {
		return Published{}, err
	}
	defer os.RemoveAll(root)
	if err := s.Index.Reindex(ctx, s.Workspace, signal.MergeSHA, root); err != nil {
		return Published{}, err
	}
	current, err = s.Gaps.Status(ctx, s.Workspace, opened.GapID)
	if err != nil {
		return Published{}, err
	}
	if domain.CanTransition(current.State, domain.StateAccepted) {
		if _, err := s.Gaps.Transition(ctx, s.Workspace, opened.GapID, domain.StateAccepted, "reviewer", "merge commit verified"); err != nil {
			return Published{}, err
		}
		current.State = domain.StateAccepted
	}
	if !domain.CanTransition(current.State, domain.StatePublished) {
		return Published{Published: true, Revision: signal.MergeSHA, State: current.State}, nil
	}
	next, err := s.Gaps.Transition(ctx, s.Workspace, opened.GapID, domain.StatePublished, "publisher", signal.MergeSHA)
	if err != nil {
		return Published{}, err
	}
	return Published{Published: true, Revision: signal.MergeSHA, State: next.State}, nil
}

func (s *Steps) upsertProposal(ctx context.Context, prepared Prepared) error {
	if s.DB == nil {
		return nil
	}
	edits, err := json.Marshal(prepared.Proposal.Edits)
	if err != nil {
		return err
	}
	claims, err := json.Marshal(prepared.Proposal.Claims)
	if err != nil {
		return err
	}
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO proposals (
				id, workspace_id, gap_ids, source_revisions, target_paths, diff, edits,
				claim_evidence_map, conflicts, risk_level, approval_status, decision, model_name
			) VALUES (
				$1, $2, ARRAY[$3]::uuid[], '[]', $4, '', $5, $6, '[]', $7, 'draft', $8, $9
			)
			ON CONFLICT (id) DO UPDATE SET
				edits = EXCLUDED.edits,
				claim_evidence_map = EXCLUDED.claim_evidence_map,
				target_paths = EXCLUDED.target_paths,
				decision = EXCLUDED.decision
		`, prepared.PropID, s.Workspace, prepared.GapID, prepared.Proposal.TargetPaths, edits, claims,
			prepared.Proposal.Risk, prepared.Decision, prepared.Proposal.Model)
		return err
	})
}

func (s *Steps) loadPrepared(ctx context.Context, gap domain.Gap) (Prepared, error) {
	files, err := readTree(s.KnowledgeRoot)
	if err != nil {
		return Prepared{}, err
	}
	if s.DB == nil {
		return Prepared{}, fmt.Errorf("proposal for %s is not stored", gap.ID)
	}
	var prop curator.Proposal
	var propID string
	err = s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		var edits, claims []byte
		scanErr := tx.QueryRow(ctx, `
			SELECT id, decision, edits, claim_evidence_map
			FROM proposals
			WHERE $1 = ANY(gap_ids)
			ORDER BY created_at DESC
			LIMIT 1
		`, gap.ID).Scan(&propID, &prop.Decision, &edits, &claims)
		if scanErr != nil {
			return scanErr
		}
		if err := json.Unmarshal(edits, &prop.Edits); err != nil {
			return err
		}
		return json.Unmarshal(claims, &prop.Claims)
	})
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{GapID: gap.ID, Decision: prop.Decision, State: gap.State, Proposal: prop, Files: files, PropID: propID}, nil
}

func (s *Steps) loadOpened(ctx context.Context, prepared Prepared) (Opened, bool, error) {
	if s.DB == nil || prepared.PropID == "" {
		return Opened{}, false, nil
	}
	var url, branch, head string
	var number int
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT coalesce(git_pr_url, ''), coalesce(git_branch, ''), coalesce(head_sha, ''), coalesce(git_pr_number, 0)
			FROM proposals WHERE id = $1
		`, prepared.PropID).Scan(&url, &branch, &head, &number)
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && url == "") {
		return Opened{}, false, nil
	}
	if err != nil {
		return Opened{}, false, err
	}
	return Opened{Prepared: prepared, Number: number, URL: url, HeadSHA: head, Branch: branch}, true, nil
}

func (s *Steps) recordPullRequest(ctx context.Context, proposalID string, result publish.Result) error {
	if s.DB == nil || proposalID == "" {
		return nil
	}
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE proposals
			SET git_pr_url = $2, git_branch = $3, head_sha = $4, git_pr_number = $5, approval_status = 'open', updated_at = now()
			WHERE id = $1
		`, proposalID, result.URL, result.Branch, result.HeadSHA, result.Number)
		return err
	})
}

func proposalID(workspace, gapID string) string {
	return uuid.NewSHA1(uuid.MustParse("6ba7b811-9dad-11d1-80b4-00c04fd430c8"), []byte(workspace+"/"+gapID)).String()
}

func prBody(prop curator.Proposal, diff string) string {
	var b strings.Builder
	b.WriteString("Curator proposal. A human must merge this. The curator cannot.\n\n")
	for _, claim := range prop.Claims {
		fmt.Fprintf(&b, "- %s (evidence: %s)\n", claim.Claim, strings.Join(claim.EvidenceIDs, ", "))
	}
	b.WriteString("\n```\n")
	b.WriteString(diff)
	b.WriteString("```\n")
	return b.String()
}

func readTree(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	return out, err
}

func materialize(root string, overlays map[string]string) (string, error) {
	dest, err := os.MkdirTemp("", "akf-publish-")
	if err != nil {
		return "", err
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
	if err != nil {
		return "", err
	}
	for rel, body := range overlays {
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
			return "", err
		}
	}
	return dest, nil
}
