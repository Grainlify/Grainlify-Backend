// Command migrate applies pending migrations to the database named by DB_URL.
//
// # Why this refuses to run
//
// DB_URL is read from the local .env, and the local .env points at PRODUCTION.
// Nothing about running `go run ./cmd/migrate` looks dangerous, which is exactly
// what makes it dangerous: the safety was one person remembering, every time,
// that the obvious command migrates the live database.
//
// So it is the tool's job now. A non-local host is refused unless the operator
// names that host on the command line:
//
//	go run ./cmd/migrate --yes-run-against-remote-host=db.example.neon.tech
//
// The guard itself lives in internal/dbguard, because cmd/payout needs the same
// one and a second copy of a safety check is a check that drifts.
//
// The flag takes the host as its VALUE, and the value must match the host
// DB_URL actually resolves to. That is deliberate: a bare confirmation flag
// gets pasted out of shell history or a runbook and confirms whatever database
// happens to be configured now. One that names the host cannot be reused
// against a different one - it fails, loudly, naming both.
//
// Production migrations do not run through here. Railway starts ./cmd/api,
// which migrates at boot when AUTO_MIGRATE is set. This command is the manual
// path, and the manual path is the one with nobody watching.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbguard"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
)

func main() {
	config.LoadDotenv()
	cfg := config.Load()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel(),
	}))
	slog.SetDefault(logger)

	// Before connecting, not after: refusing at the door means a mistyped run
	// never opens a connection to the wrong database at all.
	if err := dbguard.Check("go run ./cmd/migrate", cfg.DBURL, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	d, err := db.Connect(ctx, cfg.DBURL)
	if err != nil {
		slog.Error("db connect failed", "error", err)
		os.Exit(1)
	}
	defer d.Close()

	if err := migrate.Up(ctx, d.Pool); err != nil {
		slog.Error("migrate up failed", "error", err)
		os.Exit(1)
	}

	slog.Info("migrations applied")
}
