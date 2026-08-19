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
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
)

const confirmFlag = "--yes-run-against-remote-host"

// localHosts are the only hosts that need no confirmation.
var localHosts = map[string]bool{
	"localhost": true, "127.0.0.1": true, "::1": true, "0.0.0.0": true,
	"host.docker.internal": true,
}

// hostOf extracts the host from either a URL-style DB_URL or a keyword DSN.
//
// It returns an error rather than a guess. An unparseable DB_URL must not be
// treated as local: that would make the one case we cannot reason about the one
// case that runs unguarded, which is the shape of every entry in
// docs/VERIFICATION-TRAPS.md.
func hostOf(dbURL string) (string, error) {
	s := strings.TrimSpace(dbURL)
	if s == "" {
		return "", fmt.Errorf("DB_URL is empty")
	}
	if strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("DB_URL is not a parseable URL: %w", err)
		}
		h := u.Hostname()
		if h == "" {
			return "", fmt.Errorf("DB_URL has no host")
		}
		return h, nil
	}
	// Keyword DSN: host=... user=... — a unix socket has no host or a path one.
	for _, field := range strings.Fields(s) {
		if strings.HasPrefix(field, "host=") {
			h := strings.TrimPrefix(field, "host=")
			if h == "" {
				return "", fmt.Errorf("DB_URL sets an empty host=")
			}
			if strings.HasPrefix(h, "/") {
				return "localhost", nil // unix socket is by definition this machine
			}
			return h, nil
		}
	}
	return "", fmt.Errorf("could not find a host in DB_URL")
}

func isLocal(host string) bool { return localHosts[strings.ToLower(host)] }

// confirmedHost returns the host named by the confirmation flag, if present.
func confirmedHost(args []string) (string, bool) {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, confirmFlag+"="); ok {
			return v, true
		}
		if a == confirmFlag && i+1 < len(args) {
			return args[i+1], true
		}
		if a == confirmFlag {
			return "", true // present but with no value: seen, and will not match
		}
	}
	return "", false
}

// checkTarget decides whether this run may proceed. The error it returns is
// shown to the operator, so it names the host and never the URL - DB_URL
// carries a password.
func checkTarget(dbURL string, args []string) error {
	host, err := hostOf(dbURL)
	if err != nil {
		return fmt.Errorf("refusing to migrate: %w. Fix DB_URL, or run against a local database", err)
	}
	if isLocal(host) {
		return nil
	}
	named, present := confirmedHost(args)
	if !present {
		return fmt.Errorf(
			"refusing to migrate %q: it is not a local database.\n\n"+
				"This is very likely production. If you meant it, name the host:\n\n"+
				"    go run ./cmd/migrate %s=%s\n\n"+
				"Migrations are applied to production automatically by ./cmd/api at boot "+
				"when AUTO_MIGRATE is set, so you probably do not need this command at all.",
			host, confirmFlag, host)
	}
	if named != host {
		return fmt.Errorf(
			"refusing to migrate: the confirmation names a different database.\n\n"+
				"    you confirmed: %q\n"+
				"    DB_URL is:     %q\n\n"+
				"That mismatch is the point of naming the host - a confirmation copied "+
				"from elsewhere cannot approve this database. Check which one you meant.",
			named, host)
	}
	return nil
}

func main() {
	config.LoadDotenv()
	cfg := config.Load()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel(),
	}))
	slog.SetDefault(logger)

	// Before connecting, not after: refusing at the door means a mistyped run
	// never opens a connection to the wrong database at all.
	if err := checkTarget(cfg.DBURL, os.Args[1:]); err != nil {
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
