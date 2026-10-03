package grainhack

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// Base58 test values of the right decoded lengths: a 32-byte address and a
// 64-byte signature.
const (
	testRecipient = "DQNUbSmmakWcVKcdRXraNSgWdabZs5dMJyTVPFv21fNL"
	testTx        = "5VERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUW"
	otherTx       = "4VERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUW"
)

func (f *fx) issued() *Issued {
	f.t.Helper()
	is, err := f.svc.Issue(context.Background(), f.req())
	if err != nil {
		f.t.Fatalf("issue: %v", err)
	}
	return is
}

func (f *fx) report(is *Issued, p person, tx string) PaymentReport {
	amount := ""
	for _, l := range is.Lines {
		if l.GitHubUserID == p.githubUserID {
			amount = l.AmountMinor
		}
	}
	return PaymentReport{
		StatementID: is.StatementID.String(), GitHubUserID: p.githubUserID, AmountMinor: amount,
		Currency: "USDC", Network: is.Network, TxSignature: tx, Recipient: testRecipient,
	}
}

func TestTestValuesAreWellFormed(t *testing.T) {
	if !isBase58Len(testTx, 64) || !isBase58Len(otherTx, 64) || !isBase58Len(testRecipient, 32) {
		t.Fatal("test fixtures are not the lengths they claim")
	}
}

func TestReportPayment_RecordsOnceAndTellsTheWinner(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	is := f.issued()
	winner := f.people[0]

	res, err := f.svc.ReportPayment(ctx, f.report(is, winner, testTx))
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if !res.Accepted || res.Duplicate || !res.Notified {
		t.Fatalf("res = %+v", res)
	}
	var body, link string
	f.d.Pool.QueryRow(ctx, `SELECT body, link_path FROM notifications WHERE user_id = $1 AND type = $2`,
		winner.userID, string(notifications.TypeGrainHackPaid)).Scan(&body, &link)
	for _, want := range []string{"1 USDC", "explorer.solana.com/tx/" + testTx + "?cluster=devnet", "devnet test payment"} {
		if !strings.Contains(body, want) {
			t.Errorf("paid notice %q lacks %q", body, want)
		}
	}
	if link != "/bounties/ledger" {
		t.Errorf("link = %q", link)
	}

	// The same report again: accepted as a duplicate, nobody told twice.
	res, err = f.svc.ReportPayment(ctx, f.report(is, winner, testTx))
	if err != nil || !res.Duplicate || res.Notified {
		t.Fatalf("duplicate: %+v %v", res, err)
	}
	if n := f.notificationsOf(winner.userID, notifications.TypeGrainHackPaid); n != 1 {
		t.Fatalf("%d paid notices, want 1", n)
	}

	// A different transaction for the same winner is a second payment.
	if _, err := f.svc.ReportPayment(ctx, f.report(is, winner, otherTx)); !errors.Is(err, ErrConflictingPayment) {
		t.Fatalf("second tx: err = %v, want ErrConflictingPayment", err)
	}
	// One transaction cannot pay two winners.
	if _, err := f.svc.ReportPayment(ctx, f.report(is, f.people[1], testTx)); !errors.Is(err, ErrTxAlreadyReported) {
		t.Fatalf("reused tx: err = %v, want ErrTxAlreadyReported", err)
	}
	view, err := f.svc.AdminView(ctx, is)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range view.Lines {
		paid := l.PaidTxSignature != nil
		if paid != (l.GitHubUserID == winner.githubUserID) {
			t.Errorf("admin view paid=%v for %d", paid, l.GitHubUserID)
		}
	}
}

func TestReportPayment_RefusesWhatTheStatementDoesNotSay(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	is := f.issued()
	payable, held := f.people[0], f.people[2]

	cases := map[string]struct {
		mutate func(r *PaymentReport)
		want   error
	}{
		"held line":        {func(r *PaymentReport) { *r = f.report(is, held, testTx) }, ErrLineNotPayable},
		"wrong amount":     {func(r *PaymentReport) { r.AmountMinor = "1000001" }, ErrReportMismatch},
		"wrong network":    {func(r *PaymentReport) { r.Network = NetworkSolanaMainnet }, ErrReportMismatch},
		"wrong currency":   {func(r *PaymentReport) { r.Currency = "ANSEM" }, ErrReportMismatch},
		"not a winner":     {func(r *PaymentReport) { r.GitHubUserID = 42 }, ErrNotInStatement},
		"no statement":     {func(r *PaymentReport) { r.StatementID = uuid.NewString() }, ErrNotFound},
		"bad statement id": {func(r *PaymentReport) { r.StatementID = "x" }, ErrBadReport},
		"amount not int":   {func(r *PaymentReport) { r.AmountMinor = "1.5" }, ErrBadReport},
		"amount padded":    {func(r *PaymentReport) { r.AmountMinor = "01000000" }, ErrBadReport},
		"bad tx":           {func(r *PaymentReport) { r.TxSignature = testRecipient }, ErrBadReport},
		"bad recipient":    {func(r *PaymentReport) { r.Recipient = "0xabc" }, ErrBadReport},
	}
	for name, c := range cases {
		r := f.report(is, payable, testTx)
		c.mutate(&r)
		if _, err := f.svc.ReportPayment(ctx, r); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	if n := f.count(`SELECT count(*) FROM grainhack_payment_reports WHERE hackathon_id = $1`, f.hid); n != 0 {
		t.Fatalf("%d payment(s) recorded from refused reports", n)
	}
}

// A payment approved under a statement that has since been superseded is
// still a payment of that statement's line, and is recorded.
func TestReportPayment_AgainstASupersededStatement(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	first := f.issued()
	f.setKYC(f.people[2].userID, "verified")
	if _, err := f.svc.Issue(ctx, f.req()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ReportPayment(ctx, f.report(first, f.people[0], testTx)); err != nil {
		t.Fatalf("report against the superseded statement: %v", err)
	}
}

func TestRemindLinkWallet(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	is := f.issued()
	a, b, held, d := f.people[0], f.people[1], f.people[2], f.people[3]
	if _, err := f.svc.ReportPayment(ctx, f.report(is, d, testTx)); err != nil {
		t.Fatal(err)
	}

	// Somebody the statement does not list: refused whole, nobody told.
	_, err := f.svc.RemindLinkWallet(ctx, WalletReport{StatementID: is.StatementID.String(),
		GitHubUserIDs: []int64{a.githubUserID, 42}})
	var notIn *NotInStatementError
	if !errors.As(err, &notIn) || len(notIn.GitHubUserIDs) != 1 || notIn.GitHubUserIDs[0] != 42 {
		t.Fatalf("err = %v, want NotInStatementError naming 42", err)
	}
	if f.notificationsOf(a.userID, notifications.TypeGrainHackLinkWallet) != 0 {
		t.Fatal("a refused report still notified somebody")
	}

	res, err := f.svc.RemindLinkWallet(ctx, WalletReport{StatementID: is.StatementID.String(),
		GitHubUserIDs: []int64{a.githubUserID, b.githubUserID, held.githubUserID, d.githubUserID, a.githubUserID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notified) != 2 {
		t.Fatalf("notified = %v, want a and b", res.Notified)
	}
	reasons := map[int64]string{}
	for _, s := range res.Skipped {
		reasons[s.GitHubUserID] = s.Reason
	}
	if reasons[held.githubUserID] != "held_kyc" || reasons[d.githubUserID] != "already_paid" {
		t.Fatalf("skipped = %+v", res.Skipped)
	}
	var link string
	f.d.Pool.QueryRow(ctx, `SELECT link_path FROM notifications WHERE user_id = $1 AND type = $2`,
		a.userID, string(notifications.TypeGrainHackLinkWallet)).Scan(&link)
	if link != "/bounties/link" {
		t.Fatalf("link = %q", link)
	}

	// Reported again on the next import: nobody is asked twice.
	res, err = f.svc.RemindLinkWallet(ctx, WalletReport{StatementID: is.StatementID.String(), GitHubUserIDs: []int64{a.githubUserID}})
	if err != nil || len(res.Notified) != 0 || len(res.Skipped) != 1 || res.Skipped[0].Reason != "already_notified" {
		t.Fatalf("second report: %+v %v", res, err)
	}
	if n := f.notificationsOf(a.userID, notifications.TypeGrainHackLinkWallet); n != 1 {
		t.Fatalf("%d link-wallet notices, want 1", n)
	}

	if _, err := f.svc.RemindLinkWallet(ctx, WalletReport{StatementID: is.StatementID.String()}); !errors.Is(err, ErrBadReport) {
		t.Fatalf("empty list: err = %v", err)
	}
}

// A winner who switched the notice type off is not asked, is reported as
// such, and the claim is kept (it is their choice, not a failure to retry).
func TestRemindLinkWallet_HonoursThePreference(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	is := f.issued()
	a := f.people[0]
	if _, err := f.d.Pool.Exec(ctx, `INSERT INTO notification_preferences (user_id, type, in_app, email) VALUES ($1, $2, false, false)`,
		a.userID, string(notifications.TypeGrainHackLinkWallet)); err != nil {
		t.Fatal(err)
	}
	defer f.d.Pool.Exec(ctx, `DELETE FROM notification_preferences WHERE user_id = $1`, a.userID)
	res, err := f.svc.RemindLinkWallet(ctx, WalletReport{StatementID: is.StatementID.String(), GitHubUserIDs: []int64{a.githubUserID}})
	if err != nil || len(res.Skipped) != 1 || res.Skipped[0].Reason != "preference_off" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestBase58Lengths(t *testing.T) {
	if !isBase58Len("11111111111111111111111111111111", 32) {
		t.Error("system program address refused")
	}
	if !isBase58Len("DQNUbSmmakWcVKcdRXraNSgWdabZs5dMJyTVPFv21fNL", 32) {
		t.Error("devnet mint refused")
	}
	if isBase58Len("0OIl", 32) || isBase58Len("", 32) {
		t.Error("non-base58 accepted")
	}
	if isBase58Len("DQNUbSmmakWcVKcdRXraNSgWdabZs5dMJyTVPFv21fNL", 64) {
		t.Error("an address accepted as a signature")
	}
}
