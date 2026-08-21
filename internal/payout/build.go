package payout

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/chain"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/salt"
)

var (
	ErrInputsChanged   = errors.New("the data changed since the dry run; re-run it and read the report again")
	ErrNotAcknowledged = errors.New("the undeliverable total was not acknowledged correctly")
	ErrNoLeaves        = errors.New("no payable members; there is no tree to build")
	ErrAmountTooLarge  = errors.New("amount exceeds what the contract can hold in a u64")
	ErrAlreadyBuilt    = errors.New("a root already exists for this settlement")
	ErrUnknownPool     = errors.New("unknown pool")
)

// poolKind maps the producer-facing string onto the digest-bound enum.
//
// An unrecognised value is an ERROR, never a default. The pool is hashed into
// every leaf, so defaulting a typo to "contributor" would build a complete,
// internally consistent tree for the wrong pool - a fault presenting as a
// legitimate value, which is the family that survives every check asking only
// whether something errored.
func poolKind(p string) (chain.PoolKind, error) {
	switch p {
	case "contributor":
		return chain.PoolKindContributor, nil
	case "maintainer":
		return chain.PoolKindMaintainer, nil
	default:
		return 0, fmt.Errorf("%w: %q (expected \"contributor\" or \"maintainer\")", ErrUnknownPool, p)
	}
}

// Acknowledgement is what a person must supply to build, having read the report.
//
// Deliberately not a boolean and deliberately without an override.
//
// ExcludedTotalMinor cannot be produced without having read the reconciliation,
// which is the only place it appears. A --yes flag would be typed by somebody in
// a hurry, and being in a hurry is exactly the state this exists to interrupt.
//
// If InputDigest mismatches, there is no way to proceed except re-running the dry
// run and reading it again. That is cheap, and it is correct behaviour rather
// than friction: exclusion is irreversible once a root is published.
type Acknowledgement struct {
	InputDigest        string
	ExcludedTotalMinor *big.Int
}

// Result is what a build produced.
type Result struct {
	SettlementID uuid.UUID
	Root         [32]byte
	LeafCount    int
	TotalMinor   *big.Int
	Report       *Report
}

// Build re-resolves, checks the acknowledgement against fresh data, then creates
// the salt, builds the tree and writes payout_event_roots and claim_leaves in one
// transaction.
//
// It re-resolves rather than trusting the passed report, so the digest compares
// what is true NOW against what the reader approved.
func Build(ctx context.Context, pool db.DBPool, saltKeyB64 string, s Settlement, ack Acknowledgement) (*Result, error) {
	fresh, err := DryRun(ctx, pool, s)
	if err != nil {
		return nil, err
	}

	if ack.InputDigest != fresh.InputDigest {
		return nil, fmt.Errorf("%w\n  report digest: %s\n  current data:  %s\n"+
			"Something moved: an address registered, an amount corrected, or a GitHub login renamed. "+
			"A rename changes an identity hash, and that hash goes into a permanent root.",
			ErrInputsChanged, ack.InputDigest, fresh.InputDigest)
	}
	if ack.ExcludedTotalMinor == nil || ack.ExcludedTotalMinor.Cmp(fresh.ExcludedTotalMinor) != 0 {
		got := "<nil>"
		if ack.ExcludedTotalMinor != nil {
			got = ack.ExcludedTotalMinor.String()
		}
		return nil, fmt.Errorf("%w: %d people earned %s minor units they will not receive; "+
			"acknowledge that exact figure (got %s)",
			ErrNotAcknowledged, len(fresh.Owed()), fresh.ExcludedTotalMinor.String(), got)
	}

	pk, err := poolKind(s.Pool)
	if err != nil {
		return nil, err
	}

	payable := fresh.Payable()
	if len(payable) == 0 {
		return nil, ErrNoLeaves
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM payout_event_roots WHERE settlement_id = $1)`, s.SettlementID).
		Scan(&exists); err != nil {
		return nil, fmt.Errorf("payout.Build: check existing root: %w", err)
	}
	if exists {
		return nil, ErrAlreadyBuilt
	}

	// The salt is created here, not at settlement: an event that never becomes a
	// tree never acquires a secret we then have to manage and destroy.
	if err := salt.Create(ctx, pool, saltKeyB64, s.SettlementID); err != nil && !errors.Is(err, salt.ErrAlreadyExists) {
		return nil, fmt.Errorf("payout.Build: create salt: %w", err)
	}

	logins := make([]string, len(payable))
	for i, m := range payable {
		logins[i] = m.GitHubLogin
	}

	var leaves []chain.ClaimLeaf
	if err := salt.WithSalt(ctx, pool, saltKeyB64, s.SettlementID, func(h salt.Hasher) error {
		ids, err := h.IdentityHashes(logins)
		if err != nil {
			return err
		}
		leaves = make([]chain.ClaimLeaf, len(payable))
		for i, m := range payable {
			// The contract holds an amount as u64. A wrapped amount is a
			// well-formed leaf for the wrong number - the family where a fault
			// presents as a legitimate value - so refuse rather than truncate.
			if m.AmountMinor.Cmp(new(big.Int).SetUint64(math.MaxUint64)) > 0 {
				return fmt.Errorf("%w: %s owes %s", ErrAmountTooLarge, m.GitHubLogin, m.AmountMinor)
			}
			leaves[i] = chain.ClaimLeaf{
				Pool:         pk,
				IdentityHash: ids[i],
				ClaimAddress: m.ClaimAddress,
				AmountMinor:  new(big.Int).Set(m.AmountMinor),
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("payout.Build: identity hashes: %w", err)
	}

	// # DO NOT RECOMPUTE identity_hash TO VERIFY A LEAF
	//
	// The hash committed here is H(lower(login) || salt) for the login as it was
	// at THIS moment. GitHub lets people rename, so recomputing it later can
	// legitimately produce a different value for the same person - and the root
	// is permanent.
	//
	// Anything checking a leaf must compare against the STORED claim_leaves
	// .identity_hash, never against a freshly computed one. The chain agrees:
	// `claim` takes identity_hash as a caller-supplied argument and never derives
	// it, which is what makes a rename harmless for the claimant.
	//
	// A recomputing verifier would pass for everyone who never renamed and fail
	// for everyone who did, which reads as data corruption and is not.
	tree, err := chain.BuildMerkleTree(leaves)
	if err != nil {
		return nil, fmt.Errorf("payout.Build: build tree: %w", err)
	}
	root := tree.Root

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("payout.Build: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO payout_event_roots (settlement_id, chain_id, escrow_address, root, total_minor, leaf_count)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		s.SettlementID, s.ChainID, "", root[:], fresh.LeafTotalMinor.Int64(), len(leaves)); err != nil {
		return nil, fmt.Errorf("payout.Build: insert root: %w", err)
	}

	for i, l := range leaves {
		lh := l.Hash()
		m := payable[i]
		if _, err := tx.Exec(ctx, `
			INSERT INTO claim_leaves
			  (settlement_id, leaf_index, leaf_hash, identity_hash, claim_address, amount_minor, pool)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			s.SettlementID, i, lh[:], l.IdentityHash[:], m.ClaimAddress, m.AmountMinor.Int64(), s.Pool); err != nil {
			return nil, fmt.Errorf("payout.Build: insert leaf %d: %w", i, err)
		}
	}

	// The ledger, written in the SAME transaction as the root and the leaves.
	//
	// Either the root, the leaves and the record of who was owed and got
	// nothing all land, or none of them do. Written here rather than anywhere
	// later because this is the moment the answer is true: afterwards it can
	// only be reconstructed by re-running resolve against address and account
	// data that has since moved, and somebody registering an address the day
	// after publication would silently change who "was excluded".
	//
	// Two records, deliberately, because they are different kinds of thing:
	//
	//   settlement_lines.excluded_reason - history. What tree N said. Never
	//     touched again, because tree N is immutable and editing its account
	//     later is editing the account of an event that already happened.
	//
	//   settlement_holds - live state. The object that moves: it is created
	//     here and released by a later settlement that pays it.
	for _, m := range fresh.Members {
		if !m.Outcome.Owed() {
			continue
		}
		tag, err := tx.Exec(ctx, `
			UPDATE `+settlementLinesTable+`
			SET excluded_reason = $1
			WHERE settlement_id = $2 AND user_id = $3`,
			string(m.Outcome), s.SettlementID, m.UserID)
		if err != nil {
			return nil, fmt.Errorf("payout.Build: record exclusion for %s: %w", m.UserID, err)
		}
		// An UPDATE matching nothing is the silent failure this whole record
		// exists to prevent, arriving one layer earlier: the publication would
		// succeed, the hold would be written, and the historical line would
		// say this person was never excluded from anything. Refuse instead -
		// the transaction rolls back and the root is not published.
		if tag.RowsAffected() != 1 {
			return nil, fmt.Errorf(
				"payout.Build: exclusion for %s matched %d settlement lines, want exactly 1; "+
					"the entitlement has no line to record against and publishing would lose the record",
				m.UserID, tag.RowsAffected())
		}
		// ON CONFLICT DO NOTHING rather than an upsert: a retried publication
		// of this tree must not create a second hold, and must not overwrite
		// the amount frozen by the first attempt either.
		if _, err := tx.Exec(ctx, `
			INSERT INTO settlement_holds (user_id, origin_settlement_id, amount_minor, reason)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id, origin_settlement_id) DO NOTHING`,
			m.UserID, s.SettlementID, m.AmountMinor.Int64(), string(m.Outcome)); err != nil {
			return nil, fmt.Errorf("payout.Build: record hold for %s: %w", m.UserID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("payout.Build: commit: %w", err)
	}

	return &Result{
		SettlementID: s.SettlementID,
		Root:         root,
		LeafCount:    len(leaves),
		TotalMinor:   fresh.LeafTotalMinor,
		Report:       fresh,
	}, nil
}
