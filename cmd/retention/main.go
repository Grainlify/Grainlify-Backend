// Command retention prints the dry run of the daily retention pass
// (internal/erasure/retention.go): what one pass would delete or update now,
// and which Didit sessions it would ask Didit to delete. Nothing else.
//
//	go run ./cmd/retention -dry-run
//	railway run --service grainlify -- go run ./cmd/retention -dry-run
//
// It reads DB_URL from the environment and prints the report as text, then as
// JSON.
//
// # Why there is no real run here
//
// The pass itself runs only inside the API process, and only when
// RETENTION_JOB_ENABLED is exactly "true". This command refuses to run without
// -dry-run, so the obvious command can never be the one that deletes.
//
// # Why it has no host guard
//
// cmd/migrate and cmd/payout refuse a non-local database unless the host is
// named (internal/dbguard), because they write. This one cannot: every
// connection it opens has default_transaction_read_only on, and the report
// itself is read inside BEGIN ... READ ONLY. Its whole purpose is to be read
// against production before the job is switched on there.
//
// It never calls Didit, GitHub or the bounty agent: the External it hands the
// dry run refuses every call.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/erasure"
)

// noCalls is the External of a dry run: it says whether Didit is configured
// in this environment, and refuses every call.
type noCalls struct{ didit bool }

var errNoCalls = errors.New("retention dry run: external calls are not allowed")

func (n noCalls) RevokeGitHub(context.Context, string) error              { return errNoCalls }
func (n noCalls) DeleteDiditSession(context.Context, string) error        { return errNoCalls }
func (n noCalls) EraseAtAgent(context.Context, int64, string, bool) error { return errNoCalls }
func (n noCalls) DiditConfigured() bool                                   { return n.didit }

func main() {
	dryRun := flag.Bool("dry-run", false, "report what one retention pass would do, without doing it (required)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/retention -dry-run")
		flag.PrintDefaults()
	}
	flag.Parse()
	if !*dryRun || flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "refusing to run: this command only does the dry run (-dry-run).\n"+
			"The retention pass itself runs only in the API process, when RETENTION_JOB_ENABLED=true.")
		os.Exit(2)
	}

	config.LoadDotenv()
	cfg := config.Load()
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(cfg config.Config) error {
	if cfg.DBURL == "" {
		return errors.New("DB_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Never print DB_URL or anything parsed from it: the errors below say
	// what failed, not where.
	pc, err := pgxpool.ParseConfig(cfg.DBURL)
	if err != nil {
		return errors.New("DB_URL could not be parsed")
	}
	pc.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pc.ConnConfig.RuntimeParams["application_name"] = "retention-dry-run"
	pc.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return errors.New("could not open a connection pool for DB_URL")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return errors.New("could not connect to the database named by DB_URL")
	}

	rep, err := erasure.NewRetention(pool, noCalls{didit: cfg.DiditAPIKey != ""}, 0).DryRun(ctx)
	if err != nil {
		return err
	}
	rep.WriteText(os.Stdout)
	fmt.Println("\n--- JSON ---")
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}
