// Command calibrate is the internal labelling tool's CLI.
//
// Local only. It refuses to run without CALIBRATION_ENABLED, reads candidates
// from the product database and writes everything else to a local one, and is
// never mounted in the API.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/calibration"
)

func main() {
	if os.Getenv("CALIBRATION_ENABLED") != "true" {
		fmt.Fprintln(os.Stderr, "refusing to run: CALIBRATION_ENABLED is not 'true'.")
		fmt.Fprintln(os.Stderr, "This is an internal tool. It is never enabled in production.")
		os.Exit(2)
	}

	name := flag.String("name", "", "sample name, e.g. set-1")
	seed := flag.Int64("seed", 0, "PRNG seed; required to draw, and recorded with the draw")
	dry := flag.Bool("dry-run", false, "draw and print the composition without writing anything")
	snapshot := flag.Bool("snapshot", false, "fetch and freeze the diff and linked issue for a drawn sample")
	status := flag.Bool("status", false, "report how complete a sample's snapshots are")
	flag.Parse()

	if *name == "" {
		fmt.Fprintln(os.Stderr, "usage:")
		fmt.Fprintln(os.Stderr, "  calibrate -name set-1 -seed 20260813 [-dry-run]   draw a sample")
		fmt.Fprintln(os.Stderr, "  calibrate -name set-1 -snapshot                   freeze diffs and linked issues")
		fmt.Fprintln(os.Stderr, "  calibrate -name set-1 -status                     report snapshot coverage")
		os.Exit(2)
	}
	if !*snapshot && !*status && *seed == 0 {
		fmt.Fprintln(os.Stderr, "drawing requires -seed")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// The local database is always needed. The product database is needed only
	// to draw - snapshotting and status work entirely from what the draw
	// already recorded, which is the point of copying pr_number onto the
	// sample row.
	localDSN := os.Getenv("CALIBRATION_DB_URL")
	if localDSN == "" {
		fmt.Fprintln(os.Stderr, "CALIBRATION_DB_URL (local) is required")
		os.Exit(2)
	}
	local, err := pgxpool.New(ctx, localDSN)
	if err != nil {
		fatal("connect local: %v", err)
	}
	defer local.Close()

	if *status {
		reportCoverage(ctx, local, *name)
		return
	}
	if *snapshot {
		runSnapshots(ctx, local, *name)
		return
	}

	sourceDSN := os.Getenv("CALIBRATION_SOURCE_DB_URL")
	if sourceDSN == "" {
		fmt.Fprintln(os.Stderr, "CALIBRATION_SOURCE_DB_URL (read-only, product) is required to draw a sample")
		os.Exit(2)
	}
	source, err := pgxpool.New(ctx, sourceDSN)
	if err != nil {
		fatal("connect source: %v", err)
	}
	defer source.Close()

	candidates, err := calibration.LoadCandidates(ctx, source)
	if err != nil {
		fatal("%v", err)
	}
	exclude, err := calibration.AlreadySampled(ctx, local)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("candidates: %d   already sampled (excluded): %d\n", len(candidates), len(exclude))

	gh := calibration.NewGitHubClient(githubToken())
	plan := calibration.DefaultPlan()

	start := time.Now()
	draw, err := calibration.DrawSample(ctx, candidates, plan, *seed, gh.SizeFnFor(), exclude)
	if err != nil {
		fatal("draw: %v", err)
	}
	fmt.Printf("drew %d in %s, %d candidates examined (one API call each)\n\n",
		len(draw.Selected), time.Since(start).Round(time.Second), draw.Examined)

	printComposition(draw)

	if *dry {
		fmt.Println("\ndry run: nothing written")
		return
	}
	id, err := calibration.PersistDraw(ctx, local, *name, draw)
	if err != nil {
		fatal("persist: %v", err)
	}
	fmt.Printf("\nwritten: sample %s (%s)\ncandidate pool hash: %s\n", *name, id, draw.CandidateHash)
}

// runSnapshots freezes every outstanding pull request in a sample.
//
// Resumable and honest about incompleteness: it fetches only what is missing,
// and if anything fails it names each one and exits non-zero. A sample that is
// 20 of 25 must never look finished - a labeller who starts on a subset
// produces a partial set that reads like a whole one.
func runSnapshots(ctx context.Context, local *pgxpool.Pool, name string) {
	pending, err := calibration.PendingSnapshots(ctx, local, name)
	if err != nil {
		fatal("%v", err)
	}
	if len(pending) == 0 {
		fmt.Println("nothing to fetch; every pull request in this sample is already snapshotted")
		reportCoverage(ctx, local, name)
		return
	}
	fmt.Printf("fetching %d snapshots\n\n", len(pending))

	gh := calibration.NewGitHubClient(githubToken())
	var failures []calibration.SnapshotResult
	for i, p := range pending {
		res := calibration.FetchAndStore(ctx, local, gh, p)
		switch {
		case res.Err != nil:
			fmt.Printf("  %2d/%d  %-36s #%-6d FAILED: %v\n", i+1, len(pending), p.ProjectFullName, p.Number, res.Err)
			failures = append(failures, res)
		default:
			note := "no linked issue"
			if res.HasIssue {
				note = "issue frozen"
			}
			if res.Truncated {
				note += ", diff truncated"
			}
			fmt.Printf("  %2d/%d  %-36s #%-6d ok (%s)\n", i+1, len(pending), p.ProjectFullName, p.Number, note)
		}
	}

	fmt.Println()
	reportCoverage(ctx, local, name)

	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d snapshot(s) failed. Re-run with -snapshot to retry only those.\n", len(failures))
		os.Exit(1)
	}
}

func reportCoverage(ctx context.Context, local *pgxpool.Pool, name string) {
	c, err := calibration.Coverage(ctx, local, name)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("snapshots: %d of %d", c.Snapshot, c.Total)
	if c.Complete {
		fmt.Printf("  COMPLETE - ready to label\n")
	} else {
		fmt.Printf("  INCOMPLETE - not ready to label\n")
	}
	fmt.Printf("linked issues frozen: %d of %d snapshotted (%d have none, which the screen states plainly)\n",
		c.WithIssue, c.Snapshot, c.Snapshot-c.WithIssue)
	if len(c.Missing) > 0 {
		fmt.Println("missing:")
		for _, m := range c.Missing {
			fmt.Printf("  - %s\n", m)
		}
	}
}

func printComposition(d calibration.Draw) {
	byProject, byBand, merged, unmerged, held := d.Composition()

	fmt.Println("by project:")
	projects := make([]string, 0, len(byProject))
	for p := range byProject {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	for _, p := range projects {
		fmt.Printf("  %-36s %d\n", p, byProject[p])
	}

	fmt.Println("\nby size band:")
	for _, b := range []calibration.SizeBand{
		calibration.BandTiny, calibration.BandSmall, calibration.BandMedium,
		calibration.BandLarge, calibration.BandHuge,
	} {
		fmt.Printf("  %-8s %d (target %d)\n", b, byBand[b], d.Plan.BandTargets[b])
	}

	corpusPct := 0.0
	if d.CorpusTotal > 0 {
		corpusPct = 100 * float64(d.CorpusUnmerged) / float64(d.CorpusTotal)
	}
	fmt.Printf("\noutcome:   merged %d, unmerged %d  (target %d-%d)\n", merged, unmerged, d.Plan.UnmergedFloor, d.Plan.UnmergedCeiling)
	fmt.Printf("           sample is %.0f%% unmerged against a corpus that is %.0f%% (%d of %d)\n",
		100*float64(unmerged)/float64(len(d.Selected)), corpusPct, d.CorpusUnmerged, d.CorpusTotal)
	fmt.Printf("held back: %d\n", held)

	if len(d.Relaxations) > 0 {
		fmt.Println("\nrelaxations - constraints the pool could not satisfy exactly:")
		for _, r := range d.Relaxations {
			fmt.Printf("  - %s\n", r)
		}
	}

	fmt.Println("\nthe drawn set:")
	for _, s := range d.Selected {
		flag := ""
		if s.HeldBack {
			flag = "  [HELD BACK]"
		}
		state := "unmerged"
		if s.Merged {
			state = "merged"
		}
		fmt.Printf("  %-36s #%-6d %-8s %-8s %5d lines%s\n",
			s.ProjectFullName, s.Number, state, s.Band, s.ChangedLines, flag)
	}
}

// githubToken prefers an explicit token and falls back to the gh CLI, which is
// how a local operator is already authenticated.
func githubToken() string {
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
