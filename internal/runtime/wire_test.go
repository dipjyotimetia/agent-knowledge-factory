package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/runtime"
)

func TestReconcileReportsAMissingRepository(t *testing.T) {
	signaler := runtime.NewReviews(nil, config.Config{Workspace: config.Workspace{ID: "banking-demo", DefaultBranch: "main"}}, nil, nil, func(string) string { return "" })
	err := signaler.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "database") {
		t.Fatal(err)
	}
}
