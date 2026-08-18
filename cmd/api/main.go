package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/api"
	"github.com/jagadeesh/grainlify/backend/internal/bus"
	"github.com/jagadeesh/grainlify/backend/internal/bus/natsbus"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
	"github.com/jagadeesh/grainlify/backend/internal/syncjobs"
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
	)

	// The boot gate. Before the database, before anything that could half-work:
	// if a variable behind a live feature is empty, refuse to start and say which
	// feature died. Most of these fail silently at runtime - an empty
	// PUBLIC_BASE_URL means Didit verifications complete and never come back, and
	// nothing logs an error - so the loud failure has to happen here or not at
	// all.
	//
	// Safe because Railway keeps the previous deployment serving when a new one
	// fails its healthcheck, proven by deploying a deliberately broken build and
	// watching production carry on. A refused boot is therefore a blocked bad
	// config, not an outage.
	if missing := cfg.MissingRequired(); len(missing) > 0 {
		if cfg.GatesBoot() {
			slog.Error("configuration gate failed", "step", "3.5", "action", "required_config_missing",
				"env", cfg.Env, "missing_count", len(missing))
			for _, r := range missing {
				slog.Error("required configuration missing",
					"variable", r.Name, "feature", r.Feature, "consequence", r.Consequence)
			}
			// Also to stderr as one block: the structured lines above are precise,
			// but a person staring at a failed deploy should not have to
			// reassemble them.
			fmt.Fprintln(os.Stderr, config.MissingRequiredMessage(missing))
			os.Exit(1)
		}
		// dev: name them, then carry on.
		for _, r := range missing {
			slog.Warn("required configuration missing (not gated in dev)",
				"variable", r.Name, "feature", r.Feature)
		}
	}

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
		d, err := db.Connect(ctx, cfg.DBURL)
		cancel()
		if err != nil {
			slog.Error("db connection failed", "step", "4", "action", "db_connection_failed",
				"error", err,
				"error_type", fmt.Sprintf("%T", err),
			)
			os.Exit(1)
		}
		slog.Info("db connection successful", "step", "4.3", "action", "db_connection_successful",
			"max_conns", 10,
		)
		database = d
		defer func() {
			slog.Info("closing database connection")
			database.Close()
		}()

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
				err := migrate.Up(context.Background(), database.Pool)
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
		defer func() {
			slog.Info("closing NATS connection")
			eventBus.Close()
		}()
	} else {
		slog.Info("nats skipped", "step", "6", "action", "nats_skipped", "reason", "NATS_URL not set")
	}

	slog.Info("initializing api", "step", "7", "action", "initializing_api")
	app := api.New(cfg, api.Deps{DB: database, Bus: eventBus})
	slog.Info("api initialized", "step", "7", "action", "api_initialized")

	// Background workers (dev convenience). In production we run `cmd/worker` instead.
	// If NATS is configured, prefer the external worker process.
	if cfg.NATSURL == "" && database != nil && database.Pool != nil {
		slog.Info("starting background worker", "step", "8", "action", "starting_background_worker")
		worker := syncjobs.New(cfg, database.Pool)
		go func() {
			slog.Info("background worker started")
			_ = worker.Run(context.Background())
		}()

		// GrainHack's periodic reconciliation crawl (AI-specs.md §2.2) -
		// re-enqueues sync_issues jobs for hackathon-relevant projects so
		// issue-prep label changes get picked up even with no other GitHub
		// webhook firing in between. Started alongside the main worker
		// above since it only ever produces sync_jobs rows for that same
		// worker to consume; same NATS_URL-unset condition applies.
		reconciler := hackathon.NewReconciler(database.Pool)
		go func() {
			slog.Info("hackathon reconciler started")
			_ = reconciler.Run(context.Background())
		}()

		// GrainHack's assignment pipeline (AI-specs.md §4): closes
		// application windows and runs their weighted draws, releases
		// stale assignments, and warns contributors before an event ends.
		// Separate from the reconciler above because this one writes
		// assignments and calls GitHub, rather than only enqueueing jobs.
		assignmentRunner := hackathon.NewAssignmentRunnerFromConfig(cfg, database.Pool)
		go func() {
			slog.Info("hackathon assignment runner started")
			_ = assignmentRunner.Run(context.Background())
		}()

		// Backstop for a Didit webhook that never arrived.
		//
		// A session flagged "In Review" is routed to OUR review queue, not
		// Didit's, and nothing surfaced that queue - three sessions sat there
		// for up to 22 hours and were found only when a contributor
		// complained. The webhook now alerts on the transition, but Didit
		// retries twice and then drops the delivery, and the only other thing
		// that re-reads a session is the status poll, which runs when the
		// contributor opens their billing page. Somebody told to wait has no
		// reason to open it.
		//
		// Alerts only. It never changes a status, and the claim table makes it
		// silent for any session already alerted about.
		kycSweeper := handlers.NewKYCReviewSweeper(cfg, database)
		go func() {
			slog.Info("kyc review sweeper started")
			kycSweeper.Run(context.Background())
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

	// Resolve fork state for projects indexed before is_fork existed.
	//
	// Started here rather than run as a one-off script because the exclusion it
	// feeds is a correctness property, not a migration chore: until a project's
	// fork state is known it counts toward ranking, so the resolution has to
	// happen wherever the code runs, not wherever somebody remembered to run a
	// command. It is a no-op once every row is resolved.
	go handlers.NewForkBackfiller(cfg, database).Run(context.Background())

	// Fill in description and topics for projects indexed before the sync
	// stored them. Deliberately does not touch needs_metadata - see the type.
	go handlers.NewMetadataBackfiller(cfg, database).Run(context.Background())

	errCh := make(chan error, 1)
	go func() {
		slog.Info("starting http server", "step", "9", "action", "starting_http_server",
			"addr", cfg.HTTPAddr,
			"port", os.Getenv("PORT"),
		)
		errCh <- app.Listen(cfg.HTTPAddr)
	}()

	// Give server a moment to start
	time.Sleep(100 * time.Millisecond)
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

	slog.Info("initiating graceful shutdown", "step", "10", "action", "initiating_graceful_shutdown")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := api.Shutdown(ctx, app); err != nil {
		slog.Error("graceful shutdown failed",
			"error", err,
			"error_type", fmt.Sprintf("%T", err),
		)
		os.Exit(1)
	}

	slog.Info("shutdown complete")
}
