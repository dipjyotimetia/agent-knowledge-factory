// Package workflows turns a gap into a pull request and publishes only after
// a verified merge commit. The sync runner and the Temporal workflow call the
// same steps.
package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/gaps"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publication"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/traces"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SignalReview is the durable signal sent by a verified webhook.
const SignalReview = "review"

// ReviewSignal is what a human did. Merged is not trusted until DefaultHead matches.
// HeadChanged asks the workflow to revalidate the current pull-request head.
type ReviewSignal struct {
	Merged      bool
	Rejected    bool
	HeadChanged bool
	MergeSHA    string
	HeadSHA     string
	Reason      string
}

// Steps are the side effects of curation.
type Steps struct {
	Workspace     string
	KnowledgeRoot string
	Gaps          *gaps.Service
	DB            interface {
		WithWorkspace(ctx context.Context, workspaceID string, fn func(context.Context, pgx.Tx) error) error
	}
	Index            *index.Service
	Publication      *publication.Service
	Publisher        publish.Publisher
	Evidence         []curator.Evidence
	Rules            []curator.Rule
	Advisor          curator.Modeler
	MaxModelCalls    int
	MaxSourceFetches int
	MaxTokens        int
	CasesPath        string
	FetchEvidence    EvidenceLoader
	attemptID        string
	loadCorpus       func(context.Context) (string, map[string]string, error)
	attemptFeedback  func(context.Context) ([]string, error)
}

// Prepared is a validated proposal that has not been published.
type Prepared struct {
	GapID    string
	Decision string
	State    string
	Proposal curator.Proposal
	Files    map[string]string
	PropID   string
	BaseSHA  string
	Branch   string
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
	if s.MaxSourceFetches > 0 && len(s.Evidence) > s.MaxSourceFetches {
		return Prepared{}, fmt.Errorf("source fetch budget exceeded")
	}
	base, files, err := s.proposalCorpus(ctx)
	if err != nil {
		return Prepared{}, err
	}
	var feedback []string
	if s.attemptFeedback != nil {
		feedback, err = s.attemptFeedback(ctx)
		if err != nil {
			return Prepared{}, err
		}
	} else if s.Gaps != nil {
		feedback, err = s.Gaps.UnreadFeedback(ctx, s.Workspace)
		if err != nil {
			return Prepared{}, err
		}
	}
	if s.attemptID == "" && gap.Report.TraceRef != nil && *gap.Report.TraceRef != "" {
		if raw, err := os.ReadFile(*gap.Report.TraceRef); err == nil { // #nosec G304 -- trace_ref is an operator-supplied path, not request-controlled retrieval
			if doc, err := traces.ImportJSON(raw); err == nil {
				feedback = append(feedback, traces.HumanAsks(doc)...)
			}
		}
	}
	var suggested *curator.Edit
	if gap.Report.Suggested != nil {
		suggested = &curator.Edit{Path: gap.Report.Suggested.Path, Old: gap.Report.Suggested.Old, New: gap.Report.Suggested.New}
	}
	prop, err := curator.Propose(ctx, curator.Request{
		GapID:         gapID,
		Category:      gap.Report.Category,
		Summary:       gap.Report.Summary,
		Observed:      gap.Report.Observed,
		Expected:      gap.Report.Expected,
		Suggested:     suggested,
		Feedback:      feedback,
		Files:         files,
		Evidence:      s.Evidence,
		Rules:         s.Rules,
		Advisor:       s.Advisor,
		MaxModelCalls: s.MaxModelCalls,
		MaxTokens:     s.MaxTokens,
	})
	if err != nil {
		return Prepared{}, err
	}
	prepared := Prepared{GapID: gapID, Decision: prop.Decision, Proposal: prop, Files: files, BaseSHA: base}
	if prop.Decision != "update" && prop.Decision != "create-skill" {
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
	prepared.PropID = s.attemptID
	if prepared.PropID == "" {
		prepared.PropID = proposalID(s.Workspace, gapID)
	}
	if s.attemptID == "" {
		if err := s.persistEvidence(ctx, gap, prepared.Proposal); err != nil {
			return Prepared{}, err
		}
	}
	if err := s.upsertProposal(ctx, prepared); err != nil {
		return Prepared{}, err
	}
	if s.Gaps != nil && s.attemptID == "" {
		if err := s.Gaps.ConsumeFeedback(ctx, s.Workspace); err != nil {
			return Prepared{}, err
		}
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
	if !actionable(prepared.Decision) {
		return Opened{Prepared: prepared}, nil
	}
	if prepared.PropID == "" {
		return Opened{}, fmt.Errorf("refusing to open a pull request without a persisted proposal")
	}
	if existing, ok, err := s.loadOpened(ctx, prepared); err != nil {
		return Opened{}, err
	} else if ok {
		return existing, nil
	}
	files, diff, err := proposalEdits(prepared)
	if err != nil {
		return Opened{}, err
	}

	var treeDigest string
	var gateErr error
	if s.Publication != nil {
		full := maps.Clone(prepared.Files)
		maps.Copy(full, files)
		if err := s.Publication.PinProposal(ctx, prepared.PropID, prepared.BaseSHA); err != nil {
			return Opened{}, err
		}
		receipt, err := s.Publication.EvaluateCandidate(ctx, prepared.PropID, prepared.BaseSHA, full)
		treeDigest, gateErr = receipt.TreeDigest, err
	} else {
		gateErr = s.vetProposal(ctx, prepared, files)
	}
	if err := gateErr; err != nil {
		if errors.Is(err, ErrCriticalRegression) {
			_, _ = s.Gaps.Transition(ctx, s.Workspace, prepared.GapID, domain.StateFailed, "evaluator", err.Error())
		}
		return Opened{}, err
	}
	branch := prepared.Branch
	if branch == "" {
		branch = "curator/" + prepared.GapID
	}
	result, err := s.Publisher.CreatePullRequest(ctx, publish.Request{
		ExpectedBase: prepared.BaseSHA, TreeDigest: treeDigest,
		Title:  "Knowledge: " + prepared.Proposal.Claims[0].Claim,
		Body:   prBody(prepared.Proposal, diff),
		Branch: branch,
		Files:  files,
	})
	if err != nil {
		return Opened{}, err
	}
	if s.Publication != nil {
		err = s.Publication.RecordMapping(ctx, prepared.PropID, prepared.BaseSHA, result)
	} else {
		err = s.recordPullRequest(ctx, prepared.PropID, result)
	}
	if err != nil {
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
	if s.Publication != nil {
		_, err := s.Publication.RequestRelease(ctx, publication.Request{Trigger: "pr-merge", GapID: opened.GapID, PRNumber: opened.Number})
		return Published{State: current.State}, err
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
	gap, err := s.Gaps.Status(ctx, s.Workspace, opened.GapID)
	if err != nil {
		return Published{}, err
	}
	var targeted []string
	if s.CasesPath != "" {
		cases, err := evaluations.Load(s.CasesPath)
		if err != nil {
			return Published{}, err
		}
		targeted = evaluations.TargetedIDs(cases, gap.Report.Summary, gap.Report.Observed, gap.Report.Expected)
	}
	if err := s.noteHumanDelta(ctx, opened, merged, signal.MergeSHA); err != nil {
		return Published{}, err
	}
	before, after, err := s.commitCandidate(ctx, signal.MergeSHA, root, opened.PropID)
	if err != nil {
		if errors.Is(err, ErrCriticalRegression) {
			_, _ = s.Gaps.Transition(ctx, s.Workspace, opened.GapID, domain.StateFailed, "evaluator", err.Error())
		}
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
	if s.CasesPath != "" && evaluations.ProvesImprovement(before, after, targeted) && domain.CanTransition(next.State, domain.StateVerified) {
		verified, err := s.Gaps.Transition(ctx, s.Workspace, opened.GapID, domain.StateVerified, "evaluator", "targeted case improved")
		if err != nil {
			return Published{}, err
		}
		next = verified
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
		revisions, err := json.Marshal(prepared.Proposal.SourceRevisions)
		if err != nil {
			return err
		}
		conflicts, err := json.Marshal(prepared.Proposal.Conflicts)
		if err != nil {
			return err
		}
		if prepared.Proposal.SourceRevisions == nil {
			revisions = []byte("[]")
		}
		if prepared.Proposal.Conflicts == nil {
			conflicts = []byte("[]")
		}
		diff := editDiff(prepared.Proposal)
		paths := prepared.Proposal.TargetPaths
		if paths == nil {
			paths = []string{}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO proposals (
				id, workspace_id, gap_ids, source_revisions, target_paths, diff, edits,
				claim_evidence_map, conflicts, risk_level, approval_status, decision, model_name,
				input_tokens, output_tokens
			) VALUES (
				$1, $2, ARRAY[$3]::uuid[], $4, $5, $6, $7, $8, $9, $10, 'draft', $11, $12, $13, $14
			)
			ON CONFLICT (id) DO UPDATE SET
				edits = EXCLUDED.edits,
				claim_evidence_map = EXCLUDED.claim_evidence_map,
				target_paths = EXCLUDED.target_paths,
				source_revisions = EXCLUDED.source_revisions,
				diff = EXCLUDED.diff,
				conflicts = EXCLUDED.conflicts,
				decision = EXCLUDED.decision,
				model_name = EXCLUDED.model_name,
				input_tokens = EXCLUDED.input_tokens,
				output_tokens = EXCLUDED.output_tokens,
				updated_at = now()
		`, prepared.PropID, s.Workspace, prepared.GapID, revisions, paths, diff, edits, claims, conflicts,
			prepared.Proposal.Risk, prepared.Decision, prepared.Proposal.Model, prepared.Proposal.InputTokens, prepared.Proposal.OutputTokens)
		return err
	})
}

func (s *Steps) loadPrepared(ctx context.Context, gap domain.Gap) (Prepared, error) {
	base, files, err := s.proposalCorpus(ctx)
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
			WHERE $1 = ANY(gap_ids) AND ($2='' OR id::text=$2)
			ORDER BY created_at DESC
			LIMIT 1
		`, gap.ID, s.attemptID).Scan(&propID, &prop.Decision, &edits, &claims)
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
	return Prepared{GapID: gap.ID, Decision: prop.Decision, State: gap.State, Proposal: prop, Files: files, PropID: propID, BaseSHA: base}, nil
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

func actionable(decision string) bool {
	return decision == "update" || decision == "create-skill"
}

func editDiff(prop curator.Proposal) string {
	var b strings.Builder
	for _, edit := range prop.Edits {
		fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n-%s\n+%s\n", edit.Path, edit.Path, edit.Old, edit.New)
	}
	return b.String()
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
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	var rels []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel, err = safeRel(rel)
		if err != nil {
			return err
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, rel := range rels {
		body, err := readRoot(base, rel)
		if err != nil {
			return nil, err
		}
		out[rel] = string(body)
	}
	return out, nil
}

func materialize(root string, overlays map[string]string) (string, error) {
	files, err := readTree(root)
	if err != nil {
		return "", err
	}
	for rel, body := range overlays {
		rel, err = safeRel(rel)
		if err != nil {
			return "", err
		}
		files[rel] = body
	}
	dest, err := os.MkdirTemp("", "akf-publish-")
	if err != nil {
		return "", err
	}
	tree, err := os.OpenRoot(dest)
	if err != nil {
		return "", err
	}
	defer tree.Close()
	for rel, body := range files {
		if dir := path.Dir(rel); dir != "." {
			if err := tree.MkdirAll(dir, 0o750); err != nil {
				return "", err
			}
		}
		if err := tree.WriteFile(rel, []byte(body), 0o600); err != nil {
			return "", err
		}
	}
	return dest, nil
}

func readRoot(base *os.Root, rel string) ([]byte, error) {
	file, err := base.Open(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, 8<<20))
}

func safeRel(rel string) (string, error) {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("refusing path %q", rel)
	}
	return rel, nil
}

func (s *Steps) proposalCorpus(ctx context.Context) (string, map[string]string, error) {
	if s.loadCorpus != nil {
		return s.loadCorpus(ctx)
	}
	if s.Publication != nil {
		base, files, err := s.Publication.ProposalBase(ctx)
		return base.SHA, files, err
	}
	files, err := readTree(s.KnowledgeRoot)
	return "", files, err
}

func proposalEdits(prepared Prepared) (map[string]string, string, error) {
	files := map[string]string{}
	var diff strings.Builder
	for _, edit := range prepared.Proposal.Edits {
		body, ok := prepared.Files[edit.Path]
		if previous, exists := files[edit.Path]; exists {
			body, ok = previous, true
		}
		if edit.Old == "" && !ok {
			files[edit.Path] = edit.New
			fmt.Fprintf(&diff, "--- /dev/null\n+++ b/%s\n+%s\n", edit.Path, edit.New)
			continue
		}
		if !ok {
			return nil, "", fmt.Errorf("missing %s", edit.Path)
		}
		files[edit.Path] = strings.Replace(body, edit.Old, edit.New, 1)
		fmt.Fprintf(&diff, "--- a/%s\n+++ b/%s\n-%s\n+%s\n", edit.Path, edit.Path, edit.Old, edit.New)
	}
	return files, diff.String(), nil
}
