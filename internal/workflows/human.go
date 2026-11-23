package workflows

import (
	"context"
	"strings"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
)

// HumanLines returns non-empty lines a reviewer added that were not in the proposal.
func HumanLines(proposal curator.Proposal, original, merged map[string]string) []string {
	var extra []string
	seen := map[string]bool{}
	for _, edit := range proposal.Edits {
		body, ok := merged[edit.Path]
		if !ok {
			continue
		}
		before := original[edit.Path]
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || seen[line] {
				continue
			}
			if strings.Contains(before, line) || strings.Contains(edit.New, line) || strings.Contains(edit.Old, line) {
				continue
			}
			seen[line] = true
			extra = append(extra, line)
		}
	}
	return extra
}

func (s *Steps) noteHumanDelta(ctx context.Context, opened Opened, merged map[string]string, sha string) error {
	lines := HumanLines(opened.Proposal, opened.Files, merged)
	if len(lines) == 0 || s.Gaps == nil {
		return nil
	}
	observed := ""
	if len(opened.Proposal.Edits) > 0 {
		observed = opened.Proposal.Edits[0].New
	}
	_, err := s.Gaps.Report(ctx, domain.GapReport{
		WorkspaceID:   s.Workspace,
		AgentRunID:    "human-delta",
		TaskKind:      "review",
		Category:      domain.CategoryStaleInstruction,
		Summary:       "Human edit on the merged knowledge",
		Observed:      observed,
		Expected:      strings.Join(lines, "\n"),
		ClientEventID: "human-delta-" + sha,
	}, "reviewer")
	return err
}
