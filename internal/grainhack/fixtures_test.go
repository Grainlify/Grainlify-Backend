package grainhack

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

type person struct {
	userID       uuid.UUID
	githubUserID int64
	login        string
	units        int
}

type fx struct {
	t         *testing.T
	d         *db.DB
	svc       *Service
	hid       uuid.UUID
	payoutRun uuid.UUID
	admin     uuid.UUID
	project   uuid.UUID
	people    []person // in creation order: 1, 2, 3, 4 units
	users     []uuid.UUID
}

// resetGrainHack empties this package's tables. TRUNCATE, because the rows are
// immutable to DELETE by design.
func resetGrainHack(t *testing.T, d *db.DB) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), `
		TRUNCATE grainhack_notices, grainhack_payment_reports, grainhack_results_statement_lines,
		         grainhack_results_statements, grainhack_broadcast_notices`); err != nil {
		t.Fatalf("reset: %v", err)
	}
}

// fixture: a settled, non-shadow event with a 10 USDC contributor pool and
// four winners earning 1, 2, 3 and 4 units - so 1, 2, 3 and 4 USDC. The third
// is not KYC-verified; everyone else is.
func fixture(t *testing.T) *fx {
	t.Helper()
	d := dbtest.DB(t)
	resetGrainHack(t, d)
	ctx := context.Background()
	f := &fx{t: t, d: d}
	must := func(sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("fixture: %v\n  %.120s", err, sql)
		}
	}

	f.admin = uuid.New()
	must(`INSERT INTO users (id, role) VALUES ($1, 'admin')`, f.admin)
	f.users = append(f.users, f.admin)
	if err := d.Pool.QueryRow(ctx, `INSERT INTO projects (owner_user_id, github_full_name) VALUES ($1,$2) RETURNING id`,
		f.admin, "acme/gh-"+uuid.NewString()[:8]).Scan(&f.project); err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `
		INSERT INTO hackathons (name, contributor_prize_pool, phase, appeals_closed_at)
		VALUES ($1, 10, 'settled', now()) RETURNING id`, "GrainHack test "+uuid.NewString()[:8]).Scan(&f.hid); err != nil {
		t.Fatalf("hackathon: %v", err)
	}
	must(`INSERT INTO hackathon_config_settings (hackathon_id, key, value) VALUES ($1, 'judging_shadow_mode', 'false')`, f.hid)
	if err := d.Pool.QueryRow(ctx, `
		INSERT INTO hackathon_payout_runs (hackathon_id, contributor_prize_pool, total_units, unit_value)
		VALUES ($1, 10, 10, 1) RETURNING id`, f.hid).Scan(&f.payoutRun); err != nil {
		t.Fatalf("payout run: %v", err)
	}
	for i, units := range []int{1, 2, 3, 4} {
		kyc := "verified"
		if i == 2 {
			kyc = "pending"
		}
		f.people = append(f.people, f.addWinner(units, kyc, true, i+1))
	}

	key := ed25519.NewKeyFromSeed(goldenSeed())
	f.svc = &Service{Pool: d.Pool, Key: key, Network: NetworkSolanaDevnet,
		Notify: notifications.New(d, nil, ""), Now: func() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) }}

	t.Cleanup(func() {
		ctx := context.Background()
		resetGrainHack(t, d)
		d.Pool.Exec(ctx, `DELETE FROM keeperhub_payout_runs WHERE hackathon_id = $1`, f.hid)
		d.Pool.Exec(ctx, `DELETE FROM settlements WHERE hackathon_id = $1`, f.hid)
		d.Pool.Exec(ctx, `DELETE FROM hackathons WHERE id = $1`, f.hid)
		d.Pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, f.project)
		for _, u := range f.users {
			d.Pool.Exec(ctx, `DELETE FROM notifications WHERE user_id = $1`, u)
			d.Pool.Exec(ctx, `DELETE FROM contributor_addresses WHERE user_id = $1`, u)
			d.Pool.Exec(ctx, `DELETE FROM github_accounts WHERE user_id = $1`, u)
			d.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u)
		}
	})
	return f
}

// addWinner adds a verdict for a new person. withGitHub false leaves them
// without a GitHub account, recorded under a verdict login.
func (f *fx) addWinner(units int, kyc string, withGitHub bool, pr int) person {
	f.t.Helper()
	ctx := context.Background()
	p := person{userID: uuid.New(), units: units, login: "gh-" + uuid.NewString()[:8],
		githubUserID: 1_000_000 + int64(uuid.New().ID()%1_000_000_000)}
	if _, err := f.d.Pool.Exec(ctx, `INSERT INTO users (id, role, kyc_status) VALUES ($1, 'contributor', $2)`, p.userID, kyc); err != nil {
		f.t.Fatalf("user: %v", err)
	}
	f.users = append(f.users, p.userID)
	if withGitHub {
		if _, err := f.d.Pool.Exec(ctx, `INSERT INTO github_accounts (user_id, github_user_id, login, access_token)
		      VALUES ($1, $2, $3, '\x00')`, p.userID, p.githubUserID, p.login); err != nil {
			f.t.Fatalf("github: %v", err)
		}
	} else {
		p.githubUserID = 0
	}
	if _, err := f.d.Pool.Exec(ctx, `
		INSERT INTO hackathon_verdicts (hackathon_id, project_id, pr_number, user_id, github_login, final_bucket, units, curve_multiplier)
		VALUES ($1, $2, $3, $4, $5, 'accepted', $6, 1)`, f.hid, f.project, pr, p.userID, p.login, units); err != nil {
		f.t.Fatalf("verdict: %v", err)
	}
	return p
}

func (f *fx) req() IssueRequest {
	return IssueRequest{HackathonID: f.hid, Pool: PoolContributor, ActorID: f.admin, Confirm: true}
}

func (f *fx) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.d.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("count: %v\n  %s", err, sql)
	}
	return n
}

func (f *fx) notificationsOf(uid uuid.UUID, typ notifications.Type) int {
	return f.count(`SELECT count(*) FROM notifications WHERE user_id = $1 AND type = $2`, uid, string(typ))
}

func (f *fx) setKYC(uid uuid.UUID, status string) {
	f.t.Helper()
	if _, err := f.d.Pool.Exec(context.Background(), `UPDATE users SET kyc_status = $2 WHERE id = $1`, uid, status); err != nil {
		f.t.Fatal(err)
	}
}
