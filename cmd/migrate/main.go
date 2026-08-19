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
	"strings"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbguard"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
)

// checkArgs rejects any argument this command does not implement.
//
// It exists because `go run ./cmd/migrate down 1` used to log "migrations up to
// date, no changes needed" and exit 0. The arguments were never parsed: only the
// host confirmation was inspected, everything else fell through, and main called
// Up unconditionally. So a rollback silently ran a no-op forward migration and
// reported success.
//
// That is worse than an unimplemented command. An unrecognised argument that
// errors tells you rollback is not available; one that exits 0 tells you
// rollback WORKED, and the next person to reach for it will be doing so in an
// incident.
//
// Exit 0 was a legal answer to a question nobody asked, which is the shape of a
// fault presenting as a legitimate value.
//
// Deliberately here rather than in internal/dbguard, which cmd/payout also uses:
// payout takes real subcommands (dry-run, persist, build, publish), so a shared
// "no subcommands" rule would break it. This command only ever migrates forward,
// and down migrations are applied by hand because reversing a migration against
// real rows is a decision, not a command-line argument.
func checkArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == dbguard.ConfirmFlag {
			i++ // its value, if given separately
			continue
		}
		if strings.HasPrefix(a, dbguard.ConfirmFlag+"=") {
			continue
		}
		return fmt.Errorf(
			"unrecognised argument %q.\n\n"+
				"This command only migrates forward, and takes no subcommand. In "+
				"particular there is no \"down\": rolling back is done by applying the "+
				"matching .down.sql by hand, because reversing a migration against real "+
				"rows is a decision rather than an argument.\n\n"+
				"    psql \"$DB_URL\" -v ON_ERROR_STOP=1 -f migrations/<version>.down.sql\n\n"+
				"The only accepted argument is %s=<host>.",
			a, dbguard.ConfirmFlag)
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
	// Arguments first: an unrecognised one is rejected before anything else, so a
	// mistyped command cannot be answered by doing something different.
	if err := checkArgs(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

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
