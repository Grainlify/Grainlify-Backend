package handlers

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// A real Solana address (the SPL Token program) and the system program, whose
// address is 32 zero bytes and so starts with a run of '1's.
const (
	solAddr     = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	solSystem   = "11111111111111111111111111111111"
	testSeedB64 = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=" // bytes 0..31
)

func bountyWalletApp(uid *uuid.UUID, d *db.DB, keyB64 string, now time.Time) *fiber.App {
	app := fiber.New()
	h := NewBountyWalletHandler(d, keyB64, "http://127.0.0.1:1")
	h.now = func() time.Time { return now }
	inject := func(c *fiber.Ctx) error {
		if uid != nil {
			c.Locals(auth.LocalUserID, uid.String())
		}
		return c.Next()
	}
	app.Post("/me/bounty-wallet/challenge", inject, h.PostChallenge)
	app.Post("/me/bounty-wallet/read-challenge", inject, h.PostReadChallenge)
	return app
}

func testPub(t *testing.T) ed25519.PublicKey {
	t.Helper()
	seed, _ := base64.StdEncoding.DecodeString(testSeedB64)
	return ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
}

func TestBountyWallet_UnconfiguredKeyAnswers503(t *testing.T) {
	uid := uuid.New()
	for _, key := range []string{"", "not base64!", base64.StdEncoding.EncodeToString(make([]byte, 31))} {
		code, out := postJSON(t, bountyWalletApp(&uid, nil, key, time.Now()), "/me/bounty-wallet/challenge", map[string]any{"wallet": solAddr})
		if code != 503 || out["error"] != "bounty_wallet_link_unconfigured" {
			t.Fatalf("key %q: got %d %v, want 503 unconfigured", key, code, out)
		}
	}
}

func TestBountyWallet_RequiresSignedInUser(t *testing.T) {
	code, _ := postJSON(t, bountyWalletApp(nil, nil, testSeedB64, time.Now()), "/me/bounty-wallet/challenge", map[string]any{"wallet": solAddr})
	if code != 401 {
		t.Fatalf("got %d, want 401", code)
	}
}

func TestBountyWallet_RefusesWhatIsNotASolanaAddress(t *testing.T) {
	uid := uuid.New()
	app := bountyWalletApp(&uid, nil, testSeedB64, time.Now())
	for _, w := range []string{"", "0x52908400098527886E0F7030069857D2E4169EE7", solAddr + "1", solAddr[:len(solAddr)-1], "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5D0", strings.Repeat("1", 33)} {
		code, out := postJSON(t, app, "/me/bounty-wallet/challenge", map[string]any{"wallet": w})
		if code != 400 || out["error"] != "invalid_solana_address" {
			t.Fatalf("wallet %q: got %d %v, want 400 invalid_solana_address", w, code, out)
		}
	}
}

func TestIsSolanaAddress(t *testing.T) {
	for _, ok := range []string{solAddr, solSystem, "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU"} {
		if !isSolanaAddress(ok) {
			t.Errorf("%s should be valid", ok)
		}
	}
}

// The agent parses this text back; its shape is a contract.
func TestBountyLinkMessage_Shape(t *testing.T) {
	issued := time.Date(2026, 9, 19, 14, 2, 11, 0, time.UTC)
	got := BountyLinkMessage("Octocat", 583231, solAddr, "3f9c1a0be27d4c85", issued, issued.Add(10*time.Minute))
	want := "Grainlify: link this wallet to my GitHub account\n" +
		"GitHub: Octocat (id 583231)\n" +
		"Wallet: " + solAddr + "\n" +
		"Nonce: 3f9c1a0be27d4c85\n" +
		"Issued: 2026-09-19T14:02:11Z\n" +
		"Expires: 2026-09-19T14:12:11Z"
	if got != want {
		t.Fatalf("message changed:\n%s\nwant:\n%s", got, want)
	}
}

func linkGitHub(t *testing.T, d *db.DB, uid uuid.UUID, login string) int64 {
	t.Helper()
	ghID := 900000000 + rand.Int63n(99999999)
	if _, err := d.Pool.Exec(context.Background(),
		`INSERT INTO github_accounts (user_id, github_user_id, login, access_token) VALUES ($1,$2,$3,'\x00')`, uid, ghID, login); err != nil {
		t.Fatal(err)
	}
	return ghID
}

func TestBountyWallet_NoGitHubAccountIs409(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	code, out := postJSON(t, bountyWalletApp(&uid, d, testSeedB64, time.Now()), "/me/bounty-wallet/challenge", map[string]any{"wallet": solAddr})
	if code != 409 || out["error"] != "github_not_linked" {
		t.Fatalf("got %d %v, want 409 github_not_linked", code, out)
	}
}

func TestBountyWallet_CountersignsTheSessionsOwnGitHubAccount(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	ghID := linkGitHub(t, d, uid, "Octocat")
	now := time.Date(2026, 9, 19, 14, 2, 11, 500, time.UTC)
	app := bountyWalletApp(&uid, d, testSeedB64, now)

	code, out := postJSON(t, app, "/me/bounty-wallet/challenge", map[string]any{"wallet": solAddr})
	if code != 200 {
		t.Fatalf("got %d %v", code, out)
	}
	msg, _ := out["message"].(string)
	nonce := regexp.MustCompile(`(?m)^Nonce: ([0-9a-f]{32})$`).FindStringSubmatch(msg)
	if nonce == nil {
		t.Fatalf("no 128-bit hex nonce in:\n%s", msg)
	}
	want := BountyLinkMessage("Octocat", ghID, solAddr, nonce[1], now.Truncate(time.Second), now.Truncate(time.Second).Add(10*time.Minute))
	if msg != want {
		t.Fatalf("message:\n%s\nwant:\n%s", msg, want)
	}

	sig, err := base64.StdEncoding.DecodeString(out["countersignature"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(testPub(t), []byte(bountyLinkDomain+msg), sig) {
		t.Fatal("countersignature does not verify over domain + message")
	}
	// Without the domain prefix it must NOT verify: the key's signatures mean
	// only this one thing.
	if ed25519.Verify(testPub(t), []byte(msg), sig) {
		t.Fatal("countersignature verifies without the domain prefix")
	}

	// A body naming some other account changes nothing: identity is the session's.
	_, out2 := postJSON(t, app, "/me/bounty-wallet/challenge", map[string]any{"wallet": solAddr, "login": "someone-else", "github_user_id": 1})
	msg2, _ := out2["message"].(string)
	if !strings.Contains(msg2, "GitHub: Octocat (id ") || strings.Contains(msg2, "someone-else") {
		t.Fatalf("identity taken from the request body:\n%s", msg2)
	}
	if msg2 == msg {
		t.Fatal("two challenges carried the same nonce")
	}

	// Writes nothing.
	var n int
	d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM contributor_addresses WHERE user_id=$1`, uid).Scan(&n)
	if n != 0 {
		t.Fatalf("challenge wrote %d payout-address rows", n)
	}
}

func TestBountyReadMessage_Shape(t *testing.T) {
	issued := time.Date(2026, 9, 26, 9, 30, 0, 0, time.UTC)
	got := BountyReadMessage("Octocat", 583231, "3f9c1a0be27d4c853f9c1a0be27d4c85", issued, issued.Add(10*time.Minute))
	want := "Grainlify: read my linked wallet\n" +
		"GitHub: Octocat (id 583231)\n" +
		"Nonce: 3f9c1a0be27d4c853f9c1a0be27d4c85\n" +
		"Issued: 2026-09-26T09:30:00Z\n" +
		"Expires: 2026-09-26T09:40:00Z"
	if got != want {
		t.Fatalf("read message changed:\n%s\nwant:\n%s", got, want)
	}
	// It must carry no Wallet line: a read challenge asks, it does not assert.
	if strings.Contains(got, "Wallet:") {
		t.Fatal("a read challenge must not contain a Wallet line")
	}
}

// The two challenges are signed by the same key, so the domain prefix is the
// only thing keeping them apart. A read signature verified under the link
// domain would mean a read challenge could stand in for a link.
func TestBountyWallet_ReadAndLinkDomainsAreDistinct(t *testing.T) {
	if bountyReadDomain == bountyLinkDomain {
		t.Fatal("read and link domains must differ")
	}
	issued := time.Date(2026, 9, 26, 9, 30, 0, 0, time.UTC)
	seed, _ := base64.StdEncoding.DecodeString(testSeedB64)
	key := ed25519.NewKeyFromSeed(seed)
	readMsg := BountyReadMessage("Octocat", 583231, "3f9c1a0be27d4c853f9c1a0be27d4c85", issued, issued.Add(10*time.Minute))
	sig := ed25519.Sign(key, []byte(bountyReadDomain+readMsg))

	if !ed25519.Verify(testPub(t), []byte(bountyReadDomain+readMsg), sig) {
		t.Fatal("a read signature must verify under the read domain")
	}
	if ed25519.Verify(testPub(t), []byte(bountyLinkDomain+readMsg), sig) {
		t.Fatal("a read signature must NOT verify under the link domain")
	}
}

func TestBountyWallet_ReadChallengeCountersignsTheSessionsOwnAccount(t *testing.T) {
	d := dbtest.DB(t)
	uid := newUser(t, d)
	ghID := linkGitHub(t, d, uid, "Octocat")
	now := time.Date(2026, 9, 26, 9, 30, 0, 0, time.UTC)
	app := bountyWalletApp(&uid, d, testSeedB64, now)

	code, out := postJSON(t, app, "/me/bounty-wallet/read-challenge", map[string]any{})
	if code != 200 {
		t.Fatalf("got %d %v", code, out)
	}
	msg, _ := out["message"].(string)
	// The GitHub identity comes from the session, never from the request.
	if !strings.Contains(msg, fmt.Sprintf("GitHub: Octocat (id %d)", ghID)) {
		t.Fatalf("message does not name the session's own account: %q", msg)
	}
	csig, _ := out["countersignature"].(string)
	sig, err := base64.StdEncoding.DecodeString(csig)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(testPub(t), []byte(bountyReadDomain+msg), sig) {
		t.Fatal("countersignature does not verify under the read domain")
	}
}

func TestBountyWallet_ReadChallengeRequiresSignedInUser(t *testing.T) {
	app := bountyWalletApp(nil, nil, testSeedB64, time.Now())
	code, out := postJSON(t, app, "/me/bounty-wallet/read-challenge", map[string]any{})
	if code != 401 {
		t.Fatalf("got %d %v, want 401", code, out)
	}
}

// The pairing with the agent is only checkable from outside if this key is
// published, and it must be the verifying half -- never the seed.
func TestBountyWallet_CountersignKeyIsThePublicHalf(t *testing.T) {
	app := bountyWalletApp(nil, nil, testSeedB64, time.Now())
	app.Get("/bounty-wallet/countersign-key", NewBountyWalletHandler(nil, testSeedB64, "http://127.0.0.1:1").GetCountersignKey)
	res, err := app.Test(httptest.NewRequest("GET", "/bounty-wallet/countersign-key", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body struct {
		PublicKey  string `json:"public_key"`
		ReadDomain string `json:"read_domain"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString(testPub(t))
	if body.PublicKey != want {
		t.Fatalf("public_key = %q, want %q", body.PublicKey, want)
	}
	if body.ReadDomain != bountyReadDomain {
		t.Fatalf("read_domain = %q, want %q", body.ReadDomain, bountyReadDomain)
	}
	// The seed must never appear.
	if strings.Contains(body.PublicKey, testSeedB64) {
		t.Fatal("the response leaked the signing seed")
	}
}

// The browser makes ONE call, to an origin it already trusts, and this service
// does the signed hop. The point is that no wallet extension sits in the
// middle of the second request.
func TestBountyWallet_GetLink_AsksTheAgentItself(t *testing.T) {
	var gotBody []byte
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"linked":true,"wallet":"HKMMpctYvofRCSF2uGnqfEGWcmMhD8A86xFqgmWTvcq9","linkedAt":"2026-09-26T09:00:00Z"}`))
	}))
	defer agent.Close()

	d := dbtest.DB(t)
	uid := newUser(t, d)
	ghID := linkGitHub(t, d, uid, "Octocat")
	h := NewBountyWalletHandler(d, testSeedB64, agent.URL)
	app := fiber.New()
	app.Get("/me/bounty-wallet/link", func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, uid.String()); return c.Next() }, h.GetLink)

	res, err := app.Test(httptest.NewRequest("GET", "/me/bounty-wallet/link", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var out struct {
		Linked bool    `json:"linked"`
		Wallet *string `json:"wallet"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Linked || out.Wallet == nil {
		t.Fatalf("expected a linked wallet, got %+v", out)
	}

	// What we sent the agent must be a properly countersigned read challenge
	// naming this session's own GitHub account.
	var sent struct {
		Message          string `json:"message"`
		Countersignature string `json:"countersignature"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("the agent received something that is not JSON: %q", string(gotBody))
	}
	if !strings.Contains(sent.Message, fmt.Sprintf("GitHub: Octocat (id %d)", ghID)) {
		t.Fatalf("challenge does not name the session's account: %q", sent.Message)
	}
	sig, err := base64.StdEncoding.DecodeString(sent.Countersignature)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(testPub(t), []byte(bountyReadDomain+sent.Message), sig) {
		t.Fatal("the challenge we sent the agent does not verify under the read domain")
	}
}

func TestBountyWallet_GetLink_AgentDownIsNotNoWallet(t *testing.T) {
	// "We could not reach the agent" and "you have no wallet" are different
	// claims. Conflating them is what made the page tell people to link a
	// wallet they had already linked.
	d := dbtest.DB(t)
	uid := newUser(t, d)
	linkGitHub(t, d, uid, "Octocat")
	h := NewBountyWalletHandler(d, testSeedB64, "http://127.0.0.1:1")
	app := fiber.New()
	app.Get("/me/bounty-wallet/link", func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, uid.String()); return c.Next() }, h.GetLink)
	res, _ := app.Test(httptest.NewRequest("GET", "/me/bounty-wallet/link", nil), -1)
	if res.StatusCode != 502 {
		t.Fatalf("status = %d, want 502 for an unreachable agent", res.StatusCode)
	}
}

func TestBountyWallet_PostLink_RelaysTheAgentsAnswerVerbatim(t *testing.T) {
	// A relay, not an authority: the agent re-verifies both signatures and its
	// refusals are written for the person who asked, so they pass through
	// unchanged rather than being reworded here.
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["walletSignature"] != "wallet-sig" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"bad_wallet_signature"}`))
			return
		}
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"wallet_linked_to_another_account","detail":"that wallet is already linked"}`))
	}))
	defer agent.Close()

	d := dbtest.DB(t)
	uid := newUser(t, d)
	h := NewBountyWalletHandler(d, testSeedB64, agent.URL)
	app := fiber.New()
	app.Post("/me/bounty-wallet/link", func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, uid.String()); return c.Next() }, h.PostLink)

	code, out := postJSON(t, app, "/me/bounty-wallet/link", map[string]any{
		"message": "m", "countersignature": "c", "walletSignature": "wallet-sig",
	})
	if code != 409 {
		t.Fatalf("status = %d, want the agent's own 409", code)
	}
	if out["error"] != "wallet_linked_to_another_account" {
		t.Fatalf("the agent's refusal was not passed through: %v", out)
	}
}

func TestBountyWallet_PostLink_RequiresSignedInUser(t *testing.T) {
	h := NewBountyWalletHandler(nil, testSeedB64, "http://127.0.0.1:1")
	app := fiber.New()
	app.Post("/me/bounty-wallet/link", h.PostLink)
	code, _ := postJSON(t, app, "/me/bounty-wallet/link", map[string]any{})
	if code != 401 {
		t.Fatalf("status = %d, want 401", code)
	}
}
