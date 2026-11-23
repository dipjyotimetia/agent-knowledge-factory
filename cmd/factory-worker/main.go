// Command factory-worker runs durable curation workflows.
// Workflow registration is added with the curator workflow. Until then the
// process only validates its configuration and waits for shutdown.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/buildinfo"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("factory-worker", "version", buildinfo.Version, "temporal_address", os.Getenv("TEMPORAL_ADDRESS"))
	if len(os.Args) > 1 && os.Args[1] == "--check" {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("no workflows registered yet")
	<-ctx.Done()
}
