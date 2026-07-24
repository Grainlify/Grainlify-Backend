package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
)

func main() {
	allowIrreversibleFlag := flag.Bool("allow-irreversible", false, "allow irreversible migrations to run")
	dryRunFlag := flag.Bool("dry-run", false, "list pending migrations without applying them")
	flag.Parse()

	config.LoadDotenv()
	cfg := config.Load()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel(),
	}))
	slog.SetDefault(logger)

	if err := cfg.Validate(); err != nil {
		slog.Error("startup config validation failed", "error", err)
		os.Exit(1)
	}

	allowIrreversible := cfg.IsDev() || *allowIrreversibleFlag || os.Getenv("MIGRATE_ALLOW_IRREVERSIBLE") == "1"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	d, err := db.Connect(ctx, cfg.DBURL, db.PoolConfig{
		MaxConns:        cfg.DBMaxConns,
		MinConns:        cfg.DBMinConns,
		MaxConnLifetime: cfg.DBMaxConnLifetime,
		MaxConnIdleTime: cfg.DBMaxConnIdleTime,
	})
	if err != nil {
		slog.Error("db connect failed", "error", err)
		os.Exit(1)
	}
	defer d.Close()

	if *dryRunFlag {
		pending, err := migrate.PendingMigrations(ctx, d.Pool)
		if err != nil {
			slog.Error("dry-run failed", "error", err)
			os.Exit(1)
		}
		for _, p := range pending {
			// Use os.Stdout directly to bypass slog for scriptable output
			_, _ = os.Stdout.WriteString(p.Name + "\t" + p.Checksum + "\n")
		}
		os.Exit(0)
	}

	if err := migrate.Up(ctx, d.Pool, allowIrreversible); err != nil {
		slog.Error("migrate up failed", "error", err)
		os.Exit(1)
	}

	slog.Info("migrations applied")
}
