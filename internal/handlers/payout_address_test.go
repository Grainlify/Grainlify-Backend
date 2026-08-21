package handlers

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func addrApp(uid uuid.UUID, d *db.DB) *fiber.App {
	app := fiber.New()
	h := NewPayoutAddressHandler(d)
	inject := func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, uid.String()); return c.Next() }
	app.Post("/me/payout-address/challenge", inject, h.PostChallenge)
	app.Post("/me/payout-address", inject, h.PostAddress)
	return app
}

func postJSON(t *testing.T, app *fiber.App, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	res, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	rb, _ := io.ReadAll(res.Body)
	var out map[string]any
	json.Unmarshal(rb, &out)
	return res.StatusCode, out
}

// registerFor drives the real challenge/sign/register flow with a real key.
func registerFor(t *testing.T, d *db.DB, uid uuid.UUID, priv ed25519.PrivateKey) (int, map[string]any) {
	t.Helper()
	pub := priv.Public().(ed25519.PublicKey)
	address := auth.AptosAddressFromEd25519(pub)
	app := addrApp(uid, d)

	code, ch := postJSON(t, app, "/me/payout-address/challenge",
		map[string]any{"chain_id": "aptos-testnet", "address": address})
	if code != 200 {
		t.Fatalf("challenge: %d %v", code, ch)
	}
	msg, _ := ch["message"].(string)
	nonce, _ := ch["nonce"].(string)
	sig := ed25519.Sign(priv, []byte(auth.AptosFullMessage(msg, nonce)))

	return postJSON(t, app, "/me/payout-address", map[string]any{
		"chain_id":   "aptos-testnet",
		"address":    address,
		"public_key": hex.EncodeToString(pub),
		"signature":  hex.EncodeToString(sig),
		"nonce":      nonce,
	})
}

func newUser(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := d.Pool.Exec(context.Background(),
		`INSERT INTO users (id, role) VALUES ($1,'contributor')`, id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Pool.Exec(context.Background(), `DELETE FROM contributor_addresses WHERE user_id=$1`, id)
		d.Pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, id)
	})
	return id
}

func TestRegister_HappyPath(t *testing.T) {
	d := dbtest.DB(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	code, body := registerFor(t, d, newUser(t, d), priv)
	if code != 201 {
		t.Fatalf("register: %d %v", code, body)
	}
	if body["verified_at"] == nil {
		t.Error("no verified_at returned")
	}
}

// The defect the unique index closes: two accounts, one live address.
//
// The refusal belongs at REGISTRATION, where the person is present and can act.
// At claim time the account that gets the error is whichever one asks, which is
// usually the innocent one being refused a real claim for something the other
// account did.
func TestRegister_RefusesAnAddressLiveOnAnotherAccount(t *testing.T) {
	d := dbtest.DB(t)
	_, priv, _ := ed25519.GenerateKey(nil)

	if code, body := registerFor(t, d, newUser(t, d), priv); code != 201 {
		t.Fatalf("first registration: %d %v", code, body)
	}
	// A second account proving control of the SAME key.
	code, body := registerFor(t, d, newUser(t, d), priv)
	if code != 409 {
		t.Fatalf("second account: status %d, want 409 (%v)", code, body)
	}
	if body["error"] != "address_registered_to_another_account" {
		t.Fatalf("error = %v", body["error"])
	}
	// It must say what to do, not merely refuse.
	detail, _ := body["detail"].(string)
	if detail == "" || !bytes.Contains([]byte(detail), []byte("contact us")) {
		t.Errorf("the refusal offers no remedy: %q", detail)
	}
}

// Superseding must free the address, or a person who moves wallets can never
// give the old one to anybody - including their own second account.
func TestRegister_ASupersededAddressIsFreed(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(nil)
	first := newUser(t, d)

	if code, _ := registerFor(t, d, first, priv); code != 201 {
		t.Fatal("first registration failed")
	}
	d.Pool.Exec(ctx, `UPDATE contributor_addresses SET superseded_at=now() WHERE user_id=$1`, first)

	if code, body := registerFor(t, d, newUser(t, d), priv); code != 201 {
		t.Fatalf("a superseded address was not freed: %d %v", code, body)
	}
}

// The conflict check runs only AFTER the signature verifies, so the endpoint is
// not an oracle for which addresses are registered.
func TestRegister_DoesNotRevealRegistrationWithoutProofOfControl(t *testing.T) {
	d := dbtest.DB(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	if code, _ := registerFor(t, d, newUser(t, d), priv); code != 201 {
		t.Fatal("setup registration failed")
	}

	// A different account claims the same address with a signature it cannot make.
	pub := priv.Public().(ed25519.PublicKey)
	address := auth.AptosAddressFromEd25519(pub)
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	otherPub := otherPriv.Public().(ed25519.PublicKey)

	app := addrApp(newUser(t, d), d)
	_, ch := postJSON(t, app, "/me/payout-address/challenge",
		map[string]any{"chain_id": "aptos-testnet", "address": address})
	msg, _ := ch["message"].(string)
	nonce, _ := ch["nonce"].(string)

	code, body := postJSON(t, app, "/me/payout-address", map[string]any{
		"chain_id":   "aptos-testnet",
		"address":    address,
		"public_key": hex.EncodeToString(otherPub),
		"signature":  hex.EncodeToString(ed25519.Sign(otherPriv, []byte(auth.AptosFullMessage(msg, nonce)))),
		"nonce":      nonce,
	})
	if body["error"] == "address_registered_to_another_account" {
		t.Fatalf("the endpoint revealed that %s is registered, to a caller who never proved "+
			"control of it — that is an enumeration oracle", address)
	}
	if code != 400 {
		t.Fatalf("status %d, want 400 for a signature from the wrong key (%v)", code, body)
	}
}

// The unique index firing must map to the sentence, not to a 500.
//
// Reached only under a real race, so tested against the error directly rather
// than by trying to win one: a test that must win a race to reach a branch
// passes for the wrong reason on a slow day.
func TestIsDuplicateAddress(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"the index firing", errString(`ERROR: duplicate key value violates unique constraint "idx_contributor_addresses_one_account" (SQLSTATE 23505)`), true},
		{"a different unique index", errString(`duplicate key value violates unique constraint "idx_contributor_addresses_live"`), false},
		{"an unrelated failure", errString("connection reset by peer"), false},
		{"no error", nil, false},
	} {
		if got := isDuplicateAddress(tc.err); got != tc.want {
			t.Errorf("%s: isDuplicateAddress = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
