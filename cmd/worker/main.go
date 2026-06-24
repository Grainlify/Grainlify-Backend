package main

import (
	"context"
	"errors"
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
	shutdownwait "github.com/jagadeesh/grainlify/backend/internal/shutdown"
	"github.com/jagadeesh/grainlify/backend/internal/syncjobs"
	"github.com/jagadeesh/grainlify/backend/internal/worker"
)

func main() {
	// Load environment variables and configuration
	config.LoadDotenv()
	cfg := config.Load()

	// Set up logger
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel()}))
	slog.SetDefault(logger)

	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	var workerWG sync.WaitGroup

	// ---------- Database connection ----------
	if cfg.DBURL == "" {
		slog.Error("DB_URL not set")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	dbConn, err := db.Connect(ctx, cfg.DBURL, db.PoolConfig{
		MaxConns:        cfg.DBMaxConns,
		MinConns:        cfg.DBMinConns,
		MaxConnLifetime: cfg.DBMaxConnLifetime,
		MaxConnIdleTime: cfg.DBMaxConnIdleTime,
	})
	cancel()
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer func() {
		slog.Info("closing database connection")
		dbConn.Close()
	}()

	// ---------- NATS connection ----------
	if cfg.NATSURL == "" {
		slog.Error("NATS_URL not set")
		os.Exit(1)
	}
	nbus, err := natsbus.Connect(cfg.NATSURL)
	if err != nil {
		slog.Error("failed to connect to NATS", "error", err)
		os.Exit(1)
	}
	defer func() {
		slog.Info("closing NATS connection")
		nbus.Close()
	}()

	// ---------- GitHub webhook consumer ----------
	consumer := &worker.GitHubWebhookConsumer{Ingest: &ingest.GitHubWebhookIngestor{Pool: dbConn.Pool}}
	if err := consumer.Subscribe(workerCtx, nbus.Conn(), worker.GitHubWebhookQueueGroup); err != nil {
		slog.Error("failed to subscribe to webhook events", "error", err)
		os.Exit(1)
	}
	defer func() {
		if consumer.Sub != nil {
			_ = consumer.Sub.Unsubscribe()
		}
	}()

	// ---------- Sync jobs runner (concurrent) ----------
	syncWorker := syncjobs.New(cfg, dbConn.Pool)
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		slog.Info("starting syncjobs worker")
		if err := syncWorker.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("syncjobs worker exited", "error", err)
		}
	}()

	// ---------- Graceful shutdown ----------
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	slog.Info("shutdown signal received, draining workers")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	stopWorkers()
	if err := shutdownwait.Wait(shutdownCtx, &workerWG); err != nil {
		slog.Warn("worker shutdown exceeded deadline", "error", err)
	}
	slog.Info("worker shutdown complete")
}
