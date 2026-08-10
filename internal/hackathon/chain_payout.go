package hackathon

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// On-chain spec §2.2: payout arithmetic is per chain.
//
//	unit_value(chain) = contributor_pool(chain) / total_units(chain)
//
// Each chain is financially its own event. A sponsor funds a chain, and that
// money pays that chain's contributors and nobody else's.

// ChainPayout is one chain's computed plan.
type ChainPayout struct {
	ChainID string      `json:"chain_id"`
	Pool    float64     `json:"contributor_pool"`
	Plan    *PayoutPlan `json:"plan"`
}

// ComputePayoutsPerChain divides each chain's pool over only that chain's
// accepted PRs.
//
// The diminishing-returns curve applies **within a chain** - a contributor's
// Nth accepted PR *on that chain*. Deliberately not global: a global curve
// would discount one sponsor's pool because of activity in a pool they have
// no relationship to, which contradicts the separate-pool model the whole
// design rests on.
//
// The consequence, which is published rather than hidden: working across two
// chains restarts the curve on each. That is a known property, not a
// loophole - the global slot limits from §4 of the AI spec still cap how much
// work one person can hold at once, regardless of how it is spread.
func ComputePayoutsPerChain(
	prsByChain map[string][]JudgedPR,
	poolByChain map[string]float64,
	cfg map[string]string,
) ([]ChainPayout, error) {
	chains := make([]string, 0, len(poolByChain))
	for c := range poolByChain {
		chains = append(chains, c)
	}
	// Sorted so a recomputation months later produces byte-identical output.
	sort.Strings(chains)

	out := make([]ChainPayout, 0, len(chains))
	for _, chainID := range chains {
		plan, err := ComputePayout(prsByChain[chainID], poolByChain[chainID], cfg)
		if err != nil {
			return nil, fmt.Errorf("hackathon.ComputePayoutsPerChain: %s: %w", chainID, err)
		}
		out = append(out, ChainPayout{ChainID: chainID, Pool: poolByChain[chainID], Plan: plan})
	}
	return out, nil
}

// LoadJudgedPRsByChain groups a hackathon's judged PRs by the chain their
// issue was tagged with.
//
// A verdict with no chain lands under "" and is handled by the off-chain
// path, which is the correct behaviour for an event that has no chain pools
// at all - the great majority today.
func LoadJudgedPRsByChain(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) (map[string][]JudgedPR, error) {
	rows, err := pool.Query(ctx, `
SELECT v.id::text, v.github_login, v.final_bucket,
       COALESCE(pr.merged_at_github, v.created_at),
       COALESCE(v.chain_id, hi.chain_id, '')
FROM hackathon_verdicts v
LEFT JOIN hackathon_issues hi ON hi.id = v.hackathon_issue_id
LEFT JOIN github_pull_requests pr
       ON pr.project_id = v.project_id AND pr.number = v.pr_number
WHERE v.hackathon_id = $1 AND v.final_bucket IS NOT NULL
ORDER BY v.id
`, hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.LoadJudgedPRsByChain: %w", err)
	}
	defer rows.Close()

	out := map[string][]JudgedPR{}
	for rows.Next() {
		var p JudgedPR
		var chainID string
		if err := rows.Scan(&p.VerdictID, &p.Login, &p.Bucket, &p.MergedAt, &chainID); err != nil {
			return nil, fmt.Errorf("hackathon.LoadJudgedPRsByChain: scan: %w", err)
		}
		out[chainID] = append(out[chainID], p)
	}
	return out, rows.Err()
}

// LoadChainPools reads each chain's contributor pool for an event, in whole
// units, from hackathon_chain_pools.
//
// Returns an empty map when the event runs no chains, which is what keeps the
// existing single-pool path unchanged for every event that is not on-chain.
func LoadChainPools(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) (map[string]float64, error) {
	rows, err := pool.Query(ctx, `
SELECT chain_id, (contributor_pool / (10 ^ asset_decimals))::float8
FROM hackathon_chain_pools
WHERE hackathon_id = $1
ORDER BY chain_id
`, hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.LoadChainPools: %w", err)
	}
	defer rows.Close()

	out := map[string]float64{}
	for rows.Next() {
		var chainID string
		var amount float64
		if err := rows.Scan(&chainID, &amount); err != nil {
			return nil, fmt.Errorf("hackathon.LoadChainPools: scan: %w", err)
		}
		out[chainID] = amount
	}
	return out, rows.Err()
}
