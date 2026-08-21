// Command payout runs the founding-pool payout sequence.
//
// # Why this exists
//
// internal/founding and internal/payout were built, tested and mutation-tested,
// and nothing called them. Writing docs/RUNBOOK-founding-payout.md is what found
// it: a documented procedure that could not be performed, because four packages
// had no entry point. That is the eighth instance in this codebase of something
// built and never called, and the first where the consequence is an unrunnable
// runbook rather than a dormant column.
//
// # Why the irreversible steps are separate subcommands
//
// Not flags on one command. Three of the five irreversible moments in the payout
// sequence do not look irreversible - persisting a settlement, building a tree,
// and destroying a salt all complete quietly - and a subcommand somebody has to
// type is a small amount of friction in exactly the right place.
//
//	payout dry-run     repeatable, writes nothing
//	payout report      repeatable, writes nothing
//	payout status      read-only
//	payout persist     IRREVERSIBLE - creates the settlement id everything keys to
//	payout build       IRREVERSIBLE - freezes each address into a leaf
//	payout publish     records a chain publication, after verifying the root
//
// The read-only ones are trivially repeatable on purpose: the gate only works if
// re-reading the report is cheaper than arguing with it.
//
// # Why running these through a Go test is not an option
//
// A test writes to whatever TEST_DB_URL names, and building the habit of running
// one against a real settlement is precisely what cmd/migrate's host guard
// exists to prevent. That workaround does not come back as a convenience.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/chainread"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbguard"
	"github.com/jagadeesh/grainlify/backend/internal/founding"
	"github.com/jagadeesh/grainlify/backend/internal/payout"
	"github.com/jagadeesh/grainlify/backend/internal/settlement"
)

const usage = `payout — the founding-pool payout sequence

  READ-ONLY (repeatable, writes nothing)
    dry-run  --pool-usdc <amount>    compute a settlement without recording it
    report   --settlement <id>       the payout gate: leaves, exclusions, totals
    status   --settlement <id>       what has been built and published so far

  IRREVERSIBLE (each its own act, deliberately)
    persist  --pool-usdc <amount>    record a settlement  [creates the id everything keys to]
    build    --settlement <id> --digest <hex> --acknowledge-undeliverable <minor>
    publish  --settlement <id> --escrow <addr> --tx <hash>

  Every subcommand refuses a non-local database unless you name the host:
    --yes-run-against-remote-host=<host>
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	config.LoadDotenv()
	cfg := config.Load()

	// Same guard as cmd/migrate, and for a worse reason. A migration against the
	// wrong database is recoverable; persist and build against the wrong one
	// produce a settlement and a tree for an event that does not exist there,
	// and a publish funded against that tree moves real money on its strength.
	if err := dbguard.Check("go run ./cmd/payout "+sub, cfg.DBURL, args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	d, err := db.Connect(ctx, cfg.DBURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db connect failed: %v\n", err)
		os.Exit(1)
	}
	defer d.Close()

	var runErr error
	switch sub {
	case "dry-run":
		runErr = cmdDryRun(ctx, d, args)
	case "persist":
		runErr = cmdPersist(ctx, d, args)
	case "report":
		runErr = cmdReport(ctx, d, args)
	case "build":
		runErr = cmdBuild(ctx, d, cfg, args)
	case "publish":
		runErr = cmdPublish(ctx, d, args)
	case "status":
		runErr = cmdStatus(ctx, d, args)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, runErr)
		os.Exit(1)
	}
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func requireSettlement(args []string) (uuid.UUID, error) {
	v := flagValue(args, "--settlement")
	if v == "" {
		return uuid.Nil, errors.New("--settlement <id> is required")
	}
	return uuid.Parse(v)
}

func chainOf(args []string) string {
	if v := flagValue(args, "--chain"); v != "" {
		return v
	}
	return "aptos-testnet"
}

// foundingConfig sources the settlement inputs.
//
// The pool size is REQUIRED rather than defaulted. internal/founding falls back
// to 3000 USDC when the key is absent, which is a sensible library default and a
// dangerous operator one: the number that decides how much money is distributed
// should be typed by the person distributing it, and appear in their shell
// history next to the command that used it.
func foundingConfig(args []string) (map[string]string, error) {
	pool := flagValue(args, "--pool-usdc")
	if pool == "" {
		return nil, errors.New("--pool-usdc <amount> is required.\n" +
			"It is not defaulted here on purpose: this is the number that decides how much money\n" +
			"is distributed, and it should be typed rather than inherited from a library default")
	}
	return map[string]string{"founding_pool_usdc": pool}, nil
}

func cmdDryRun(ctx context.Context, d *db.DB, args []string) error {
	fc, err := foundingConfig(args)
	if err != nil {
		return err
	}
	res, err := founding.DryRun(ctx, d.Pool, fc)
	if err != nil {
		return err
	}
	fmt.Printf("SETTLEMENT DRY RUN — nothing recorded\n")
	fmt.Printf("  members            %d\n", len(res.Lines))
	fmt.Printf("  payable            %d\n", len(res.PayableLines()))
	fmt.Printf("  pool (minor)       %s\n", res.PoolMinor)
	fmt.Printf("  allocated (minor)  %s\n", res.TotalAllocatedMinor())
	fmt.Printf("\nNothing has been written. `payout persist` records this.\n")
	return nil
}

func cmdPersist(ctx context.Context, d *db.DB, args []string) error {
	// Its own subcommand rather than a --persist flag, because this creates the
	// settlement id that every later step keys to and it completes quietly.
	fc, err := foundingConfig(args)
	if err != nil {
		return err
	}
	res, err := founding.DryRun(ctx, d.Pool, fc)
	if err != nil {
		return err
	}
	if err := settlement.Persist(ctx, d.Pool, res); err != nil {
		return err
	}
	fmt.Printf("SETTLEMENT PERSISTED\n  settlement_id  %s\n", res.SettlementID)
	fmt.Printf("\nNext: payout report --settlement %s\n", res.SettlementID)
	return nil
}

func cmdReport(ctx context.Context, d *db.DB, args []string) error {
	sid, err := requireSettlement(args)
	if err != nil {
		return err
	}
	s, err := payout.LoadSettlement(ctx, d.Pool, sid, chainOf(args))
	if err != nil {
		return err
	}
	r, err := payout.DryRun(ctx, d.Pool, s)
	if err != nil {
		return err
	}
	fmt.Print(r.Render())
	return nil
}

func cmdBuild(ctx context.Context, d *db.DB, cfg config.Config, args []string) error {
	sid, err := requireSettlement(args)
	if err != nil {
		return err
	}
	digest := flagValue(args, "--digest")
	ackStr := flagValue(args, "--acknowledge-undeliverable")
	if digest == "" || ackStr == "" {
		return errors.New("build requires --digest and --acknowledge-undeliverable, both from the report.\n" +
			"Run `payout report --settlement <id>` and copy them; the acknowledgement is a figure that\n" +
			"appears only in the reconciliation, so it cannot be produced without having read it")
	}
	ack, ok := new(big.Int).SetString(ackStr, 10)
	if !ok {
		return fmt.Errorf("--acknowledge-undeliverable %q is not an integer of minor units", ackStr)
	}
	s, err := payout.LoadSettlement(ctx, d.Pool, sid, chainOf(args))
	if err != nil {
		return err
	}
	res, err := payout.Build(ctx, d.Pool, cfg.SaltEncKeyB64, s,
		payout.Acknowledgement{InputDigest: digest, ExcludedTotalMinor: ack})
	if err != nil {
		return err
	}
	fmt.Printf("TREE BUILT\n")
	fmt.Printf("  settlement   %s\n", res.SettlementID)
	fmt.Printf("  root         0x%x\n", res.Root)
	fmt.Printf("  leaves       %d\n", res.LeafCount)
	fmt.Printf("  FUND EXACTLY %s minor units\n", res.TotalMinor)
	fmt.Printf("\nNext, from the Aptos-Contracts checkout (the CLI profile lives there):\n")
	fmt.Printf("  initialise, fund %s, then publish_root with the root above.\n", res.TotalMinor)
	fmt.Printf("  Then: payout publish --settlement %s --escrow <addr> --tx <hash>\n", res.SettlementID)
	return nil
}

// cmdPublish records a chain publication that has already happened.
//
// It does not submit anything. Publishing is done with the aptos CLI, from the
// Aptos-Contracts checkout where the profile lives, and this records the result -
// which is the step that makes claims visible, because /me/claims keys on
// published_tx being set.
func cmdPublish(ctx context.Context, d *db.DB, args []string) error {
	sid, err := requireSettlement(args)
	if err != nil {
		return err
	}
	escrow := flagValue(args, "--escrow")
	tx := flagValue(args, "--tx")
	if escrow == "" || tx == "" {
		return errors.New("publish requires --escrow <address> and --tx <hash>")
	}
	// VERIFY THE ESCROW BEFORE RECORDING IT.
	//
	// --escrow is typed by a person, and `initialise` is ungated: anybody can
	// create an escrow at an address they control. Recording an unverified
	// address means every later chain read - claimed state, deadline, reminders -
	// asks somebody else's escrow and believes the answer is ours.
	//
	// The root makes the address self-verifying. Ours committed to a specific
	// 32 bytes; another escrow has different bytes or none. A typo cannot survive
	// this check, and neither can a substitution.
	var want []byte
	var chainID string
	if err := d.Pool.QueryRow(ctx,
		`SELECT root, chain_id FROM payout_event_roots WHERE settlement_id = $1`, sid).Scan(&want, &chainID); err != nil {
		return fmt.Errorf("no tree built for settlement %s: %w", sid, err)
	}
	cc, err := payout.ChainConfigFor(ctx, d.Pool, chainID)
	if err != nil {
		return err
	}
	var ref *string
	_ = d.Pool.QueryRow(ctx, `SELECT rpc_endpoint_ref FROM chain_configs WHERE chain_id=$1`, chainID).Scan(&ref)
	refName := ""
	if ref != nil {
		refName = *ref
	}
	nodeURL, err := chainread.EndpointFor(refName)
	if err != nil {
		return fmt.Errorf("cannot verify the escrow without a node: %w", err)
	}
	got, published, err := chainread.New(nodeURL).Root(ctx, cc.ContractAddress, escrow)
	if err != nil {
		return fmt.Errorf("reading the root of %s: %w", escrow, err)
	}
	if !published {
		return fmt.Errorf("escrow %s has NO published root.\n"+
			"Either the publish_root transaction did not land, or this is not the escrow you published to",
			escrow)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("escrow %s does not hold our root.\n"+
			"  on chain: 0x%x\n  ours:     0x%x\n"+
			"Nothing has been recorded. This address belongs to a different escrow - `initialise` is "+
			"ungated, so an escrow existing at an address proves nothing about whose it is.",
			escrow, got, want)
	}

	tag, err := d.Pool.Exec(ctx, `
		UPDATE payout_event_roots
		SET escrow_address = $2, published_tx = $3, published_at = now()
		WHERE settlement_id = $1 AND published_tx IS NULL`, sid, escrow, tx)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("nothing recorded: settlement %s has no unpublished root.\n"+
			"Either it was never built, or a publication is already recorded - check `payout status`", sid)
	}
	fmt.Printf("PUBLICATION RECORDED\n  settlement %s\n  escrow     %s\n  tx         %s\n", sid, escrow, tx)
	fmt.Printf("\nClaims are now visible to contributors: /me/claims keys on published_tx.\n")
	return nil
}

func cmdStatus(ctx context.Context, d *db.DB, args []string) error {
	sid, err := requireSettlement(args)
	if err != nil {
		return err
	}
	var leaves, claimed int
	var root []byte
	var total int64
	var tx *string
	_ = d.Pool.QueryRow(ctx, `SELECT count(*) FROM claim_leaves WHERE settlement_id=$1`, sid).Scan(&leaves)
	_ = d.Pool.QueryRow(ctx, `SELECT count(*) FROM chain_operations WHERE event_ref=$1 AND kind='claim' AND state='paid'`, sid.String()).Scan(&claimed)
	err = d.Pool.QueryRow(ctx, `SELECT root, total_minor, published_tx FROM payout_event_roots WHERE settlement_id=$1`, sid).Scan(&root, &total, &tx)
	fmt.Printf("SETTLEMENT %s\n", sid)
	if err != nil {
		fmt.Printf("  no tree built\n")
		return nil
	}
	fmt.Printf("  root          0x%x\n", root)
	fmt.Printf("  leaf total    %d minor units\n", total)
	fmt.Printf("  leaves        %d\n", leaves)
	fmt.Printf("  claimed       %d  (from chain_operations, written only by the reconciler)\n", claimed)
	if tx == nil {
		fmt.Printf("  published     NO — contributors cannot see this yet\n")
	} else {
		fmt.Printf("  published     %s\n", *tx)
	}
	var salt string
	_ = d.Pool.QueryRow(ctx, `SELECT CASE WHEN destroyed_at IS NULL THEN 'live' ELSE 'destroyed: '||destroyed_reason END
		FROM payout_event_salts WHERE settlement_id=$1`, sid).Scan(&salt)
	if salt != "" {
		fmt.Printf("  salt          %s\n", salt)
	}
	return nil
}
