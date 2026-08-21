package payout

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/chainread"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// ChainClient is the chain read this package needs. An interface so a caller can
// substitute one, and so the no-cache property below is testable.
type ChainClient interface {
	ClaimDeadline(ctx context.Context, module, escrow string) (int64, error)
	IsClaimed(ctx context.Context, module, escrow, leafHex string) (bool, error)
}

// LiveReader answers chain questions for a settlement, every time, from the
// chain.
//
// # There is no cache here, and that is the feature
//
// A deadline reminder is scheduled against a deadline. `extend_deadline` is
// admin-gated, one-directional, and is THE DOCUMENTED REMEDY for a late
// claimant: when somebody loses a wallet, extending their window is how we help.
//
// So the cached case is not "slightly stale". It is: we extend somebody's
// deadline because they asked for help, and a reminder fires on the old date
// telling them they are about to lose money we have already given them more time
// to collect. The cache is wrong exactly when the remedy has been used, which is
// exactly when the person is paying attention.
//
// A reminder is also a message we send to a person. Being confidently wrong in
// one is worse than being late, and a chain read costs milliseconds against a
// message somebody will act on.
//
// **Do not add a TTL here.** If reminder volume ever makes the read cost matter,
// batch the settlements into one pass - the deadline is per event, not per
// person, so a thousand reminders for one settlement is still one read.
type LiveReader struct {
	pool    db.DBPool
	connect func(nodeURL string) ChainClient
}

// NewLiveReader builds a reader against real fullnodes.
func NewLiveReader(pool db.DBPool) *LiveReader {
	return &LiveReader{
		pool:    pool,
		connect: func(nodeURL string) ChainClient { return chainread.New(nodeURL) },
	}
}

// NewLiveReaderWith injects a client factory, for tests.
func NewLiveReaderWith(pool db.DBPool, connect func(string) ChainClient) *LiveReader {
	return &LiveReader{pool: pool, connect: connect}
}

type target struct {
	module string
	escrow string
}

// resolve finds the module and escrow for a settlement.
//
// The escrow address is READ FROM OUR OWN RECORD of the publish, never derived
// and never taken from a caller. `initialise` is ungated, so an escrow existing
// at some address proves nothing about whose it is; the only address we trust is
// the one `payout publish` recorded after verifying it holds our root.
func (r *LiveReader) resolve(ctx context.Context, settlementID uuid.UUID) (target, ChainClient, error) {
	var escrow, chainID string
	var publishedTx *string
	err := r.pool.QueryRow(ctx, `
		SELECT escrow_address, chain_id, published_tx
		FROM payout_event_roots WHERE settlement_id = $1`, settlementID).Scan(&escrow, &chainID, &publishedTx)
	if err != nil {
		return target{}, nil, fmt.Errorf("no published root for settlement %s: %w", settlementID, err)
	}
	if publishedTx == nil || escrow == "" {
		return target{}, nil, fmt.Errorf("settlement %s has no recorded publication; "+
			"there is no escrow to read and no deadline to remind against", settlementID)
	}
	cc, err := ChainConfigFor(ctx, r.pool, chainID)
	if err != nil {
		return target{}, nil, err
	}
	var ref *string
	_ = r.pool.QueryRow(ctx, `SELECT rpc_endpoint_ref FROM chain_configs WHERE chain_id=$1`, chainID).Scan(&ref)
	name := ""
	if ref != nil {
		name = *ref
	}
	nodeURL, err := chainread.EndpointFor(name)
	if err != nil {
		return target{}, nil, err
	}
	return target{module: cc.ContractAddress, escrow: escrow}, r.connect(nodeURL), nil
}

// Deadline reads the claim window's close, live, on every call.
func (r *LiveReader) Deadline(ctx context.Context, settlementID uuid.UUID) (time.Time, error) {
	t, client, err := r.resolve(ctx, settlementID)
	if err != nil {
		return time.Time{}, err
	}
	unix, err := client.ClaimDeadline(ctx, t.module, t.escrow)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(unix, 0).UTC(), nil
}

// Claimed reports whether one leaf has been claimed, live, on every call.
func (r *LiveReader) Claimed(ctx context.Context, settlementID uuid.UUID, leafHex string) (bool, error) {
	t, client, err := r.resolve(ctx, settlementID)
	if err != nil {
		return false, err
	}
	return client.IsClaimed(ctx, t.module, t.escrow, leafHex)
}
