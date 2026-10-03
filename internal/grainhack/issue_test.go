package grainhack

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

func pgCode(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// GuardPayoutRelease is the precondition: every one of its refusals refuses
// the statement, and a refusal writes nothing.
func TestIssue_GuardPayoutReleaseComesFirst(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()

	unconfirmed := f.req()
	unconfirmed.Confirm = false
	if _, err := f.svc.Issue(ctx, unconfirmed); !errors.Is(err, hackathon.ErrPayoutNotReleasable) {
		t.Fatalf("unconfirmed: err = %v, want ErrPayoutNotReleasable", err)
	}
	noActor := f.req()
	noActor.ActorID = uuid.Nil
	if _, err := f.svc.Issue(ctx, noActor); !errors.Is(err, hackathon.ErrPayoutNotReleasable) {
		t.Fatalf("no actor: err = %v, want ErrPayoutNotReleasable", err)
	}

	f.d.Pool.Exec(ctx, `UPDATE hackathons SET appeals_closed_at = NULL WHERE id = $1`, f.hid)
	if _, err := f.svc.Issue(ctx, f.req()); !errors.Is(err, hackathon.ErrPayoutNotReleasable) {
		t.Fatalf("appeals open: err = %v, want ErrPayoutNotReleasable", err)
	}
	f.d.Pool.Exec(ctx, `UPDATE hackathons SET appeals_closed_at = now(), phase = 'closed' WHERE id = $1`, f.hid)
	if _, err := f.svc.Issue(ctx, f.req()); !errors.Is(err, hackathon.ErrPayoutNotReleasable) {
		t.Fatalf("not settled: err = %v, want ErrPayoutNotReleasable", err)
	}
	f.d.Pool.Exec(ctx, `UPDATE hackathons SET phase = 'settled' WHERE id = $1`, f.hid)
	// Shadow mode is the default; removing the override must refuse.
	f.d.Pool.Exec(ctx, `DELETE FROM hackathon_config_settings WHERE hackathon_id = $1`, f.hid)
	if _, err := f.svc.Issue(ctx, f.req()); !errors.Is(err, hackathon.ErrPayoutNotReleasable) {
		t.Fatalf("shadow mode: err = %v, want ErrPayoutNotReleasable", err)
	}

	if n := f.count(`SELECT count(*) FROM grainhack_results_statements WHERE hackathon_id = $1`, f.hid); n != 0 {
		t.Fatalf("a refused issue wrote %d statement(s)", n)
	}
}

func TestIssue_RefusesWithoutConfigurationOrForTheMaintainerPool(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	noKey := *f.svc
	noKey.Key = nil
	if _, err := noKey.Issue(ctx, f.req()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("no key: err = %v, want ErrNotConfigured", err)
	}
	r := f.req()
	r.Pool = PoolMaintainer
	if _, err := f.svc.Issue(ctx, r); !errors.Is(err, ErrUnsupportedPool) {
		t.Fatalf("maintainer: err = %v, want ErrUnsupportedPool", err)
	}
	other := uuid.New()
	r = f.req()
	r.PayoutRunID = &other
	if _, err := f.svc.Issue(ctx, r); !errors.Is(err, ErrPayoutRunNotCurrent) {
		t.Fatalf("stale run id: err = %v, want ErrPayoutRunNotCurrent", err)
	}
}

func TestNewService_Configuration(t *testing.T) {
	s, err := NewService(nil, "", "", nil)
	if err != nil || s.Key != nil || s.Network != NetworkSolanaDevnet {
		t.Fatalf("unset: %v %+v", err, s)
	}
	seed := "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	s, err = NewService(nil, seed, "solana-mainnet", nil)
	if err != nil || s.Key == nil || s.Network != NetworkSolanaMainnet {
		t.Fatalf("mainnet: %v %+v", err, s)
	}
	if s, err := NewService(nil, seed, "base-sepolia", nil); err == nil || s.Key != nil {
		t.Fatal("an unknown network left signing on")
	}
	if s, err := NewService(nil, "garbage", "", nil); err == nil || s.Key != nil {
		t.Fatal("a malformed key left signing on")
	}
}

// The statement: amounts are SettlementFor's, the unverified winner is held
// (not dropped), the lines sum to the pool, the JSON is canonical and signed,
// and the held winner - only - is told.
func TestIssue_WritesTheSignedStatement(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()

	is, err := f.svc.Issue(ctx, f.req())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if is.Supersedes != nil || is.ComputationID != f.payoutRun || is.Network != NetworkSolanaDevnet || is.PoolMinor != "10000000" {
		t.Fatalf("header wrong: %+v", is)
	}
	pub := ed25519.NewKeyFromSeed(goldenSeed()).Public().(ed25519.PublicKey)
	if !Verify(pub, is.Statement, is.Signature) {
		t.Fatal("stored signature does not verify against the stored statement")
	}
	if is.PublicKey != PublicKeyB64(f.svc.Key) || is.StatementSHA256 != SHA256Hex(is.Statement) {
		t.Fatal("public key or sha256 wrong")
	}

	var doc struct {
		V             int    `json:"v"`
		Kind          string `json:"kind"`
		StatementID   string `json:"statement_id"`
		Supersedes    *string
		HackathonID   string `json:"hackathon_id"`
		HackathonName string `json:"hackathon_name"`
		Pool          string `json:"pool"`
		ComputationID string `json:"computation_id"`
		Currency      string `json:"currency"`
		Network       string `json:"network"`
		PoolMinor     string `json:"pool_minor"`
		IssuedAt      string `json:"issued_at"`
		Lines         []struct {
			GitHubUserID int64  `json:"github_user_id"`
			Login        string `json:"login"`
			AmountMinor  string `json:"amount_minor"`
			Status       string `json:"status"`
		} `json:"lines"`
	}
	if err := json.Unmarshal([]byte(is.Statement), &doc); err != nil {
		t.Fatalf("statement is not JSON: %v", err)
	}
	if doc.V != 1 || doc.Kind != "grainhack_results" || doc.StatementID != is.StatementID.String() ||
		doc.HackathonID != f.hid.String() || doc.ComputationID != f.payoutRun.String() || doc.Pool != "contributor" ||
		doc.Currency != "USDC" || doc.Network != "solana-devnet" || doc.PoolMinor != "10000000" ||
		doc.IssuedAt != "2026-10-03T12:00:00Z" || !strings.HasPrefix(doc.HackathonName, "GrainHack test ") {
		t.Fatalf("statement fields wrong: %s", is.Statement)
	}
	if !strings.Contains(is.Statement, `"supersedes":null`) {
		t.Fatalf("supersedes is not null: %s", is.Statement)
	}

	want := map[int64]struct{ amount, status, login string }{}
	for i, p := range f.people {
		st := StatusPayable
		if i == 2 {
			st = StatusHeldKYC
		}
		want[p.githubUserID] = struct{ amount, status, login string }{
			[]string{"1000000", "2000000", "3000000", "4000000"}[i], st, p.login}
	}
	if len(doc.Lines) != 4 {
		t.Fatalf("%d lines, want 4 (a held winner must not be dropped)", len(doc.Lines))
	}
	var prev int64
	for _, l := range doc.Lines {
		if l.GitHubUserID <= prev {
			t.Fatal("lines not sorted by github_user_id")
		}
		prev = l.GitHubUserID
		w := want[l.GitHubUserID]
		if l.AmountMinor != w.amount || l.Status != w.status || l.Login != w.login {
			t.Errorf("line %d = %+v, want %+v", l.GitHubUserID, l, w)
		}
	}
	// The stored statement is exactly the canonical form of its own content.
	if !isCanonical(t, is.Statement) {
		t.Fatal("stored statement is not in canonical form")
	}

	held := f.people[2]
	if n := f.notificationsOf(held.userID, notifications.TypeGrainHackPayoutHeldKYC); n != 1 {
		t.Errorf("held winner got %d held notices, want 1", n)
	}
	for i, p := range f.people {
		if i != 2 && f.notificationsOf(p.userID, notifications.TypeGrainHackPayoutHeldKYC) != 0 {
			t.Errorf("payable winner %d was told they are held", i)
		}
	}
	latest, err := f.svc.Latest(ctx, f.hid, PoolContributor)
	if err != nil || latest.StatementID != is.StatementID {
		t.Fatalf("latest = %v, %v", latest, err)
	}
}

// isCanonical re-encodes the parsed statement with sorted keys and no
// whitespace and compares. encoding/json sorts map keys, and with HTML
// escaping off matches for the ASCII content these fixtures use.
func isCanonical(t *testing.T, s string) bool {
	t.Helper()
	var v any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(b.String(), "\n") == s
}

func TestIssue_StatementsAreImmutable(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	is, err := f.svc.Issue(ctx, f.req())
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE grainhack_results_statements SET network = 'solana-mainnet' WHERE id = $1`,
		`DELETE FROM grainhack_results_statements WHERE id = $1`,
		`UPDATE grainhack_results_statement_lines SET status = 'payable' WHERE statement_id = $1`,
		`UPDATE grainhack_results_statement_lines SET amount_minor = amount_minor + 1 WHERE statement_id = $1`,
		`DELETE FROM grainhack_results_statement_lines WHERE statement_id = $1`,
	} {
		_, err := f.d.Pool.Exec(ctx, sql, is.StatementID)
		if pgCode(err) != SQLStateImmutable {
			t.Errorf("%s: err = %v, want %s", sql, err, SQLStateImmutable)
		}
	}
	// A line added after issue breaks the sum and is refused at commit.
	extra := f.addWinner(1, "verified", true, 50)
	_, err = f.d.Pool.Exec(ctx, `
		INSERT INTO grainhack_results_statement_lines (statement_id, github_user_id, user_id, login, amount_minor, status)
		VALUES ($1, $2, $3, $4, 1, 'payable')`, is.StatementID, extra.githubUserID, extra.userID, extra.login)
	if pgCode(err) != SQLStateSumMismatch {
		t.Fatalf("late line: err = %v, want %s", err, SQLStateSumMismatch)
	}
	again, _ := f.svc.Get(ctx, is.StatementID)
	if again.Statement != is.Statement || len(again.Lines) != 4 {
		t.Fatal("the statement changed")
	}
}

func TestIssue_RefusesAndNamesAWinnerWithoutGitHub(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	orphan := f.addWinner(5, "verified", false, 60)

	_, err := f.svc.Issue(ctx, f.req())
	var missing *MissingGitHubError
	if !errors.As(err, &missing) || !errors.Is(err, ErrWinnersWithoutGitHub) {
		t.Fatalf("err = %v, want MissingGitHubError", err)
	}
	if len(missing.Winners) != 1 || missing.Winners[0].UserID != orphan.userID ||
		len(missing.Winners[0].Logins) != 1 || missing.Winners[0].Logins[0] != orphan.login {
		t.Fatalf("winners = %+v, want the orphan named by user id and verdict login", missing.Winners)
	}
	if !strings.Contains(err.Error(), orphan.login) {
		t.Fatalf("the refusal does not name them: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM grainhack_results_statements WHERE hackathon_id = $1`, f.hid); n != 0 {
		t.Fatalf("%d statement(s) written despite the refusal", n)
	}
}

// When a held winner's KYC clears, issuing again supersedes: same computation,
// same amounts, that line now payable. Nothing changed: refused.
func TestIssue_SupersedesWhenAHeldWinnerClearsKYC(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	first, err := f.svc.Issue(ctx, f.req())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Issue(ctx, f.req()); !errors.Is(err, ErrNothingToSupersede) {
		t.Fatalf("re-issue with nothing changed: err = %v, want ErrNothingToSupersede", err)
	}

	held := f.people[2]
	f.setKYC(held.userID, "verified")
	view, err := f.svc.AdminView(ctx, first)
	if err != nil || !view.SupersedeAvailable {
		t.Fatalf("admin view does not offer the supersede: %v %+v", err, view)
	}

	second, err := f.svc.Issue(ctx, f.req())
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if second.Supersedes == nil || *second.Supersedes != first.StatementID || second.ComputationID != first.ComputationID {
		t.Fatalf("second = %+v, want it to supersede %s on the same computation", second, first.StatementID)
	}
	if !strings.Contains(second.Statement, `"supersedes":"`+first.StatementID.String()+`"`) {
		t.Fatalf("canonical supersedes missing: %s", second.Statement)
	}
	for _, l := range second.Lines {
		if l.Status != StatusPayable {
			t.Errorf("line %d still %s", l.GitHubUserID, l.Status)
		}
	}
	for i := range first.Lines {
		if first.Lines[i].AmountMinor != second.Lines[i].AmountMinor {
			t.Errorf("amount moved on supersede: %s -> %s", first.Lines[i].AmountMinor, second.Lines[i].AmountMinor)
		}
	}
	chain, _ := f.svc.Chain(ctx, f.hid, PoolContributor)
	if len(chain) != 2 || chain[0] != first.StatementID || chain[1] != second.StatementID {
		t.Fatalf("chain = %v", chain)
	}
	latest, _ := f.svc.Latest(ctx, f.hid, PoolContributor)
	if latest.StatementID != second.StatementID {
		t.Fatal("latest is not the superseding statement")
	}
	if _, err := f.svc.Issue(ctx, f.req()); !errors.Is(err, ErrNothingToSupersede) {
		t.Fatalf("third issue: err = %v, want ErrNothingToSupersede", err)
	}
	// The old statement is untouched and still readable.
	old, err := f.svc.Get(ctx, first.StatementID)
	if err != nil || old.Statement != first.Statement {
		t.Fatal("the superseded statement changed")
	}
	// A superseding statement that does not name its predecessor's event
	// cannot be stored: the composite key refuses it.
	_, err = f.d.Pool.Exec(ctx, `
		INSERT INTO grainhack_results_statements
		  (id, supersedes, hackathon_id, pool, computation_id, currency, network, pool_minor,
		   canonical_json, signature, signing_public_key, issued_by, issued_at)
		VALUES ($1, $2, $3, 'contributor', $4, 'USDC', 'solana-mainnet', 1, 'x', 'x', 'x', $5, now())`,
		uuid.New(), second.StatementID, f.hid, f.payoutRun, f.admin)
	if pgCode(err) != "23503" {
		t.Fatalf("a supersede on another network: err = %v, want foreign_key_violation", err)
	}
}

// Amounts that moved, a new computation, or a different network are not a
// supersede: an admin decides what that means.
func TestIssue_RefusesASupersedeThatIsNotTheSameSettlement(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	if _, err := f.svc.Issue(ctx, f.req()); err != nil {
		t.Fatal(err)
	}
	f.setKYC(f.people[2].userID, "verified")

	mainnet := *f.svc
	mainnet.Network = NetworkSolanaMainnet
	if _, err := mainnet.Issue(ctx, f.req()); !errors.Is(err, ErrNetworkChanged) {
		t.Fatalf("network: err = %v, want ErrNetworkChanged", err)
	}

	f.d.Pool.Exec(ctx, `UPDATE hackathon_verdicts SET units = units + 1 WHERE hackathon_id = $1 AND user_id = $2`,
		f.hid, f.people[0].userID)
	if _, err := f.svc.Issue(ctx, f.req()); !errors.Is(err, ErrSettlementChanged) {
		t.Fatalf("amounts moved: err = %v, want ErrSettlementChanged", err)
	}

	f.d.Pool.Exec(ctx, `
		INSERT INTO hackathon_payout_runs (hackathon_id, contributor_prize_pool, total_units, unit_value, created_at)
		VALUES ($1, 10, 11, 1, now() + interval '1 second')`, f.hid)
	if _, err := f.svc.Issue(ctx, f.req()); !errors.Is(err, ErrComputationChanged) {
		t.Fatalf("new computation: err = %v, want ErrComputationChanged", err)
	}
	if n := f.count(`SELECT count(*) FROM grainhack_results_statements WHERE hackathon_id = $1`, f.hid); n != 1 {
		t.Fatalf("%d statements, want only the first", n)
	}
}

// A held notice reaches each held winner once, however many statements list
// them as held.
func TestIssue_HeldNoticeIsSentOnce(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	if _, err := f.svc.Issue(ctx, f.req()); err != nil {
		t.Fatal(err)
	}
	// Another winner is held now (KYC reset), so a supersede happens and lists
	// the original held winner as held again.
	f.setKYC(f.people[0].userID, "pending")
	if _, err := f.svc.Issue(ctx, f.req()); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if n := f.notificationsOf(f.people[2].userID, notifications.TypeGrainHackPayoutHeldKYC); n != 1 {
		t.Errorf("original held winner told %d times, want 1", n)
	}
	if n := f.notificationsOf(f.people[0].userID, notifications.TypeGrainHackPayoutHeldKYC); n != 1 {
		t.Errorf("newly held winner told %d times, want 1", n)
	}
}

func TestFormatMinor(t *testing.T) {
	for in, want := range map[string]string{
		"4000000": "4 USDC", "4500000": "4.5 USDC", "1": "0.000001 USDC", "0": "0 USDC", "123456789": "123.456789 USDC",
	} {
		if got := FormatMinor(in, "USDC"); got != want {
			t.Errorf("FormatMinor(%s) = %q, want %q", in, got, want)
		}
	}
}
