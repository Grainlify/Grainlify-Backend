package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/ingest"
	"github.com/jagadeesh/grainlify/backend/internal/worker"
)

// TestWorkerDeps_NoPanic verifies that all types used by main can be
// instantiated without panicking when given nil/zero deps (no real DB or NATS).
func TestWorkerDeps_NoPanic(t *testing.T) {
	consumer := &worker.GitHubWebhookConsumer{
		Ingest: &ingest.GitHubWebhookIngestor{Pool: nil},
	}

	// Subscribe with a nil NATS conn must not panic and should return nil
	// (the implementation is a no-op when nc == nil).
	if err := consumer.Subscribe(context.Background(), nil, worker.GitHubWebhookQueueGroup); err != nil {
		t.Fatalf("Subscribe(nil conn) returned error: %v", err)
	}
}

// TestFailFast_MissingDBURL verifies that the startup guard exits non-zero
// when DB_URL is absent and APP_ENV is not "dev".
//
// We can't call os.Exit directly in tests, so we replicate the guard logic
// inline and check it returns the expected sentinel.
func TestFailFast_MissingDBURL(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("DB_URL", "")
	t.Setenv("NATS_URL", "")

	cfg := config.Load()
	if cfg.Env == "dev" {
		t.Skip("guard only fires in non-dev — skip if default env is dev")
	}
	if cfg.DBURL != "" {
		t.Fatal("expected DB_URL to be empty")
	}
	// The guard would call os.Exit(1); we just verify the condition is met.
	if cfg.DBURL == "" && cfg.Env != "dev" {
		return // guard would fire — test passes
	}
	t.Fatal("fail-fast guard would not fire as expected")
}

// TestFailFast_MissingNATSURL mirrors the NATS guard.
func TestFailFast_MissingNATSURL(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("DB_URL", "postgresql://user:pass@localhost/db")
	t.Setenv("NATS_URL", "")

	cfg := config.Load()
	if cfg.NATSURL != "" {
		t.Fatal("expected NATS_URL to be empty")
	}
	if cfg.NATSURL == "" && cfg.Env != "dev" {
		return
	}
	t.Fatal("fail-fast guard would not fire as expected")
}

// TestGracefulShutdown_ContextCancelStopsWait verifies that the WaitGroup
// drain pattern used in main unblocks when context is cancelled — without
// requiring real infrastructure.
func TestGracefulShutdown_ContextCancelStopsWait(t *testing.T) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	done := make(chan struct{})
	go func() {
		// Simulate a worker that exits when ctx is cancelled.
		<-ctx.Done()
		close(done)
	}()

	// Trigger shutdown by sending the process a SIGTERM.
	self, _ := os.FindProcess(os.Getpid())
	_ = self.Signal(syscall.SIGTERM)

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
		// Workers drained within timeout.
	case <-timer.C:
		t.Fatal("workers did not drain within timeout after SIGTERM")
	}
}
