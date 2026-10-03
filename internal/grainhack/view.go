package grainhack

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AdminLine is one line as the admin screen shows it: the stored line, the
// person's KYC status now, and any payment the agent reported.
type AdminLine struct {
	IssuedLine
	KYCVerifiedNow  bool    `json:"kyc_verified_now"`
	PaidTxSignature *string `json:"paid_tx_signature"`
	PaidTxURL       *string `json:"paid_tx_url"`
}

// AdminView is GET/POST /admin/hackathons/:id/results-statement's answer.
type AdminView struct {
	*Issued
	Lines []AdminLine `json:"lines"`
	// Chain is every statement for the event pool, oldest first.
	Chain []uuid.UUID `json:"chain"`
	// SupersedeAvailable: some line's status no longer matches the person's
	// KYC now, so issuing again would produce a superseding statement.
	SupersedeAvailable bool       `json:"supersede_available"`
	CurrentPayoutRunID *uuid.UUID `json:"current_payout_run_id"`
}

// AdminView builds the admin screen's view of a statement.
func (s *Service) AdminView(ctx context.Context, is *Issued) (*AdminView, error) {
	chain, err := s.Chain(ctx, is.HackathonID, is.Pool)
	if err != nil {
		return nil, err
	}
	v := &AdminView{Issued: is, Chain: chain, Lines: make([]AdminLine, 0, len(is.Lines))}
	for _, l := range is.Lines {
		al := AdminLine{IssuedLine: l}
		var kyc string
		if err := s.Pool.QueryRow(ctx, `SELECT COALESCE(kyc_status, '') FROM users WHERE id = $1`, l.UserID).Scan(&kyc); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("grainhack: kyc now: %w", err)
		}
		al.KYCVerifiedNow = kyc == "verified"
		if (l.Status == StatusHeldKYC) == al.KYCVerifiedNow {
			v.SupersedeAvailable = true
		}
		var tx string
		err := s.Pool.QueryRow(ctx, `
			SELECT tx_signature FROM grainhack_payment_reports
			WHERE hackathon_id = $1 AND pool = $2 AND github_user_id = $3`,
			is.HackathonID, is.Pool, l.GitHubUserID).Scan(&tx)
		switch {
		case err == nil:
			u := ExplorerTxURL(is.Network, tx)
			al.PaidTxSignature, al.PaidTxURL = &tx, &u
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, fmt.Errorf("grainhack: payment for line: %w", err)
		}
		v.Lines = append(v.Lines, al)
	}
	v.CurrentPayoutRunID, err = CurrentPayoutRun(ctx, s.Pool, is.HackathonID)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// ExplorerTxURL links a Solana transaction on the explorer for its network.
func ExplorerTxURL(network, sig string) string {
	u := "https://explorer.solana.com/tx/" + sig
	if network == NetworkSolanaDevnet {
		u += "?cluster=devnet"
	}
	return u
}
