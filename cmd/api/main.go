package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/api"
	"github.com/jagadeesh/grainlify/backend/internal/bus"
	"github.com/jagadeesh/grainlify/backend/internal/bus/natsbus"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
	shutdownwait "github.com/jagadeesh/grainlify/backend/internal/shutdown"
	"github.com/jagadeesh/grainlify/backend/internal/syncjobs"
)

// Build metadata populated via -ldflags at build time.
// Example:
//
//	go build -ldflags="-X main.Version=$(git describe --tags --always) \
//	                   -X main.Commit=$(git rev-parse --short HEAD) \
//	                   -X main.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" ./cmd/api
var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = "unknown"
)

func main() {
	slog.Info("=== Grainlify API Starting ===")
	slog.Info("loading environment variables", "step", "1", "action", "loading_environment_variables")

	config.LoadDotenv()
	slog.Info("loading configuration", "step", "2", "action", "loading_configuration")
	cfg := config.Load()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel(),
	}))
	slog.SetDefault(logger)

	if err := cfg.Validate(); err != nil {
		slog.Error("startup config validation failed", "error", err)
		os.Exit(1)
	}

	// Log configuration (mask sensitive values)
	slog.Info("configuration loaded", "step", "3", "action", "configuration_loaded",
		"env", cfg.Env,
		"log_level", cfg.Log,
		"http_addr", cfg.HTTPAddr,
		"port", os.Getenv("PORT"),
		"db_url_set", cfg.DBURL != "",
		"auto_migrate", cfg.AutoMigrate,
		"jwt_secret_set", cfg.JWTSecret != "",
		"nats_url_set", cfg.NATSURL != "",
		"github_oauth_client_id_set", cfg.GitHubOAuthClientID != "",
		"public_base_url", cfg.PublicBaseURL,
		"shutdown_timeout", cfg.ShutdownTimeout.String(),
	)

	slog.Info("connecting to database", "step", "4", "action", "connecting_to_database")
	var database *db.DB
	if cfg.DBURL == "" {
		if cfg.Env != "dev" {
			slog.Error("db connection failed", "step", "4", "action", "db_connection_failed",
				"error", "DB_URL is required in non-dev environments",
				"env", cfg.Env,
			)
			os.Exit(1)
		}
		slog.Warn("db connection skipped", "step", "4", "action", "db_connection_skipped",
			"reason", "DB_URL not set; running without database (only /health will be useful)",
		)
	} else {
		slog.Info("parsing db url", "step", "4.1", "action", "parsing_db_url", "db_url_length", len(cfg.DBURL))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		slog.Info("attempting db connection", "step", "4.2", "action", "attempting_db_connection", "timeout", "10s")
		d, err := db.Connect(ctx, cfg.DBURL, db.PoolConfig{
			MaxConns:        cfg.DBMaxConns,
			MinConns:        cfg.DBMinConns,
			MaxConnLifetime: cfg.DBMaxConnLifetime,
			MaxConnIdleTime: cfg.DBMaxConnIdleTime,
		})
		cancel()
		if err != nil {
			slog.Error("db connection failed", "step", "4", "action", "db_connection_failed",
				"error", err,
				"error_type", fmt.Sprintf("%T", err),
			)
			os.Exit(1)
		}
		slog.Info("db connection successful", "step", "4.3", "action", "db_connection_successful",
			"max_conns", cfg.DBMaxConns,
		)
		database = d

		if cfg.AutoMigrate {
			slog.Info("checking if migrations are needed", "step", "5", "action", "checking_migrations")
			needsMigration, err := migrate.NeedsMigration(context.Background(), database.Pool)
			if err != nil {
				slog.Error("failed to check if migrations are needed", "step", "5", "action", "check_migration_failed",
					"error", err,
					"error_type", fmt.Sprintf("%T", err),
				)
				// If we can't check, assume migrations are needed to be safe
				needsMigration = true
			}

			if needsMigration {
				slog.Info("migrations needed, running database migrations", "step", "5", "action", "running_database_migrations")
				// Use background context - migrations handle their own retries without timeouts
				allowIrreversible := cfg.IsDev() || os.Getenv("MIGRATE_ALLOW_IRREVERSIBLE") == "1"
				err := migrate.Up(context.Background(), database.Pool, allowIrreversible)
				if err != nil {
					slog.Error("migration failed", "step", "5", "action", "migration_failed",
						"error", err,
						"error_type", fmt.Sprintf("%T", err),
					)
					os.Exit(1)
				}
				slog.Info("migrations complete", "step", "5", "action", "migrations_complete")
			} else {
				slog.Info("migrations up to date, skipping", "step", "5", "action", "migrations_skipped")
			}
		} else {
			slog.Info("migrations skipped", "step", "5", "action", "migrations_skipped", "reason", "AUTO_MIGRATE=false")
		}
	}

	slog.Info("connecting to nats", "step", "6", "action", "connecting_to_nats")
	var eventBus bus.Bus
	if cfg.NATSURL != "" {
		slog.Info("nats url provided", "step", "6.1", "action", "nats_url_provided", "nats_url_length", len(cfg.NATSURL))
		b, err := natsbus.Connect(cfg.NATSURL)
		if err != nil {
			slog.Error("nats connection failed", "step", "6", "action", "nats_connection_failed",
				"error", err,
				"error_type", fmt.Sprintf("%T", err),
			)
			os.Exit(1)
		}
		slog.Info("nats connection successful", "step", "6.2", "action", "nats_connection_successful")
		eventBus = b
	} else {
		slog.Info("nats skipped", "step", "6", "action", "nats_skipped", "reason", "NATS_URL not set")
	}

	slog.Info("initializing api", "step", "7", "action", "initializing_api")
	buildInfo := handlers.BuildInfo{Version: Version, Commit: Commit, BuildTime: BuildTime}
	app := api.New(cfg, api.Deps{DB: database, Bus: eventBus}, buildInfo)
	slog.Info("api initialized", "step", "7", "action", "api_initialized",
		"version", Version, "commit", Commit, "build_time", BuildTime)

	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	var workerWG sync.WaitGroup

	// Background workers (dev convenience). In production we run `cmd/worker` instead.
	// If NATS is configured, prefer the external worker process.
	if cfg.NATSURL == "" && database != nil && database.Pool != nil {
		slog.Info("starting background worker", "step", "8", "action", "starting_background_worker")
		worker := syncjobs.New(cfg, database.Pool)
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			slog.Info("background worker started")
			if err := worker.Run(workerCtx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("background worker exited", "error", err)
			}
		}()

		// GitHub App cleanup is now handled via webhooks (installation.deleted events)
		// No need for periodic polling
	} else {
		slog.Info("background worker skipped", "step", "8", "action", "background_worker_skipped",
			"reason", func() string {
				if cfg.NATSURL != "" {
					return "NATS configured (use external worker)"
				}
				if database == nil {
					return "database not available"
				}
				return "unknown"
			}(),
		)
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("starting http server", "step", "9", "action", "starting_http_server",
			"addr", cfg.HTTPAddr,
			"port", os.Getenv("PORT"),
		)
		errCh <- app.Listen(cfg.HTTPAddr)
	}()

	// The success banner is only logged once the listener is confirmed not
	// to have already failed within the grace window (e.g. address already
	// in use), rather than on a fixed timer race against errCh.
	if err := awaitListenerStartup(errCh, 100*time.Millisecond); err != nil {
		slog.Error("http server exited",
			"error", err,
			"error_type", fmt.Sprintf("%T", err),
		)
		os.Exit(1)
	}
	slog.Info("=== Grainlify API Started Successfully ===",
		"http_addr", cfg.HTTPAddr,
		"env", cfg.Env,
	)

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		slog.Info("shutdown signal received", "signal", sig.String())
	case err := <-errCh:
		// Fiber returns nil only on clean shutdown; treat any error as fatal.
		slog.Error("http server exited",
			"error", err,
			"error_type", fmt.Sprintf("%T", err),
		)
		os.Exit(1)
	}

	slog.Info("initiating graceful shutdown", "step", "10", "action", "initiating_graceful_shutdown",
		"shutdown_timeout", cfg.ShutdownTimeout.String(),
	)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	runner := gracefulShutdownRunner{deps: gracefulShutdownDeps{
		ShutdownHTTP: func(ctx context.Context) error {
			return api.Shutdown(ctx, app)
		},
		StopWorkers: stopWorkers,
		WaitWorkers: func(ctx context.Context) error {
			return shutdownwait.Wait(ctx, &workerWG)
		},
		CloseBus: func(context.Context) error {
			if eventBus == nil {
				return nil
			}
			slog.Info("closing NATS connection")
			eventBus.Close()
			return nil
		},
		CloseDB: func(context.Context) error {
			if database == nil {
				return nil
			}
			slog.Info("closing database connection")
			database.Close()
			return nil
		},
	}}
	result := runner.Run(ctx)
	if result.WorkerErr != nil {
		slog.Warn("worker shutdown exceeded deadline", "error", result.WorkerErr)
	}
	if result.Err != nil {
		slog.Error("graceful shutdown failed",
			"error", result.Err,
			"error_type", fmt.Sprintf("%T", result.Err),
		)
		os.Exit(1)
	}

	slog.Info("shutdown complete")
}

type gracefulShutdownDeps struct {
	ShutdownHTTP func(context.Context) error
	StopWorkers  func()
	WaitWorkers  func(context.Context) error
	CloseBus     func(context.Context) error
	CloseDB      func(context.Context) error
}

type gracefulShutdownResult struct {
	Err       error
	WorkerErr error
}

type gracefulShutdownRunner struct {
	deps gracefulShutdownDeps
	once sync.Once
	res  gracefulShutdownResult
}

func (r *gracefulShutdownRunner) Run(ctx context.Context) gracefulShutdownResult {
	r.once.Do(func() {
		r.res = runGracefulShutdown(ctx, r.deps)
	})
	return r.res
}

// awaitListenerStartup waits up to grace for the HTTP listener to report a
// startup failure on errCh, so callers can avoid logging a success banner
// after a listener that already failed (e.g. address already in use).
//
// It returns the listener's error if one arrives within grace, or nil once
// grace elapses without one — at which point the listener is presumed to be
// up and accepting connections.
func awaitListenerStartup(errCh <-chan error, grace time.Duration) error {
	select {
	case err := <-errCh:
		return err
	case <-time.After(grace):
		return nil
	}
}

// runGracefulShutdown preserves the API process shutdown order. The HTTP
// listener stops first so new connections are rejected while in-flight requests
// can drain with the database still open. Workers are then canceled and waited
// on, the message bus is drained/closed, and the database pool is closed last.
func runGracefulShutdown(ctx context.Context, deps gracefulShutdownDeps) gracefulShutdownResult {
	var shutdownErr error

	if deps.ShutdownHTTP != nil {
		if err := deps.ShutdownHTTP(ctx); err != nil {
			return gracefulShutdownResult{Err: fmt.Errorf("shutdown http listener: %w", err)}
		}
	}

	if deps.StopWorkers != nil {
		deps.StopWorkers()
	}

	var workerErr error
	if deps.WaitWorkers != nil {
		workerErr = deps.WaitWorkers(ctx)
	}

	if deps.CloseBus != nil {
		if err := deps.CloseBus(ctx); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("close bus: %w", err))
		}
	}

	if deps.CloseDB != nil {
		if err := deps.CloseDB(ctx); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("close database: %w", err))
		}
	}

	return gracefulShutdownResult{Err: shutdownErr, WorkerErr: workerErr}
}
