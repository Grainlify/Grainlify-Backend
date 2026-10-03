package grainhack

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// What the bounty agent tells the backend: a payment it confirmed on chain,
// and winners it cannot pay because they have no Solana wallet linked. Both
// come in over GRAINHACK_STATEMENT_TOKEN and are checked against the signed
// statement they name before anyone is told anything.

var (
	ErrBadReport          = errors.New("grainhack: malformed report")
	ErrNotInStatement     = errors.New("grainhack: that winner is not in the statement")
	ErrLineNotPayable     = errors.New("grainhack: that line is not payable in the statement")
	ErrReportMismatch     = errors.New("grainhack: the report does not match the statement")
	ErrConflictingPayment = errors.New("grainhack: a different payment is already recorded for this winner")
	ErrTxAlreadyReported  = errors.New("grainhack: that transaction is already recorded for a different winner")
)

// PaymentReport is POST /grainhack/payments.
type PaymentReport struct {
	StatementID  string `json:"statement_id"`
	GitHubUserID int64  `json:"github_user_id"`
	AmountMinor  string `json:"amount_minor"`
	Currency     string `json:"currency"`
	Network      string `json:"network"`
	TxSignature  string `json:"tx_signature"`
	Recipient    string `json:"recipient"`
}

// PaymentResult is what the agent gets back.
type PaymentResult struct {
	Accepted  bool `json:"accepted"`
	Duplicate bool `json:"duplicate"`
	Notified  bool `json:"notified"`
}

func statementLine(is *Issued, ghID int64) (IssuedLine, bool) {
	for _, l := range is.Lines {
		if l.GitHubUserID == ghID {
			return l, true
		}
	}
	return IssuedLine{}, false
}

// ReportPayment records a confirmed payment and tells the winner, once.
//
// The same report twice (same winner, same transaction) is accepted as a
// duplicate and tells nobody again. A different transaction for a winner who
// already has one is refused: the signer pays a winner once, so that is a
// second payment, not a retry, and a person needs to look at it.
func (s *Service) ReportPayment(ctx context.Context, r PaymentReport) (*PaymentResult, error) {
	sid, err := uuid.Parse(strings.TrimSpace(r.StatementID))
	if err != nil {
		return nil, fmt.Errorf("%w: statement_id", ErrBadReport)
	}
	amount, ok := new(big.Int).SetString(strings.TrimSpace(r.AmountMinor), 10)
	if !ok || amount.Sign() <= 0 || amount.String() != r.AmountMinor {
		return nil, fmt.Errorf("%w: amount_minor must be a positive decimal integer string", ErrBadReport)
	}
	if r.GitHubUserID <= 0 {
		return nil, fmt.Errorf("%w: github_user_id", ErrBadReport)
	}
	if !isBase58Len(r.TxSignature, 64) {
		return nil, fmt.Errorf("%w: tx_signature must be a base58 Solana transaction signature", ErrBadReport)
	}
	if !isBase58Len(r.Recipient, 32) {
		return nil, fmt.Errorf("%w: recipient must be a base58 Solana address", ErrBadReport)
	}

	is, err := s.Get(ctx, sid)
	if err != nil {
		return nil, err
	}
	line, ok := statementLine(is, r.GitHubUserID)
	if !ok {
		return nil, fmt.Errorf("%w: github_user_id %d, statement %s", ErrNotInStatement, r.GitHubUserID, sid)
	}
	if line.Status != StatusPayable {
		return nil, fmt.Errorf("%w: github_user_id %d is %s in statement %s", ErrLineNotPayable, r.GitHubUserID, line.Status, sid)
	}
	if line.AmountMinor != amount.String() || r.Currency != is.Currency || r.Network != is.Network {
		return nil, fmt.Errorf("%w: statement %s has %s %s on %s for github_user_id %d",
			ErrReportMismatch, sid, line.AmountMinor, is.Currency, is.Network, r.GitHubUserID)
	}

	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO grainhack_payment_reports
		  (hackathon_id, pool, github_user_id, user_id, statement_id, amount_minor, currency, network, tx_signature, recipient)
		VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8, $9, $10)
		ON CONFLICT (hackathon_id, pool, github_user_id) DO NOTHING`,
		is.HackathonID, is.Pool, r.GitHubUserID, line.UserID, sid, amount.String(), is.Currency, is.Network,
		r.TxSignature, r.Recipient)
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return nil, fmt.Errorf("%w: %s", ErrTxAlreadyReported, r.TxSignature)
		}
		return nil, fmt.Errorf("grainhack: record payment: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var existingTx string
		err := s.Pool.QueryRow(ctx, `
			SELECT tx_signature FROM grainhack_payment_reports
			WHERE hackathon_id = $1 AND pool = $2 AND github_user_id = $3`,
			is.HackathonID, is.Pool, r.GitHubUserID).Scan(&existingTx)
		if err != nil {
			return nil, fmt.Errorf("grainhack: read recorded payment: %w", err)
		}
		if existingTx != r.TxSignature {
			return nil, fmt.Errorf("%w: github_user_id %d already has %s, reported %s",
				ErrConflictingPayment, r.GitHubUserID, existingTx, r.TxSignature)
		}
		return &PaymentResult{Accepted: true, Duplicate: true}, nil
	}

	name := s.hackathonName(ctx, is.HackathonID)
	devnet := ""
	if is.Network == NetworkSolanaDevnet {
		devnet = " This was a Solana devnet test payment, not real money."
	}
	body := fmt.Sprintf("%s for %s has been sent to your Solana wallet %s.%s Transaction: %s",
		FormatMinor(line.AmountMinor, is.Currency), name, shortAddress(r.Recipient), devnet, ExplorerTxURL(is.Network, r.TxSignature))
	notified := s.send(ctx, is, line, "paid", notifications.TypeGrainHackPaid,
		"Your GrainHack payout has been sent", body, notifications.BountyLedgerLink())
	return &PaymentResult{Accepted: true, Notified: notified}, nil
}

// WalletReport is POST /grainhack/awaiting-wallet: payable winners the agent
// found with no live Solana wallet link when it imported the statement.
type WalletReport struct {
	StatementID   string  `json:"statement_id"`
	GitHubUserIDs []int64 `json:"github_user_ids"`
}

// WalletSkip says why one winner was not asked.
type WalletSkip struct {
	GitHubUserID int64  `json:"github_user_id"`
	Reason       string `json:"reason"` // already_notified | held_kyc | already_paid | preference_off
}

// WalletResult lists who was asked and who was not.
type WalletResult struct {
	Notified []int64      `json:"notified"`
	Skipped  []WalletSkip `json:"skipped"`
}

// NotInStatementError names the ids a wallet report got wrong. Nothing is sent
// when there are any: a report that names somebody the statement does not is
// a bug on one side, and guessing past it is how notices reach the wrong
// people.
type NotInStatementError struct{ GitHubUserIDs []int64 }

func (e *NotInStatementError) Error() string {
	return fmt.Sprintf("refused: github_user_id(s) %v are not lines of the statement; nothing was sent", e.GitHubUserIDs)
}
func (e *NotInStatementError) Unwrap() error { return ErrNotInStatement }

// RemindLinkWallet asks each named payable winner, once per event pool, to
// link a Solana wallet.
func (s *Service) RemindLinkWallet(ctx context.Context, r WalletReport) (*WalletResult, error) {
	sid, err := uuid.Parse(strings.TrimSpace(r.StatementID))
	if err != nil {
		return nil, fmt.Errorf("%w: statement_id", ErrBadReport)
	}
	if len(r.GitHubUserIDs) == 0 || len(r.GitHubUserIDs) > 1000 {
		return nil, fmt.Errorf("%w: github_user_ids must list 1 to 1000 winners", ErrBadReport)
	}
	is, err := s.Get(ctx, sid)
	if err != nil {
		return nil, err
	}
	var unknown []int64
	for _, id := range r.GitHubUserIDs {
		if _, ok := statementLine(is, id); !ok {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return nil, &NotInStatementError{GitHubUserIDs: unknown}
	}

	name := s.hackathonName(ctx, is.HackathonID)
	out := &WalletResult{Notified: []int64{}, Skipped: []WalletSkip{}}
	seen := map[int64]bool{}
	for _, id := range r.GitHubUserIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		line, _ := statementLine(is, id)
		if line.Status != StatusPayable {
			out.Skipped = append(out.Skipped, WalletSkip{id, "held_kyc"})
			continue
		}
		var paid bool
		if err := s.Pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM grainhack_payment_reports WHERE hackathon_id = $1 AND pool = $2 AND github_user_id = $3)`,
			is.HackathonID, is.Pool, id).Scan(&paid); err != nil {
			return nil, fmt.Errorf("grainhack: paid check: %w", err)
		}
		if paid {
			out.Skipped = append(out.Skipped, WalletSkip{id, "already_paid"})
			continue
		}
		body := fmt.Sprintf(
			"Your share of the %s contributor pool is %s, paid in USDC on Solana. There is no Solana wallet linked to your account, "+
				"so it cannot be sent yet. Link one once at grainlify.com/bounties/link; the same wallet is used for Bounties.",
			name, FormatMinor(line.AmountMinor, is.Currency))
		if !s.claimNotice(ctx, is, line, "link_wallet") {
			out.Skipped = append(out.Skipped, WalletSkip{id, "already_notified"})
			continue
		}
		res := notifications.InAppResult{}
		if s.Notify != nil {
			res = s.Notify.NotifyInApp(ctx, line.UserID, notifications.TypeGrainHackLinkWallet,
				"Link a Solana wallet to receive your GrainHack payout", body, notifications.WalletLinkLink())
		}
		switch {
		case res.Created:
			out.Notified = append(out.Notified, id)
		case res.Suppressed:
			out.Skipped = append(out.Skipped, WalletSkip{id, "preference_off"})
		default:
			s.releaseNotice(ctx, is, line, "link_wallet")
			return nil, fmt.Errorf("grainhack: notify github_user_id %d: %v", id, res.Err)
		}
	}
	return out, nil
}

func (s *Service) hackathonName(ctx context.Context, hid uuid.UUID) string {
	var name string
	if err := s.Pool.QueryRow(ctx, `SELECT name FROM hackathons WHERE id = $1`, hid).Scan(&name); err != nil || name == "" {
		return "GrainHack"
	}
	return name
}

func shortAddress(a string) string {
	if len(a) <= 12 {
		return a
	}
	return a[:4] + "..." + a[len(a)-4:]
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// isBase58Len reports whether s is base58 that decodes to exactly n bytes: 32
// for an address, 64 for a transaction signature.
func isBase58Len(s string, n int) bool {
	if len(s) == 0 || len(s) > 2*n {
		return false
	}
	v := new(big.Int)
	for _, r := range s {
		i := strings.IndexRune(base58Alphabet, r)
		if i < 0 {
			return false
		}
		v.Mul(v, big.NewInt(58))
		v.Add(v, big.NewInt(int64(i)))
	}
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	return zeros+len(v.Bytes()) == n
}
