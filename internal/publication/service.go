package publication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/evaluations"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/index"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/knowledge/okf"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/policy"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

type EvidenceSet []curator.Evidence

func Digest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func candidateKey(id string) string { return "candidate:" + id }

func (s *Service) head(ctx context.Context) (string, error) {
	identity, head, err := s.Forge.Resolve(ctx)
	if err != nil {
		return "", err
	}
	if identity != s.Identity || head == "" {
		return "", ErrIdentity
	}
	return head, nil
}

func (s *Service) dataset() (string, []evaluations.Case, error) {
	cases, err := evaluations.Load(s.CasesPath)
	if err != nil {
		return "", nil, fmt.Errorf("%w: evaluation suite unavailable or invalid", ErrRejected)
	}
	ids := map[string]bool{}
	critical := false
	for _, c := range cases {
		if c.ID == "" || ids[c.ID] || (c.Kind == "documentation-gap" && (c.RetrievalQuery == "" || c.Answer == "")) {
			return "", nil, fmt.Errorf("%w: malformed evaluation case", ErrRejected)
		}
		ids[c.ID] = true
		critical = critical || (c.Kind == "documentation-gap" && c.Critical)
	}
	if !critical {
		return "", nil, fmt.Errorf("%w: critical retrieval cases required", ErrRejected)
	}
	raw, err := json.Marshal(cases)
	return Digest(raw), cases, err
}

func (s *Service) evaluator() string {
	return Digest(fmt.Appendf(nil, "retrieval-v1/top-3/hybrid=%t", s.Index.Hybrid))
}

// EvaluateCandidate scores the exact proposed tree without exposing it to search clients.
func (s *Service) EvaluateCandidate(ctx context.Context, proposal, baseSHA string, files map[string]string) (Receipt, error) {
	base, err := s.Baseline(ctx)
	if err != nil {
		return Receipt{}, err
	}
	if base.ReleaseID == "" || base.SHA != baseSHA {
		return Receipt{}, ErrStale
	}
	return s.evaluate(ctx, operation{}, proposal, base, "", files)
}

// ProposalBase returns the immutable, currently installed release for a new PR.
func (s *Service) ProposalBase(ctx context.Context) (Baseline, map[string]string, error) {
	base, err := s.Baseline(ctx)
	if err != nil {
		return base, nil, err
	}
	if base.ReleaseID == "" || base.Held {
		return base, nil, ErrHeld
	}
	files, err := s.Forge.Tree(ctx, base.SHA)
	return base, files, err
}

func (s *Service) evaluate(ctx context.Context, op operation, proposal string, base Baseline, sha string, files map[string]string) (Receipt, error) {
	receipt := Receipt{CandidateID: uuid.NewString(), Baseline: base, SHA: sha, PolicyDigest: s.PolicyDigest, EvaluatorDigest: s.evaluator()}
	reject := func(reason string) (Receipt, error) {
		receipt.Outcome, receipt.Reason = "rejected", reason
		if err := s.saveReceipt(ctx, op, proposal, receipt); err != nil {
			return receipt, err
		}
		return receipt, fmt.Errorf("%w: %s", ErrRejected, reason)
	}
	dataset, cases, err := s.dataset()
	if err != nil {
		return reject("evaluation-suite")
	}
	receipt.DatasetDigest = dataset
	if s.PolicyDigest == "" || s.Sources == nil {
		return reject("policy-unavailable")
	}
	receipt.TreeDigest = publish.TreeDigest(files)
	root, err := materialize(files)
	if errors.Is(err, publish.ErrUnsafeTree) {
		return reject("unsafe-tree")
	}
	if err != nil {
		return receipt, err
	}
	defer os.RemoveAll(root)
	bundle, err := okf.Load(root, okf.Options{})
	if err != nil || !bundle.Conformant() {
		return reject("invalid-bundle")
	}
	evidence, err := s.Sources(ctx)
	if err != nil {
		return receipt, err
	}
	allowed := map[string]bool{}
	for _, source := range evidence {
		if shaPattern.MatchString(source.Revision) && source.Digest != "" && source.Content != "" && policy.Redact(source.Content) == source.Content {
			allowed[source.Location] = true
			if repoPath, ok := strings.CutPrefix(source.Location, "github://"); ok {
				parts := strings.SplitN(repoPath, "/", 3)
				if len(parts) == 3 {
					allowed["https://github.com/"+parts[0]+"/"+parts[1]+"/blob/"+source.Revision+"/"+parts[2]] = true
				}
			}
		}
	}
	for _, doc := range bundle.Concepts {
		for _, source := range doc.Sources {
			if !allowed[source.Resource] {
				return reject("source-access")
			}
		}
	}
	if proposal != "" {
		mapping, err := s.getMapping(ctx, proposal)
		if err != nil {
			return receipt, err
		}
		original, err := s.Forge.Tree(ctx, mapping.base)
		if err != nil {
			return receipt, err
		}
		if !grounded(mapping, original, files, evidence) {
			return reject("claim-evidence")
		}
	}
	if op.id != "" && op.trigger != "pr-head" {
		previous, err := s.priorReceipt(ctx, op, receipt)
		if err != nil {
			return receipt, err
		}
		if previous != "" {
			receipt.CandidateID = previous
			receipt.Outcome, receipt.Reason = "passed", "validated"
			return receipt, nil
		}
	}
	if err := s.registerCandidate(ctx, op, receipt); err != nil {
		return receipt, err
	}
	key := candidateKey(receipt.CandidateID)
	if err := s.Index.Stage(ctx, s.Workspace, key, root); err != nil {
		if errors.Is(err, index.ErrInvalidBundle) {
			return reject("invalid-index")
		}
		return receipt, err
	}
	// The staged bytes are retained only for a passing release candidate.
	keep := false
	defer func() {
		if !keep {
			_ = s.Index.DropStage(context.WithoutCancel(ctx), s.Workspace, key)
		}
	}()
	if base.ReleaseID != "" && base.SHA != "" {
		receipt.Before, err = s.score(ctx, base.SHA, cases)
		if err != nil {
			return receipt, err
		}
	}
	receipt.After, err = s.score(ctx, key, cases)
	if err != nil {
		return receipt, err
	}
	if (base.ReleaseID == "" && receipt.After.CriticalRegressions > 0) || regressed(receipt.Before, receipt.After) {
		return reject("critical-retrieval")
	}
	if op.id != "" && op.trigger != "pr-head" && sha != "" {
		if err := s.checkMergeEvidence(ctx, base, sha, files, evidence); err != nil {
			if errors.Is(err, ErrRejected) {
				return reject("claim-evidence")
			}
			return receipt, err
		}
	}
	receipt.Outcome, receipt.Reason = "passed", "validated"
	if err := s.saveReceipt(ctx, op, proposal, receipt); err != nil {
		return receipt, err
	}
	keep = op.id != "" && op.trigger != "pr-head"
	return receipt, nil
}

func (s *Service) score(ctx context.Context, revision string, cases []evaluations.Case) (evaluations.Report, error) {
	report, err := evaluations.RunRetrieval(ctx, cases, func(ctx context.Context, q string) ([]evaluations.Hit, error) {
		hits, err := s.Index.SearchAt(ctx, s.Workspace, revision, q, "", 3)
		if err != nil {
			return nil, err
		}
		scored := make([]evaluations.Hit, 0, len(hits))
		for _, hit := range hits {
			item, err := s.Index.CandidateItem(ctx, s.Workspace, hit.URI, revision)
			if err != nil {
				return nil, err
			}
			scored = append(scored, evaluations.Hit{URI: item.URI, Body: item.Body})
		}
		return scored, nil
	})
	for i := range report.Results {
		report.Results[i].Detail = ""
	}
	return report, err
}

func regressed(before, after evaluations.Report) bool {
	passed := map[string]bool{}
	for _, r := range before.Results {
		if r.Critical && r.Passed {
			passed[r.ID] = true
		}
	}
	for _, r := range after.Results {
		if passed[r.ID] && !r.Passed {
			return true
		}
	}
	return false
}

func materialize(files map[string]string) (string, error) {
	if len(files) == 0 || len(files) > 4000 {
		return "", publish.ErrUnsafeTree
	}
	paths := map[string]bool{}
	total := 0
	for name, body := range files {
		key := strings.ToLower(name)
		total += len(body)
		if paths[key] || !publish.SafeTreePath(name) || len(body) > 1<<20 || total > 24<<20 || !utf8.ValidString(body) ||
			strings.ContainsRune(body, 0) || len(policy.ScanSecrets("path", name))+len(policy.ScanSecrets("body", body)) != 0 {
			return "", publish.ErrUnsafeTree
		}
		paths[key] = true
	}
	for name := range paths {
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			if paths[parent] {
				return "", publish.ErrUnsafeTree
			}
		}
	}
	root, err := os.MkdirTemp("", "akf-candidate-")
	if err != nil {
		return "", err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(root)
		}
	}()
	dir, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	for name, body := range files {
		if err := dir.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return "", err
		}
		if err := writeCandidateFile(dir, name, body); err != nil {
			return "", err
		}
	}
	complete = true
	return root, nil
}

func writeCandidateFile(dir *os.Root, name, body string) error {
	handle, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return publish.ErrUnsafeTree
	}
	if err != nil {
		return err
	}
	_, err = handle.WriteString(body)
	return errors.Join(err, handle.Close())
}

// Process claims one durable request. Caller timeouts bound external I/O.
func (s *Service) Process(ctx context.Context) (bool, error) {
	op, err := s.claim(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = s.process(ctx, op)
	if err == nil {
		return true, nil
	}
	state, reason := "queued", "transient-error"
	switch {
	case errors.Is(err, ErrHeld):
		state, reason = "held", "rollout-hold"
	case errors.Is(err, ErrRejected), errors.Is(err, ErrIdentity), errors.Is(err, publish.ErrUnsafeTree):
		state, reason = "rejected", "publication-check-failed"
	case errors.Is(err, ErrStale), errors.Is(err, publish.ErrStaleBase):
		reason = "baseline-or-head-advanced"
	}
	if finishErr := s.finish(ctx, op, state, reason); finishErr != nil && !errors.Is(finishErr, ErrStale) {
		return true, errors.Join(err, finishErr)
	}
	return true, err
}

func (s *Service) process(ctx context.Context, op operation) error {
	head, err := s.head(ctx)
	if err != nil {
		return err
	}
	base, err := s.Baseline(ctx)
	if err != nil {
		return err
	}
	if op.trigger == "pr-head" {
		mapping, err := s.getMapping(ctx, op.proposal)
		if err != nil {
			return err
		}
		pr, err := s.exactPR(ctx, mapping)
		if err != nil {
			return err
		}
		if pr.State != "open" {
			return s.finish(ctx, op, "superseded", "PR already closed")
		}
		files, err := s.Forge.Tree(ctx, pr.HeadSHA)
		if err != nil {
			return err
		}
		if _, err = s.evaluate(ctx, op, op.proposal, base, pr.HeadSHA, files); err != nil {
			return err
		}
		return s.finish(ctx, op, "superseded", "revised PR validated")
	}
	if base.ReleaseID != "" && base.SHA != "" {
		ancestor, err := s.Forge.IsAncestor(ctx, base.SHA, head)
		if err != nil {
			return err
		}
		if !ancestor {
			return fmt.Errorf("%w: history diverged", ErrRejected)
		}
	}
	files, err := s.Forge.Tree(ctx, head)
	if err != nil {
		return err
	}
	if base.ReleaseID != "" && base.SHA == head {
		if _, _, err := s.dataset(); err != nil {
			return err
		}
		if err := s.reconcile(ctx, base.ReleaseID, head, files); err != nil {
			return err
		}
		return s.finish(ctx, op, "superseded", "release already current")
	}

	receipt, err := s.evaluate(ctx, op, "", base, head, files)
	if err != nil {
		return err
	}
	release, err := s.promote(ctx, op, receipt.CandidateID, base.Generation)
	if err != nil {
		if !errors.Is(err, ErrStale) {
			_ = s.Index.DropStage(context.WithoutCancel(ctx), s.Workspace, candidateKey(receipt.CandidateID))
		}
		return err
	}
	// Recovery uses the active release receipt if the worker stops here.
	return s.reconcile(ctx, release, head, files)
}
