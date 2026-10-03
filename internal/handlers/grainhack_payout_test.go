package handlers_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/grainhack"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

const ghAgentToken = "grainhack-agent-test-token"

// The routes as api.go mounts them: admin behind JWT + live role, the agent's
// behind the bearer the handler checks.
func grainhackApp(t *testing.T, d *db.DB, h *handlers.GrainHackPayoutHandler) *fiber.App {
	t.Helper()
	app := fiber.New()
	app.Get("/grainhack/results-key", h.PublicKey)
	app.Get("/grainhack/results-statements/:statement_id", h.AgentGetStatement)
	app.Get("/grainhack/hackathons/:hackathon_id/results-statement", h.AgentLatestStatement)
	app.Post("/grainhack/payments", h.AgentReportPayment)
	app.Post("/grainhack/awaiting-wallet", h.AgentAwaitingWallet)
	requireAdmin := auth.RequireLiveRole(handlers.NewRoleLookup(d), "admin")
	admin := app.Group("/admin", auth.RequireAuth(adminSuiteJWTSecret))
	admin.Get("/hackathons/:id/results-statement", requireAdmin, h.LatestStatement())
	admin.Post("/hackathons/:id/results-statement", requireAdmin, h.IssueStatement())
	admin.Get("/grainhack/base-sepolia-notice", requireAdmin, h.BaseSepoliaNoticePreview())
	admin.Post("/grainhack/base-sepolia-notice", requireAdmin, h.BaseSepoliaNoticeSend())
	return app
}

type ghEvent struct {
	hid, payoutRun uuid.UUID
	winners        []uuid.UUID
	ghIDs          []int64
}

// A settled, non-shadow event with a 3 USDC pool: two verified winners with
// 1 and 2 units.
func ghEventFixture(t *testing.T, d *db.DB) *ghEvent {
	t.Helper()
	ctx := context.Background()
	reset := func() {
		d.Pool.Exec(context.Background(), `TRUNCATE grainhack_notices, grainhack_payment_reports,
			grainhack_results_statement_lines, grainhack_results_statements, grainhack_broadcast_notices`)
	}
	reset()
	e := &ghEvent{}
	owner := adminSuiteInsertUser(t, d, "contributor")
	var project uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO projects (owner_user_id, github_full_name) VALUES ($1,$2) RETURNING id`,
		owner, "acme/ghh-"+uuid.NewString()[:8]).Scan(&project); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool.QueryRow(ctx, `INSERT INTO hackathons (name, contributor_prize_pool, phase, appeals_closed_at)
		VALUES ($1, 3, 'settled', now()) RETURNING id`, "ghh-"+uuid.NewString()[:8]).Scan(&e.hid); err != nil {
		t.Fatal(err)
	}
	d.Pool.Exec(ctx, `INSERT INTO hackathon_config_settings (hackathon_id, key, value) VALUES ($1, 'judging_shadow_mode', 'false')`, e.hid)
	if err := d.Pool.QueryRow(ctx, `INSERT INTO hackathon_payout_runs (hackathon_id, contributor_prize_pool, total_units, unit_value)
		VALUES ($1, 3, 3, 1) RETURNING id`, e.hid).Scan(&e.payoutRun); err != nil {
		t.Fatal(err)
	}
	for i, units := range []int{1, 2} {
		uid := uuid.New()
		gh := 7_000_000_000 + int64(uuid.New().ID()%1_000_000_000)
		login := "ghh-" + uuid.NewString()[:8]
		d.Pool.Exec(ctx, `INSERT INTO users (id, role, kyc_status) VALUES ($1, 'contributor', 'verified')`, uid)
		d.Pool.Exec(ctx, `INSERT INTO github_accounts (user_id, github_user_id, login, access_token) VALUES ($1, $2, $3, '\x00')`, uid, gh, login)
		if _, err := d.Pool.Exec(ctx, `INSERT INTO hackathon_verdicts (hackathon_id, project_id, pr_number, user_id, github_login, final_bucket, units, curve_multiplier)
			VALUES ($1, $2, $3, $4, $5, 'accepted', $6, 1)`, e.hid, project, i+1, uid, login, units); err != nil {
			t.Fatal(err)
		}
		e.winners = append(e.winners, uid)
		e.ghIDs = append(e.ghIDs, gh)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		reset()
		d.Pool.Exec(ctx, `DELETE FROM hackathons WHERE id = $1`, e.hid)
		d.Pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, project)
		for _, u := range append(e.winners, owner) {
			d.Pool.Exec(ctx, `DELETE FROM notifications WHERE user_id = $1`, u)
			d.Pool.Exec(ctx, `DELETE FROM github_accounts WHERE user_id = $1`, u)
			d.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u)
		}
	})
	return e
}

func ghService(d *db.DB, withKey bool) *grainhack.Service {
	s := &grainhack.Service{Pool: d.Pool, Network: grainhack.NetworkSolanaDevnet,
		Notify: notifications.New(d, nil, ""), Now: time.Now}
	if withKey {
		seed := make([]byte, ed25519.SeedSize)
		seed[0] = 7
		s.Key = ed25519.NewKeyFromSeed(seed)
	}
	return s
}

func agentDo(t *testing.T, app *fiber.App, method, path, bearer string, body any) (int, map[string]any) {
	t.Helper()
	return adminSuiteDo(t, app, method, path, bearer, body)
}

func TestGrainHackStatement_AdminIssueAndAgentFetch(t *testing.T) {
	d := dbtest.DB(t)
	e := ghEventFixture(t, d)
	svc := ghService(d, true)
	app := grainhackApp(t, d, handlers.NewGrainHackPayoutHandlerWith(svc, ghAgentToken))
	admin := adminSuiteInsertUser(t, d, "admin")
	contributor := adminSuiteInsertUser(t, d, "contributor")
	path := "/admin/hackathons/" + e.hid.String() + "/results-statement"

	if code, _ := adminSuiteDo(t, app, "POST", path, adminSuiteToken(t, contributor, "contributor"), map[string]any{"confirm": true}); code != fiber.StatusForbidden {
		t.Fatalf("non-admin issue got %d, want 403", code)
	}
	if code, _ := adminSuiteDo(t, app, "POST", path, "", map[string]any{"confirm": true}); code != fiber.StatusUnauthorized {
		t.Fatalf("anonymous issue got %d, want 401", code)
	}

	code, body := adminSuiteDo(t, app, "GET", path, adminSuiteToken(t, admin, "admin"), nil)
	if code != fiber.StatusNotFound || body["current_payout_run_id"] != e.payoutRun.String() {
		t.Fatalf("before issue: %d %v, want 404 with the current computation", code, body)
	}

	code, body = adminSuiteDo(t, app, "POST", path, adminSuiteToken(t, admin, "admin"), map[string]any{"confirm": false})
	if code != fiber.StatusConflict || body["error"] != "payout_not_releasable" {
		t.Fatalf("unconfirmed: %d %v", code, body)
	}
	code, body = adminSuiteDo(t, app, "POST", path, adminSuiteToken(t, admin, "admin"),
		map[string]any{"confirm": true, "payout_run_id": uuid.NewString()})
	if code != fiber.StatusConflict || body["error"] != "payout_run_not_current" {
		t.Fatalf("stale computation: %d %v", code, body)
	}

	code, body = adminSuiteDo(t, app, "POST", path, adminSuiteToken(t, admin, "admin"),
		map[string]any{"confirm": true, "payout_run_id": e.payoutRun.String()})
	if code != fiber.StatusCreated {
		t.Fatalf("issue: %d %v", code, body)
	}
	sid, _ := body["statement_id"].(string)
	statement, _ := body["statement"].(string)
	signature, _ := body["signature"].(string)
	if sid == "" || statement == "" || signature == "" || body["supersedes"] != nil {
		t.Fatalf("issue body: %v", body)
	}
	if lines, _ := body["lines"].([]any); len(lines) != 2 {
		t.Fatalf("lines: %v", body["lines"])
	}
	if !grainhack.Verify(svc.Key.Public().(ed25519.PublicKey), statement, signature) {
		t.Fatal("signature in the admin answer does not verify")
	}

	code, body = adminSuiteDo(t, app, "POST", path, adminSuiteToken(t, admin, "admin"), map[string]any{"confirm": true})
	if code != fiber.StatusConflict || body["error"] != "nothing_to_supersede" {
		t.Fatalf("re-issue: %d %v", code, body)
	}
	code, body = adminSuiteDo(t, app, "GET", path, adminSuiteToken(t, admin, "admin"), nil)
	if code != fiber.StatusOK || body["statement_id"] != sid || body["supersede_available"] != false {
		t.Fatalf("latest: %d %v", code, body)
	}

	// The agent: bearer required, and exactly {statement, signature}.
	agentPath := "/grainhack/results-statements/" + sid
	if code, _ := agentDo(t, app, "GET", agentPath, "", nil); code != fiber.StatusUnauthorized {
		t.Fatalf("no bearer got %d", code)
	}
	if code, _ := agentDo(t, app, "GET", agentPath, "wrong", nil); code != fiber.StatusUnauthorized {
		t.Fatalf("wrong bearer got %d", code)
	}
	// A user's session JWT is not the agent's token.
	if code, _ := agentDo(t, app, "GET", agentPath, adminSuiteToken(t, admin, "admin"), nil); code != fiber.StatusUnauthorized {
		t.Fatalf("an admin session got %d on the agent route", code)
	}
	code, body = agentDo(t, app, "GET", agentPath, ghAgentToken, nil)
	if code != fiber.StatusOK || len(body) != 2 || body["statement"] != statement || body["signature"] != signature {
		t.Fatalf("agent fetch: %d %v", code, body)
	}
	code, body = agentDo(t, app, "GET", "/grainhack/hackathons/"+e.hid.String()+"/results-statement", ghAgentToken, nil)
	if code != fiber.StatusOK || body["statement"] != statement {
		t.Fatalf("agent latest: %d %v", code, body)
	}
	if code, _ := agentDo(t, app, "GET", "/grainhack/results-statements/"+uuid.NewString(), ghAgentToken, nil); code != fiber.StatusNotFound {
		t.Fatalf("unknown statement got %d", code)
	}

	// Payment report and wallet report.
	is, _ := svc.Get(context.Background(), uuid.MustParse(sid))
	report := map[string]any{
		"statement_id": sid, "github_user_id": is.Lines[0].GitHubUserID, "amount_minor": is.Lines[0].AmountMinor,
		"currency": "USDC", "network": "solana-devnet",
		"tx_signature": "5VERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUW",
		"recipient":    "DQNUbSmmakWcVKcdRXraNSgWdabZs5dMJyTVPFv21fNL",
	}
	if code, _ := agentDo(t, app, "POST", "/grainhack/payments", "", report); code != fiber.StatusUnauthorized {
		t.Fatalf("unauthenticated report got %d", code)
	}
	code, body = agentDo(t, app, "POST", "/grainhack/payments", ghAgentToken, report)
	if code != fiber.StatusOK || body["accepted"] != true || body["notified"] != true || body["duplicate"] != false {
		t.Fatalf("report: %d %v", code, body)
	}
	code, body = agentDo(t, app, "POST", "/grainhack/payments", ghAgentToken, report)
	if code != fiber.StatusOK || body["duplicate"] != true {
		t.Fatalf("duplicate report: %d %v", code, body)
	}
	report["amount_minor"] = "1"
	report["github_user_id"] = is.Lines[1].GitHubUserID
	code, body = agentDo(t, app, "POST", "/grainhack/payments", ghAgentToken, report)
	if code != fiber.StatusConflict || body["error"] != "report_mismatch" {
		t.Fatalf("mismatched report: %d %v", code, body)
	}

	code, body = agentDo(t, app, "POST", "/grainhack/awaiting-wallet", ghAgentToken,
		map[string]any{"statement_id": sid, "github_user_ids": []int64{42}})
	if code != fiber.StatusBadRequest || body["error"] != "not_in_statement" {
		t.Fatalf("unknown winner: %d %v", code, body)
	}
	code, body = agentDo(t, app, "POST", "/grainhack/awaiting-wallet", ghAgentToken,
		map[string]any{"statement_id": sid, "github_user_ids": []int64{is.Lines[1].GitHubUserID}})
	if notified, _ := body["notified"].([]any); code != fiber.StatusOK || len(notified) != 1 {
		t.Fatalf("awaiting wallet: %d %v", code, body)
	}

	code, body = agentDo(t, app, "GET", "/grainhack/results-key", "", nil)
	want := base64.StdEncoding.EncodeToString(svc.Key.Public().(ed25519.PublicKey))
	if code != fiber.StatusOK || body["public_key"] != want || body["domain"] != grainhack.SignatureDomain {
		t.Fatalf("results key: %d %v", code, body)
	}
}

func TestGrainHackStatement_NamesWinnersWithoutGitHub(t *testing.T) {
	d := dbtest.DB(t)
	e := ghEventFixture(t, d)
	app := grainhackApp(t, d, handlers.NewGrainHackPayoutHandlerWith(ghService(d, true), ghAgentToken))
	admin := adminSuiteInsertUser(t, d, "admin")
	d.Pool.Exec(context.Background(), `DELETE FROM github_accounts WHERE user_id = $1`, e.winners[0])

	code, body := adminSuiteDo(t, app, "POST", "/admin/hackathons/"+e.hid.String()+"/results-statement",
		adminSuiteToken(t, admin, "admin"), map[string]any{"confirm": true})
	winners, _ := body["winners"].([]any)
	if code != fiber.StatusConflict || body["error"] != "winners_without_github" || len(winners) != 1 {
		t.Fatalf("got %d %v", code, body)
	}
	if w, _ := winners[0].(map[string]any); w["user_id"] != e.winners[0].String() {
		t.Fatalf("named %v, want %s", winners[0], e.winners[0])
	}
}

// Unset configuration: the server still builds the handler, issuing and the
// key answer 503, the agent routes refuse without a token, reads still work.
func TestGrainHackStatement_UnconfiguredAnswers503(t *testing.T) {
	d := dbtest.DB(t)
	e := ghEventFixture(t, d)
	h := handlers.NewGrainHackPayoutHandler(d, config.Config{}, nil)
	app := grainhackApp(t, d, h)
	admin := adminSuiteInsertUser(t, d, "admin")
	path := "/admin/hackathons/" + e.hid.String() + "/results-statement"

	code, body := adminSuiteDo(t, app, "POST", path, adminSuiteToken(t, admin, "admin"), map[string]any{"confirm": true})
	if code != fiber.StatusServiceUnavailable || body["error"] != "grainhack_results_unconfigured" {
		t.Fatalf("issue without key: %d %v", code, body)
	}
	if code, _ := adminSuiteDo(t, app, "GET", path, adminSuiteToken(t, admin, "admin"), nil); code != fiber.StatusNotFound {
		t.Fatalf("read without key got %d, want 404 (reads need only the database)", code)
	}
	if code, _ := agentDo(t, app, "GET", "/grainhack/results-key", "", nil); code != fiber.StatusServiceUnavailable {
		t.Fatalf("key without key got %d", code)
	}
	code, body = agentDo(t, app, "GET", "/grainhack/results-statements/"+uuid.NewString(), "anything", nil)
	if code != fiber.StatusServiceUnavailable || body["error"] != "grainhack_agent_token_unconfigured" {
		t.Fatalf("agent route without token config: %d %v", code, body)
	}

	// A malformed key or network is the same 503, never a boot failure.
	bad := handlers.NewGrainHackPayoutHandler(d, config.Config{GrainHackResultsSigningKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
		GrainHackPayoutNetwork: "base-sepolia"}, nil)
	code, _ = adminSuiteDo(t, grainhackApp(t, d, bad), "POST", path, adminSuiteToken(t, admin, "admin"), map[string]any{"confirm": true})
	if code != fiber.StatusServiceUnavailable {
		t.Fatalf("bad network got %d, want 503", code)
	}
}

func TestGrainHackBaseSepoliaNotice_AdminOnlyAndConfirmed(t *testing.T) {
	d := dbtest.DB(t)
	ghEventFixture(t, d)
	app := grainhackApp(t, d, handlers.NewGrainHackPayoutHandlerWith(ghService(d, false), ghAgentToken))
	admin := adminSuiteInsertUser(t, d, "admin")
	contributor := adminSuiteInsertUser(t, d, "contributor")
	path := "/admin/grainhack/base-sepolia-notice"

	if code, _ := adminSuiteDo(t, app, "POST", path, adminSuiteToken(t, contributor, "contributor"), map[string]any{"confirm": true}); code != fiber.StatusForbidden {
		t.Fatalf("non-admin send got %d", code)
	}
	code, body := adminSuiteDo(t, app, "GET", path, adminSuiteToken(t, admin, "admin"), nil)
	if code != fiber.StatusOK || body["title"] != "GrainHack now pays USDC on Solana" || body["sent_at"] != nil {
		t.Fatalf("preview: %d %v", code, body)
	}
	code, body = adminSuiteDo(t, app, "POST", path, adminSuiteToken(t, admin, "admin"), map[string]any{})
	if code != fiber.StatusBadRequest || body["error"] != "confirm_required" {
		t.Fatalf("unconfirmed send: %d %v", code, body)
	}
	var n int
	d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM grainhack_broadcast_notices`).Scan(&n)
	if n != 0 {
		t.Fatal("an unconfirmed send was recorded")
	}
}
