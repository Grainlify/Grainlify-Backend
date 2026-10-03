// Command syncjobs-cleanup is the one-off repair for the sync_jobs backlog the
// webhook firehose left behind: it collapses pending jobs to one per (project,
// job type) and takes back jobs stuck in 'running' by workers that no longer
// exist. Rows are marked cancelled or reset with a reason, never deleted.
//
// Dry run is the default and only reads, in a READ ONLY transaction:
//
//	go run ./cmd/syncjobs-cleanup
//
// Applying it changes rows, in one transaction, and against anything but a
// local database needs the host named:
//
//	go run ./cmd/syncjobs-cleanup --apply --yes-run-against-remote-host=<host>
//
// Running it again after an apply finds nothing to do. It is safe on either
// side of migration 20261003122120, which performs step (a) itself; running
// this first just leaves that migration nothing to collapse.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbguard"
	"github.com/jagadeesh/grainlify/backend/internal/syncjobs"
)

func main() {
	config.LoadDotenv()
	cfg := config.Load()

	fs := flag.NewFlagSet("syncjobs-cleanup", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "change rows (default: dry run, read only)")
	// One hour is over five times the longest job production has completed
	// (sync_issues, 674 s), and the old worker never renewed locked_at, so a
	// job older than this under it cannot still be running.
	stuckAfter := fs.Duration("stuck-after", time.Hour, "a 'running' job not renewed for this long is stuck")
	_ = fs.String(dbguard.ConfirmFlag[2:], "", "host of a non-local DB_URL, required with --apply")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %v\n", fs.Args())
		os.Exit(2)
	}
	if *apply {
		if err := dbguard.Check("go run ./cmd/syncjobs-cleanup --apply", cfg.DBURL, os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := db.Connect(ctx, cfg.DBURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "db connect failed:", err)
		os.Exit(1)
	}
	defer d.Close()

	r, err := syncjobs.Cleanup(ctx, d.Pool, *stuckAfter, *apply)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cleanup failed, nothing changed:", err)
		os.Exit(1)
	}

	mode := "DRY RUN (read only, nothing changed)"
	if r.Applied {
		mode = "APPLIED"
	}
	fmt.Printf("sync_jobs cleanup: %s, stuck-after %s\n\n", mode, *stuckAfter)
	printCounts("before", r.Before)
	fmt.Println("planned:")
	for _, a := range []string{syncjobs.ActionCancelDuplicatePending, syncjobs.ActionRequeueStuckRunning, syncjobs.ActionCancelStuckRunning} {
		fmt.Printf("  %-26s %d\n", a, r.Planned[a])
	}
	fmt.Println()
	if r.Applied {
		printCounts("after", r.After)
	}
}

func printCounts(label string, m map[string]int64) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Println(label + ":")
	for _, k := range keys {
		fmt.Printf("  %-26s %d\n", k, m[k])
	}
	fmt.Println()
}
