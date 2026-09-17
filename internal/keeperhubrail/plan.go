package keeperhubrail

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
	"github.com/jagadeesh/grainlify/backend/internal/payoutaddr"
)

type runRow struct {
	ID          uuid.UUID
	ChainID     string
	PayoutRunID uuid.UUID
}

// loadRun returns the run for an event and pool, or nil.
func loadRun(ctx context.Context, q pgx.Tx, hackathonID uuid.UUID, poolKind string, lock bool) (*runRow, error) {
	sql := `SELECT id, chain_id, hackathon_payout_run_id FROM keeperhub_payout_runs
	        WHERE hackathon_id = $1 AND pool = $2`
	if lock {
		// Serialises concurrent releases of the same run: the second waits,
		// then sees the first one's legs already dispatched.
		sql += ` FOR UPDATE`
	}
	var r runRow
	err := q.QueryRow(ctx, sql, hackathonID, poolKind).Scan(&r.ID, &r.ChainID, &r.PayoutRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("keeperhubrail: load run: %w", err)
	}
	return &r, nil
}

// planRun freezes an event's legs and exclusions, inside the caller's
// transaction.
//
// # Frozen here, never re-read
//
// Addresses, amounts and exclusion reasons are fixed at this moment. A person
// who registers an address tomorrow does not become a leg of this run, and one
// who changes their address does not redirect a leg already planned - the same
// freeze claim_leaves applies on the Aptos rail.
func (s *Service) planRun(ctx context.Context, tx pgx.Tx, req ReleaseRequest) (*runRow, []Exclusion, error) {
	// PURE PRODUCER. This computes and writes nothing; in particular it does
	// not write a settlements row, which would block the Aptos rail forever.
	res, err := hackathon.SettlementFor(ctx, s.Pool, req.HackathonID, req.Pool, req.ChainID)
	if err != nil {
		return nil, nil, err
	}

	type person struct {
		login, kyc, addr string
		hasAddr          bool
	}
	ids := make([]uuid.UUID, 0, len(res.Lines))
	for _, l := range res.Lines {
		ids = append(ids, l.UserID)
	}
	people := map[uuid.UUID]person{}
	rows, err := tx.Query(ctx, `
		SELECT u.id,
		       COALESCE(g.login, ''),
		       COALESCE(usr.kyc_status, ''),
		       COALESCE(a.address, ''),
		       a.address IS NOT NULL
		FROM unnest($1::uuid[]) AS u(id)
		LEFT JOIN users usr ON usr.id = u.id
		LEFT JOIN github_accounts g ON g.user_id = u.id
		LEFT JOIN contributor_addresses a
		       ON a.user_id = u.id AND a.chain_id = $2
		      AND a.chain_family = 'evm' AND a.superseded_at IS NULL`,
		ids, req.ChainID)
	if err != nil {
		return nil, nil, fmt.Errorf("keeperhubrail: join identities: %w", err)
	}
	for rows.Next() {
		var id uuid.UUID
		var p person
		if err := rows.Scan(&id, &p.login, &p.kyc, &p.addr, &p.hasAddr); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("keeperhubrail: scan identity: %w", err)
		}
		people[id] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var run runRow
	err = tx.QueryRow(ctx, `
		INSERT INTO keeperhub_payout_runs
		  (hackathon_id, pool, chain_id, pool_minor, hackathon_payout_run_id, released_by, state)
		VALUES ($1, $2, $3, $4::numeric, $5, $6, 'planned')
		RETURNING id, chain_id, hackathon_payout_run_id`,
		req.HackathonID, req.Pool, req.ChainID, res.PoolMinor.String(), req.PayoutRunID, req.ActorID,
	).Scan(&run.ID, &run.ChainID, &run.PayoutRunID)
	if err != nil {
		// The database-side half of the rail exclusion (migration 090300).
		if strings.Contains(err.Error(), "already has a settlement") {
			return nil, nil, fmt.Errorf("%w: %v", ErrSettledOnAptos, err)
		}
		return nil, nil, fmt.Errorf("keeperhubrail: insert run: %w", err)
	}

	total := new(big.Int)
	var exclusions []Exclusion
	for _, l := range res.Lines {
		if l.AmountMinor == nil || l.AmountMinor.Sign() <= 0 {
			// Rounded to nothing: no money to send and nothing withheld. The
			// settlement preview already shows these people; they are not legs.
			continue
		}
		if !l.AmountMinor.IsInt64() {
			return nil, nil, fmt.Errorf("%w: %s for %s", ErrAmountOutOfRange, l.AmountMinor, l.UserID)
		}
		total.Add(total, l.AmountMinor)
		p := people[l.UserID]

		// Same order as payout.Resolve: identity, then KYC, then address. KYC
		// is upstream of the address - registering one would not make an
		// unverified person payable.
		reason := ""
		addr := ""
		switch {
		case p.login == "":
			reason = ReasonNoGitHubAccount
		case p.kyc != "verified":
			reason = ReasonKYCUnresolved
		case !p.hasAddr:
			reason = ReasonNoAddress
		default:
			// Re-validated on the way out rather than trusted from storage: this
			// is the last point before the value is sent to a transfer. The EVM
			// validator never pads (#548).
			v, verr := payoutaddr.ValidateEVM(p.addr)
			if verr != nil {
				reason = ReasonNoAddress
			} else {
				addr = v
			}
		}

		if reason != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO keeperhub_payout_exclusions (run_id, user_id, amount_minor, reason)
				VALUES ($1, $2, $3, $4)`, run.ID, l.UserID, l.AmountMinor.Int64(), reason); err != nil {
				return nil, nil, fmt.Errorf("keeperhubrail: record exclusion: %w", err)
			}
			exclusions = append(exclusions, Exclusion{
				UserID: l.UserID, AmountMinor: l.AmountMinor.String(), Reason: reason,
			})
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO keeperhub_payout_legs (run_id, user_id, address, amount_minor)
			VALUES ($1, $2, $3, $4)`, run.ID, l.UserID, addr, l.AmountMinor.Int64()); err != nil {
			return nil, nil, fmt.Errorf("keeperhubrail: insert leg: %w", err)
		}
	}

	// Checked, not assumed. SettlementFor already asserts its lines sum to the
	// pool; this asserts that splitting them into legs and exclusions lost
	// nothing. A leg silently skipped here is money nobody is told about.
	if total.Cmp(res.PoolMinor) != 0 {
		return nil, nil, fmt.Errorf("%w: planned %s of %s minor units", ErrAllocationMismatch, total, res.PoolMinor)
	}
	return &run, exclusions, nil
}

// exclusionsFor reads a run's frozen exclusions.
func exclusionsFor(ctx context.Context, tx pgx.Tx, runID uuid.UUID) ([]Exclusion, error) {
	rows, err := tx.Query(ctx, `
		SELECT user_id, amount_minor::text, reason FROM keeperhub_payout_exclusions
		WHERE run_id = $1 ORDER BY user_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("keeperhubrail: load exclusions: %w", err)
	}
	defer rows.Close()
	var out []Exclusion
	for rows.Next() {
		var e Exclusion
		if err := rows.Scan(&e.UserID, &e.AmountMinor, &e.Reason); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
