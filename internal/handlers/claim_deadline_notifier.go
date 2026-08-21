package handlers

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/chainread"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// escrowReader is the three live reads this needs, narrowed for testing.
//
// An interface rather than *chainread.Client for the same reason
// sessionDecisionReader is one: the concrete client takes a node URL and cannot
// be pointed anywhere in a test without changing a type shared with the claim
// endpoints.
type escrowReader interface {
	ClaimDeadline(ctx context.Context, module, escrow string) (int64, error)
	IsClaimed(ctx context.Context, module, escrow, leafHex string) (bool, error)
	Balance(ctx context.Context, module, escrow string) (uint64, error)
}

// ClaimDeadlineNotifier tells contributors their claim window is closing, and
// tells the ones who missed it that their money is still there.
//
// # It reaches only people who are already visiting
//
// notifications.NotifyInApp sends no email, and users.email is non-null for
// zero accounts. So every message here is a persistent screen state with a bell
// count, not something that arrives - and the population this exists for, the
// ones slowest to set up a wallet, are by definition the least likely to be
// looking.
//
// That is a known and accepted limitation, not an oversight (#521). It is
// written here because a merged reminder feature is otherwise read as evidence
// that people were told, and for the first settlement they are being contacted
// by hand instead.
//
// # The deadline is never cached
//
// extend_deadline is the documented remedy for a late claimant, so a window can
// move at any moment. A reminder computed from a stored date tells somebody
// they have three days left after we extended them a month, which is worse than
// silence - and unlike the Claim button there is no contract behind a message
// to reject a wrong one. So the deadline is read live, per settlement, at send
// time, and a failed read sends nothing.
type ClaimDeadlineNotifier struct {
	db     *db.DB
	sink   SupportSink
	notify *notifications.Service
	cfg    config.Config

	// readerFor resolves a live reader per chain. A field so tests can supply
	// one without a node.
	readerFor func(chainID string) (escrowReader, string, error)

	interval time.Duration
}

func NewClaimDeadlineNotifier(cfg config.Config, d *db.DB, notify *notifications.Service) *ClaimDeadlineNotifier {
	n := &ClaimDeadlineNotifier{
		db:     d,
		sink:   newTelegramSupportSink(telegramSinkConfigFrom(cfg)),
		notify: notify,
		cfg:    cfg,
		// Hourly. The milestones are days apart, so a finer interval buys
		// nothing and spends somebody else's rate limit; an hour is well inside
		// the smallest gap and survives a restart without skipping one.
		interval: time.Hour,
	}
	n.readerFor = n.liveReaderFor
	return n
}

func (n *ClaimDeadlineNotifier) liveReaderFor(chainID string) (escrowReader, string, error) {
	var envRef, contract string
	if err := n.db.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(rpc_endpoint_ref,''), COALESCE(contract_address,'')
		 FROM chain_configs WHERE chain_id = $1`, chainID).Scan(&envRef, &contract); err != nil {
		return nil, "", fmt.Errorf("chain config for %s: %w", chainID, err)
	}
	nodeURL, err := chainread.EndpointFor(envRef)
	if err != nil {
		return nil, "", err
	}
	if contract == "" {
		return nil, "", fmt.Errorf("chain %s has no contract_address", chainID)
	}
	return chainread.New(nodeURL), contract, nil
}

// Run sends until the context is cancelled.
func (n *ClaimDeadlineNotifier) Run(ctx context.Context) {
	if n.db == nil || n.db.Pool == nil {
		slog.Warn("claim deadline notifier: no database, not starting")
		return
	}
	// Once at startup, for the reason the sweeper documents and the reconciler
	// had to be corrected to match: a window that closed while the process was
	// down is exactly what this exists to catch.
	n.sendOnce(ctx)

	t := time.NewTicker(n.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.sendOnce(ctx)
		}
	}
}

type deadlineCandidate struct {
	settlementID uuid.UUID
	chainID      string
	escrow       string
	userID       uuid.UUID
	leafHash     []byte
	amountMinor  int64
	assetSymbol  string
	decimals     int32
}

func (n *ClaimDeadlineNotifier) sendOnce(ctx context.Context) {
	// Every published leaf that belongs to somebody we can identify. The join
	// is the one /me/claims uses - address history, not identity_hash, which
	// needs no salt.
	rows, err := n.db.Pool.Query(ctx, `
		SELECT r.settlement_id, r.chain_id, r.escrow_address, ca.user_id,
		       l.leaf_hash, l.amount_minor
		FROM claim_leaves l
		JOIN payout_event_roots r ON r.settlement_id = l.settlement_id
		JOIN contributor_addresses ca
		  ON lower(ca.address) = lower(l.claim_address) AND ca.chain_id = r.chain_id
		WHERE r.published_tx IS NOT NULL AND r.escrow_address <> ''`)
	if err != nil {
		slog.Error("claim deadline notifier: query failed", "error", err)
		return
	}
	var work []deadlineCandidate
	for rows.Next() {
		var c deadlineCandidate
		if err := rows.Scan(&c.settlementID, &c.chainID, &c.escrow, &c.userID,
			&c.leafHash, &c.amountMinor); err != nil {
			rows.Close()
			slog.Error("claim deadline notifier: scan failed", "error", err)
			return
		}
		work = append(work, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("claim deadline notifier: read failed", "error", err)
		return
	}

	// One reader per chain, not per leaf.
	readers := map[string]escrowReader{}
	contracts := map[string]string{}
	deadlines := map[string]int64{}

	for _, c := range work {
		rd, ok := readers[c.chainID]
		if !ok {
			var contract string
			var err error
			rd, contract, err = n.readerFor(c.chainID)
			if err != nil {
				slog.Error("claim deadline notifier: no reader for chain, sending nothing",
					"chain_id", c.chainID, "error", err)
				readers[c.chainID] = nil
				continue
			}
			readers[c.chainID] = rd
			contracts[c.chainID] = contract
		}
		if rd == nil {
			continue
		}
		n.considerOne(ctx, rd, contracts[c.chainID], c, deadlines)
	}
}

func (n *ClaimDeadlineNotifier) considerOne(
	ctx context.Context, rd escrowReader, contract string,
	c deadlineCandidate, deadlines map[string]int64,
) {
	key := c.chainID + "|" + c.escrow
	deadline, ok := deadlines[key]
	if !ok {
		var err error
		deadline, err = rd.ClaimDeadline(ctx, contract, c.escrow)
		if err != nil {
			// Fail closed. A reminder computed from a deadline we could not
			// read is a reminder that may be wrong about the one fact it exists
			// to convey.
			slog.Error("claim deadline notifier: deadline unreadable, sending nothing",
				"escrow", c.escrow, "error", err)
			return
		}
		deadlines[key] = deadline
	}

	milestone, due := milestoneFor(deadline, time.Now())
	if !due {
		return
	}

	sent, err := n.alreadySent(ctx, c, milestone, deadline)
	if err != nil || sent {
		return
	}

	// Never remind somebody about money they already took.
	claimed, err := rd.IsClaimed(ctx, contract, c.escrow, "0x"+hex.EncodeToString(c.leafHash))
	if err != nil {
		slog.Error("claim deadline notifier: claim state unreadable, sending nothing",
			"escrow", c.escrow, "error", err)
		return
	}
	if claimed {
		return
	}

	if milestone == milestonePassed {
		// The only message whose truth depends on the sweep NOT having run.
		// After the deadline the admin may sweep at any time, so "your payout
		// is still sitting in the escrow" has to be established, not assumed.
		bal, err := rd.Balance(ctx, contract, c.escrow)
		if err != nil {
			slog.Error("claim deadline notifier: balance unreadable, sending nothing",
				"escrow", c.escrow, "error", err)
			return
		}
		switch {
		case bal == 0:
			// Swept, or every leaf claimed. Expected, and there is nothing to
			// tell anybody to come and collect.
			return
		case bal < uint64(c.amountMinor):
			// SHOULD NOT HAPPEN, which is exactly why it must not be a silent
			// skip. The escrow is funded to the leaf total and the sweep is
			// all-or-nothing, so a balance between zero and this person's
			// amount means funding was wrong, a sweep was partial, or something
			// we do not understand occurred - and in every one of those,
			// somebody is owed money that is not there.
			//
			// Sending nothing is right for the person: we cannot honestly tell
			// them their payout is waiting. Recording nothing would be wrong
			// for us. Same rule as an unrecognised Didit warning: an impossible
			// state that falls into an existing bucket is how you never find
			// out about it.
			slog.Error("claim deadline notifier: ESCROW SHORT OF A SINGLE UNCLAIMED LEAF",
				"escrow", c.escrow, "chain_id", c.chainID, "settlement_id", c.settlementID,
				"user_id", c.userID, "balance_minor", bal, "owed_minor", c.amountMinor)
			alertAdminOfEscrowShortfall(ctx, n.sink, c, bal)
			return
		}
	}

	dec, sym := c.decimals, c.assetSymbol
	if sym == "" {
		sym, dec = n.assetFor(ctx, c.chainID)
	}
	notice := noticeForDeadline(milestone,
		time.Unix(deadline, 0).UTC().Format("2 January 2006"),
		minorToDecimal(c.amountMinor, dec), sym)

	if n.notify != nil {
		res := n.notify.NotifyInApp(ctx, c.userID, notifications.TypeClaimDeadline,
			notice.Title, notice.Body, notifications.SettingsLink(notifications.SubtabPayout))
		if res.Err != nil {
			slog.Error("claim deadline notifier: delivery failed",
				"user_id", c.userID, "milestone", milestone, "error", res.Err)
			return
		}
	}
	n.recordSent(ctx, c, milestone, deadline)
}

// milestoneFor picks the most urgent milestone currently in window.
//
// The smallest threshold at or above the days remaining: at ten days out that
// is the fourteen-day message, at five it is the seven-day one. A job that
// starts late does not fire every missed milestone at once - it sends the one
// that is actually useful now, which is the most urgent applicable.
func milestoneFor(deadlineUnix int64, now time.Time) (claimDeadlineMilestone, bool) {
	if now.Unix() >= deadlineUnix {
		return milestonePassed, true
	}
	daysLeft := int((time.Unix(deadlineUnix, 0).Sub(now) + 24*time.Hour - time.Second) / (24 * time.Hour))
	best := claimDeadlineMilestone("")
	bestDays := 1 << 30
	for _, m := range claimDeadlineSchedule {
		if m.days >= daysLeft && m.days < bestDays {
			best, bestDays = m.milestone, m.days
		}
	}
	return best, best != ""
}

func (n *ClaimDeadlineNotifier) alreadySent(
	ctx context.Context, c deadlineCandidate, m claimDeadlineMilestone, deadline int64,
) (bool, error) {
	var exists bool
	err := n.db.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM claim_deadline_reminders
		  WHERE settlement_id=$1 AND user_id=$2 AND milestone=$3 AND deadline_unix=$4)`,
		c.settlementID, c.userID, string(m), deadline).Scan(&exists)
	if err != nil {
		slog.Error("claim deadline notifier: dedup read failed", "error", err)
	}
	return exists, err
}

func (n *ClaimDeadlineNotifier) recordSent(
	ctx context.Context, c deadlineCandidate, m claimDeadlineMilestone, deadline int64,
) {
	if _, err := n.db.Pool.Exec(ctx, `
		INSERT INTO claim_deadline_reminders (settlement_id, user_id, milestone, deadline_unix)
		VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		c.settlementID, c.userID, string(m), deadline); err != nil {
		slog.Error("claim deadline notifier: could not record a sent reminder; it may repeat",
			"user_id", c.userID, "milestone", m, "error", err)
	}
}

func (n *ClaimDeadlineNotifier) assetFor(ctx context.Context, chainID string) (string, int32) {
	var sym string
	var dec int32
	if err := n.db.Pool.QueryRow(ctx,
		`SELECT COALESCE(asset->>'symbol','USDC'), COALESCE((asset->>'decimals')::int, 6)
		 FROM chain_configs WHERE chain_id=$1`, chainID).Scan(&sym, &dec); err != nil {
		return "USDC", 6
	}
	return sym, dec
}

// alertAdminOfEscrowShortfall reports an escrow holding less than a single
// unclaimed leaf is owed.
//
// No claim table and no dedup, deliberately. Every other alert in this codebase
// is deduped because it fires on a state that stays true and would otherwise
// repeat; this one fires on a state that must never be true at all, and if it
// somehow persists then repeating hourly is the correct volume. An alert that
// goes quiet about money that is missing is the failure, not the noise.
//
// Category "kyc" is the admin DM route and never the public group. The naming
// is inherited rather than apt - it is the only DM-routed category the sink
// has - and this must not be posted publicly: it names an escrow, a settlement
// and an amount somebody is short.
func alertAdminOfEscrowShortfall(ctx context.Context, sink SupportSink, c deadlineCandidate, balance uint64) {
	if sink == nil || !sink.Configured() {
		slog.Error("escrow shortfall: no sink configured, nobody was told money is missing",
			"escrow", c.escrow, "settlement_id", c.settlementID, "user_id", c.userID)
		return
	}
	msg := fmt.Sprintf(
		"🚨 Escrow holds less than one unclaimed leaf is owed\n\n"+
			"Escrow: %s\nChain: %s\nSettlement: %s\nContributor: %s\n\n"+
			"Balance: %d minor units\nOwed to this unclaimed leaf alone: %d minor units\n\n"+
			"This state should not be reachable. The escrow is funded to the leaf total and "+
			"sweep_unclaimed is all-or-nothing, so a balance between zero and one leaf's amount "+
			"means funding was wrong, a sweep was partial, or something we do not understand "+
			"happened. Somebody is owed money that is not there.\n\n"+
			"No reminder was sent to the contributor: we cannot honestly tell them their payout "+
			"is waiting.",
		c.escrow, c.chainID, c.settlementID, c.userID, balance, c.amountMinor)

	if _, err := sink.Deliver(ctx, SupportRequest{
		ID: uuid.New(), Category: "kyc", Message: msg,
	}); err != nil {
		slog.Error("escrow shortfall: DELIVERY FAILED and money is missing",
			"escrow", c.escrow, "settlement_id", c.settlementID, "error", err)
	}
}
