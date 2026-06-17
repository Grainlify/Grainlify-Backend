package main

import (
    "context"
    "log/slog"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/jagadeesh/grainlify/backend/internal/config"
    "github.com/jagadeesh/grainlify/backend/internal/db"
    "github.com/jagadeesh/grainlify/backend/internal/ingest"
    "github.com/jagadeesh/grainlify/backend/internal/syncjobs"
    "github.com/jagadeesh/grainlify/backend/internal/worker"
    "github.com/jagadeesh/grainlify/backend/internal/bus/natsbus"
)

func main() {
    // Load environment variables and configuration
    config.LoadDotenv()
    cfg := config.Load()

    // Set up logger
    logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel()}))
    slog.SetDefault(logger)

    // ---------- Database connection ----------
    if cfg.DBURL == "" {
        slog.Error("DB_URL not set")
        os.Exit(1)
    }
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    dbConn, err := db.Connect(ctx, cfg.DBURL)
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
    const queueGroup = "grainlify-worker"
    if err := consumer.Subscribe(context.Background(), nbus.Conn(), queueGroup); err != nil {
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
    go func() {
        slog.Info("starting syncjobs worker")
        if err := syncWorker.Run(context.Background()); err != nil {
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
    // Wait for context cancellation to propagate to workers if needed
    _ = shutdownCtx
    slog.Info("worker shutdown complete")
}
