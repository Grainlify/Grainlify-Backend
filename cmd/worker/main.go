// Package main is the entry point for the Grainlify background worker process.
//
// The worker connects to PostgreSQL and NATS, then runs two long-lived jobs
// concurrently:
//
//   - [worker.GitHubWebhookConsumer] — dequeues GitHub webhook events from NATS
//     and ingests them into the database via a queue-group subscription so that
//     multiple worker replicas share the load without duplicate processing.
//
//   - [syncjobs.Worker] — polls the sync_jobs table and executes scheduled
//     repository synchronisation tasks (sync_issues, sync_prs).
//
// # Configuration
//
// All configuration is read from environment variables (see .env.example).
// The two required variables for this binary are:
//
//   - DB_URL   — PostgreSQL connection string
//   - NATS_URL — NATS server URL
//
// In non-dev environments (APP_ENV != "dev") the process exits with status 1
// when either variable is absent.
//
// # Graceful shutdown
//
// SIGINT or SIGTERM cancels the root context, which:
//  1. Unsubscribes the NATS subscription (GitHubWebhookConsumer.Subscribe
//     returns when ctx.Done() fires).
//  2. Stops the syncjobs ticker loop (syncjobs.Worker.Run returns on ctx.Done()).
//
// The process then waits up to 10 s for in-flight work to finish before
// closing the NATS and database connections and exiting cleanly.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/bus/natsbus"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/ingest"
	"github.com/jagadeesh/grainlify/backend/internal/syncjobs"
	"github.com/jagadeesh/grainlify/backend/internal/worker"
)

func main() {
	config.LoadDotenv()
	cfg := config.Load()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel(),
	})))

	slog.Info("=== Grainlify Worker Starting ===", "env", cfg.Env)

	// Fail fast on missing required config.
	if cfg.DBURL == "" {
		if cfg.Env != "dev" {
			slog.Error("DB_URL is required in non-dev environments")
			os.Exit(1)
		}
		slog.Warn("DB_URL not set; worker has nothing to do — exiting")
		os.Exit(1)
	}
	if cfg.NATSURL == "" {
		if cfg.Env != "dev" {
			slog.Error("NATS_URL is required in non-dev environments")
			os.Exit(1)
		}
		slog.Warn("NATS_URL not set; worker has nothing to do — exiting")
		os.Exit(1)
	}

	// Database.
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 10*time.Second)
	dbConn, err := db.Connect(dbCtx, cfg.DBURL)
	dbCancel()
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer dbConn.Close()

	// NATS.
	nbus, err := natsbus.Connect(cfg.NATSURL)
	if err != nil {
		slog.Error("failed to connect to NATS", "error", err)
		os.Exit(1)
	}
	defer nbus.Close()

	// Root context — cancelled on shutdown signal.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	// GitHub webhook consumer.
	consumer := &worker.GitHubWebhookConsumer{
		Ingest: &ingest.GitHubWebhookIngestor{Pool: dbConn.Pool},
	}
	if err := consumer.Subscribe(ctx, nbus.Conn(), worker.GitHubWebhookQueueGroup); err != nil {
		slog.Error("failed to subscribe to webhook events", "error", err)
		os.Exit(1)
	}
	slog.Info("github webhook consumer subscribed")

	// Syncjobs worker.
	wg.Add(1)
	go func() {
		defer wg.Done()
		slog.Info("starting syncjobs worker")
		if err := syncjobs.New(cfg, dbConn.Pool).Run(ctx); err != nil && err != context.Canceled {
			slog.Error("syncjobs worker exited with error", "error", err)
		}
	}()

	slog.Info("=== Grainlify Worker Running ===")

	// Block until signal.
	<-ctx.Done()
	slog.Info("shutdown signal received, draining workers")

	// Give goroutines up to 10 s to finish in-flight work.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	shutdownTimer := time.NewTimer(10 * time.Second)
	defer shutdownTimer.Stop()
	select {
	case <-done:
		slog.Info("all workers drained cleanly")
	case <-shutdownTimer.C:
		slog.Warn("shutdown timeout exceeded; forcing exit")
	}

	slog.Info("worker shutdown complete")
}
