package publication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/audit"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type mapping struct {
	id, branch, base string
	number           int
	proposal         curator.Proposal
}

func (s *Service) RecordMapping(ctx context.Context, id, base string, result publish.Result) error {
	if result.Number < 1 || result.Branch == "" || result.HeadSHA == "" || base == "" {
		return ErrIdentity
	}
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE proposals SET forge_repository=$3,forge_installation=$4,forge_base_branch=$5,
            forge_base_sha=$6,git_pr_number=$7,git_branch=$8,head_sha=$9,git_pr_url=$10,approval_status='open'
            WHERE workspace_id=$1 AND id=$2 AND (forge_repository IS NULL OR
            (forge_repository=$3 AND forge_installation=$4 AND forge_base_branch=$5 AND forge_base_sha=$6 AND git_pr_number=$7 AND git_branch=$8))`,
			s.Workspace, id, s.Identity.Repository, s.Identity.Installation, s.Identity.Branch, base, result.Number, result.Branch, result.HeadSHA, result.URL)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrIdentity
		}
		return err
	})
}

func (s *Service) getMapping(ctx context.Context, id string) (mapping, error) {
	var item mapping
	var edits, claims, sources []byte
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id::text,git_branch,coalesce(forge_base_sha,''),coalesce(git_pr_number,0),
            decision,edits,claim_evidence_map,source_revisions FROM proposals WHERE workspace_id=$1 AND id=$2`,
			s.Workspace, id).Scan(&item.id, &item.branch, &item.base, &item.number, &item.proposal.Decision, &edits, &claims, &sources)
	})
	if err != nil {
		return item, err
	}
	for _, pair := range []struct {
		raw         []byte
		destination any
	}{{edits, &item.proposal.Edits}, {claims, &item.proposal.Claims}, {sources, &item.proposal.SourceRevisions}} {
		if err := json.Unmarshal(pair.raw, pair.destination); err != nil {
			return item, err
		}
	}
	return item, nil
}

func (s *Service) exactPR(ctx context.Context, item mapping) (publish.PullRequest, error) {
	pr, err := s.Forge.PullRequest(ctx, item.number)
	if err != nil {
		return pr, err
	}
	if pr.Number != item.number || pr.Branch != item.branch || item.base == "" {
		return pr, ErrIdentity
	}
	return pr, nil
}

func grounded(item mapping, original, actual map[string]string, evidence EvidenceSet) bool {
	pinned := make(EvidenceSet, 0, len(evidence))
	for _, ev := range evidence {
		for _, source := range item.proposal.SourceRevisions {
			if ev.Location == source.Location && ev.Revision == source.Revision && ev.Digest == source.Digest &&
				shaPattern.MatchString(ev.Revision) && ev.Digest != "" {
				pinned = append(pinned, ev)
				break
			}
		}
	}
	if len(item.proposal.Edits) == 0 || len(pinned) == 0 || curator.Validate(curator.Request{Files: original, Evidence: pinned}, item.proposal) != nil {
		return false
	}
	expected := maps.Clone(original)
	for _, edit := range item.proposal.Edits {
		if edit.Old == "" {
			expected[edit.Path] = edit.New
		} else {
			expected[edit.Path] = strings.Replace(expected[edit.Path], edit.Old, edit.New, 1)
		}
	}
	return maps.Equal(expected, actual)
}

func (s *Service) pendingMappings(ctx context.Context) ([]string, error) {
	var ids []string
	err := s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text FROM proposals p WHERE p.workspace_id=$1 AND forge_repository=$2
            AND forge_installation=$3 AND forge_base_branch=$4 AND git_pr_number>0 AND approval_status IN ('open','approved','changes-requested')
            AND NOT EXISTS (SELECT 1 FROM proposal_merge_links l WHERE l.workspace_id=p.workspace_id AND l.proposal_id=p.id)
            ORDER BY created_at,id`, s.Workspace, s.Identity.Repository, s.Identity.Installation, s.Identity.Branch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

type mergeMatch struct {
	item mapping
	pr   publish.PullRequest
}

func (s *Service) matches(ctx context.Context, head string, files map[string]string) ([]mergeMatch, error) {
	ids, err := s.pendingMappings(ctx)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	if s.Sources == nil {
		return nil, ErrRejected
	}
	evidence, err := s.Sources(ctx)
	if err != nil {
		return nil, err
	}
	var matches []mergeMatch
	for _, id := range ids {
		item, err := s.getMapping(ctx, id)
		if err != nil {
			return nil, err
		}
		pr, err := s.exactPR(ctx, item)
		if errors.Is(err, publish.ErrPRMismatch) || errors.Is(err, ErrIdentity) {
			if err := s.recordMappingMismatch(ctx, item.id); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if pr.MergeSHA == "" {
			continue
		}
		contained, err := s.Forge.IsAncestor(ctx, pr.MergeSHA, head)
		if err != nil {
			return nil, err
		}
		if !contained {
			continue
		}
		original, err := s.Forge.Tree(ctx, item.base)
		if err != nil {
			return nil, err
		}
		reviewed, err := s.Forge.Tree(ctx, pr.HeadSHA)
		if err != nil {
			return nil, err
		}
		merged, err := s.Forge.Tree(ctx, pr.MergeSHA)
		if err != nil {
			return nil, err
		}
		// Authenticate the proposed patch before checking if its exact change
		// survives both the forge-reported merge and the current default tree.
		if !survivingEdits(item.proposal.Edits, merged) {
			continue
		}
		if !grounded(item, original, reviewed, evidence) {
			if survivingEdits(item.proposal.Edits, files) {
				return nil, fmt.Errorf("%w: merged PR has unvalidated claims", ErrRejected)
			}
			continue
		}
		if survivingEdits(item.proposal.Edits, files) {
			matches = append(matches, mergeMatch{item, pr})
		}
	}
	return matches, nil
}

func survivingEdits(edits []curator.Edit, files map[string]string) bool {
	if len(edits) == 0 {
		return false
	}
	for _, edit := range edits {
		body, exists := files[edit.Path]
		if !exists || edit.New == "" || strings.Count(body, edit.New) != 1 {
			return false
		}
		if edit.Old != "" && !strings.Contains(edit.New, edit.Old) && strings.Contains(body, edit.Old) {
			return false
		}
	}
	return true
}

func (s *Service) checkMergeEvidence(ctx context.Context, base Baseline, head string, files map[string]string, evidence EvidenceSet) error {
	matches, err := s.matches(ctx, head, files)
	if err != nil || base.ReleaseID == "" {
		return err
	}
	original, err := s.Forge.Tree(ctx, base.SHA)
	if err != nil {
		return err
	}
	// Apply known approved patches to the previous tree. Metadata and other
	// claims remaining in a direct push still have to be grounded.
	expected := maps.Clone(original)
	for _, match := range matches {
		for _, edit := range match.item.proposal.Edits {
			if edit.Old == "" {
				if _, exists := expected[edit.Path]; !exists {
					expected[edit.Path] = edit.New
				}
			} else {
				expected[edit.Path] = strings.Replace(expected[edit.Path], edit.Old, edit.New, 1)
			}
		}
	}
	knownText := map[string]bool{}
	for _, body := range original {
		knownText[body] = true
	}
	for name, body := range files {
		if body == expected[name] || knownText[body] || name == "index.md" || name == "log.md" {
			continue
		}
		edit := curator.Edit{Path: name, Old: expected[name], New: body}
		if !groundedChange(expected, edit, evidence) {
			return fmt.Errorf("%w: changed content has no grounded claims", ErrRejected)
		}
	}
	return nil
}

func groundedChange(base map[string]string, edit curator.Edit, evidence EvidenceSet) bool {
	if edit.Old == "" {
		return false
	}
	for _, ev := range evidence {
		if (ev.Classification != "contract" && ev.Classification != "policy") || !shaPattern.MatchString(ev.Revision) || ev.Digest == "" {
			continue
		}
		claims := []curator.Claim{{Claim: "Knowledge update", EvidenceIDs: []string{ev.ID}}}
		if _, err := curator.Manual(curator.Request{Files: base, Evidence: []curator.Evidence{ev}}, []curator.Edit{edit}, claims); err == nil {
			return true
		}
	}
	return false
}

func (s *Service) reconcile(ctx context.Context, release string, generation int64, head string, files map[string]string) error {
	matches, err := s.matches(ctx, head, files)
	if err != nil || len(matches) == 0 {
		return err
	}
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		var active string
		var currentGeneration int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(active_release_id::text,''), publication_generation FROM workspaces WHERE id=$1 FOR UPDATE`, s.Workspace).Scan(&active, &currentGeneration); err != nil {
			return err
		}
		if active != release || currentGeneration != generation {
			return ErrStale
		}
		var raw []byte
		var scoredAt time.Time
		if err := tx.QueryRow(ctx, `SELECT r.results,r.created_at FROM knowledge_releases l JOIN workspaces w ON w.id=l.workspace_id JOIN publication_receipts r
            ON (r.workspace_id,r.candidate_id)=(l.workspace_id,coalesce(w.active_publication_receipt_id,l.receipt_id)) WHERE l.workspace_id=$1 AND l.id=$2`, s.Workspace, release).Scan(&raw, &scoredAt); err != nil {
			return err
		}
		var reports [2]evaluations.Report
		if err := json.Unmarshal(raw, &reports); err != nil {
			return err
		}
		for _, match := range matches {
			if err := s.reconcileMatch(ctx, tx, release, head, match, reports, scoredAt); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) reconcileMatch(ctx context.Context, tx pgx.Tx, release, head string, match mergeMatch, reports [2]evaluations.Report, scoredAt time.Time) error {
	tag, err := tx.Exec(ctx, `INSERT INTO proposal_merge_links(workspace_id,proposal_id,pr_number,merge_sha,release_id,head_sha)
        VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, s.Workspace, match.item.id, match.pr.Number, match.pr.MergeSHA, release, match.pr.HeadSHA)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	var ids []string
	if err := tx.QueryRow(ctx, `UPDATE proposals SET approval_status='merged',updated_at=now() WHERE workspace_id=$1 AND id=$2
        RETURNING gap_ids::text[]`, s.Workspace, match.item.id).Scan(&ids); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.advanceGap(ctx, tx, id, head, reports, scoredAt); err != nil {
			return err
		}
	}
	_, err = audit.Append(ctx, tx, s.Workspace, audit.Event{Actor: "publication", Action: "reconciled-pr", ObjectType: "proposal", ObjectID: match.item.id,
		Revision: head, Details: map[string]any{"pr": match.pr.Number, "merge": match.pr.MergeSHA, "release": release}})
	return err
}

func (s *Service) advanceGap(ctx context.Context, tx pgx.Tx, id, head string, reports [2]evaluations.Report, scoredAt time.Time) error {
	var state string
	var targets []string
	err := tx.QueryRow(ctx, `SELECT state,ARRAY(SELECT case_id FROM gap_retrieval_cases c WHERE c.workspace_id=g.workspace_id AND c.gap_id=g.id AND c.created_at<=$3)
        FROM gap_reports g WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, s.Workspace, id, scoredAt).Scan(&state, &targets)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrIdentity
	}
	if err != nil {
		return err
	}
	for _, next := range []string{domain.StateAccepted, domain.StatePublished, domain.StateVerified} {
		if !domain.CanTransition(state, next) {
			continue
		}
		proof := ""
		if next == domain.StateVerified {
			if !evaluations.ProvesImprovement(reports[0], reports[1], targets) {
				break
			}
			proof = "retrieval"
		}
		if _, err := tx.Exec(ctx, `UPDATE gap_reports SET state=$3,proof_kind=coalesce(NULLIF($4,''),proof_kind),updated_at=now()
            WHERE workspace_id=$1 AND id=$2`, s.Workspace, id, next, proof); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO gap_events(id,workspace_id,gap_id,from_state,to_state,actor,reason,correlation_id)
            VALUES ($1,$2,$3,$4,$5,'publication','forge-confirmed release',$6)`, uuid.NewString(), s.Workspace, id, state, next, head); err != nil {
			return err
		}
		state = next
	}
	return nil
}

func (s *Service) PinProposal(ctx context.Context, id, base string) error {
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE proposals SET forge_base_sha=$3 WHERE workspace_id=$1 AND id=$2
            AND (forge_base_sha IS NULL OR forge_base_sha=$3)`, s.Workspace, id, base)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrStale
		}
		return err
	})
}

func (s *Service) recordMappingMismatch(ctx context.Context, id string) error {
	return s.DB.WithWorkspace(ctx, s.Workspace, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE proposals SET approval_status='changes-requested'
            WHERE workspace_id=$1 AND id=$2 AND approval_status IN ('open','approved')`, s.Workspace, id)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = audit.Append(ctx, tx, s.Workspace, audit.Event{Actor: "publication", Action: "pr-mapping-mismatch", ObjectType: "proposal", ObjectID: id})
		return err
	})
}
