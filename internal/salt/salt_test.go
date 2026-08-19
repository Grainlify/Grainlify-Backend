package salt

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/chain"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func key(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

func settlement(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
		INSERT INTO settlements (pool_usdc, total_weight, unit_value_usdc, pool_minor)
		VALUES (1, 1, 1, 1000000) RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatalf("insert settlement: %v", err)
	}
	t.Cleanup(func() {
		d.Pool.Exec(context.Background(), `DELETE FROM claim_leaves WHERE settlement_id=$1`, id)
		d.Pool.Exec(context.Background(), `DELETE FROM payout_event_salts WHERE settlement_id=$1`, id)
		d.Pool.Exec(context.Background(), `DELETE FROM settlements WHERE id=$1`, id)
	})
	return id
}

func hashesFor(t *testing.T, d *db.DB, k string, id uuid.UUID, logins ...string) [][32]byte {
	t.Helper()
	var got [][32]byte
	err := WithSalt(context.Background(), d.Pool, k, id, func(h Hasher) error {
		var err error
		got, err = h.IdentityHashes(logins)
		return err
	})
	if err != nil {
		t.Fatalf("WithSalt: %v", err)
	}
	return got
}

func TestSalt_RoundTripIsStable(t *testing.T) {
	d := dbtest.DB(t)
	id := settlement(t, d)
	if err := Create(context.Background(), d.Pool, key(1), id); err != nil {
		t.Fatalf("Create: %v", err)
	}
	a := hashesFor(t, d, key(1), id, "Alice", "bob")
	b := hashesFor(t, d, key(1), id, "Alice", "bob")
	if a[0] != b[0] || a[1] != b[1] {
		t.Fatal("the same login hashed differently across two decrypts")
	}
	if a[0] == a[1] {
		t.Fatal("two different logins produced the same identity hash")
	}
}

// Lowercasing is load-bearing: GitHub records capitalisation inconsistently, and
// one person hashing two ways matches their leaf in some rows and not others.
func TestSalt_LoginCaseDoesNotChangeTheHash(t *testing.T) {
	d := dbtest.DB(t)
	id := settlement(t, d)
	if err := Create(context.Background(), d.Pool, key(1), id); err != nil {
		t.Fatal(err)
	}
	got := hashesFor(t, d, key(1), id, "Alice", "alice", "  ALICE  ")
	if got[0] != got[1] || got[1] != got[2] {
		t.Fatal("case or surrounding space changed the identity hash")
	}
}

func TestSalt_DifferentSettlementsCannotBeCorrelated(t *testing.T) {
	d := dbtest.DB(t)
	a, b := settlement(t, d), settlement(t, d)
	if err := Create(context.Background(), d.Pool, key(1), a); err != nil {
		t.Fatal(err)
	}
	if err := Create(context.Background(), d.Pool, key(1), b); err != nil {
		t.Fatal(err)
	}
	if hashesFor(t, d, key(1), a, "alice")[0] == hashesFor(t, d, key(1), b, "alice")[0] {
		t.Fatal("one login hashed identically in two settlements; the salt is not per-event")
	}
}

// The AAD binding. Without it a ciphertext copied between settlements still
// decrypts, silently giving two events one salt.
func TestSalt_ACiphertextMovedToAnotherSettlementDoesNotDecrypt(t *testing.T) {
	d := dbtest.DB(t)
	a, b := settlement(t, d), settlement(t, d)
	ctx := context.Background()
	if err := Create(ctx, d.Pool, key(1), a); err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, d.Pool, key(1), b); err != nil {
		t.Fatal(err)
	}
	// Move a's ciphertext into b's row, as a careless restore or a malicious
	// UPDATE would.
	if _, err := d.Pool.Exec(ctx, `
		UPDATE payout_event_salts SET ciphertext=(SELECT ciphertext FROM payout_event_salts WHERE settlement_id=$1),
		                              nonce=(SELECT nonce FROM payout_event_salts WHERE settlement_id=$1)
		WHERE settlement_id=$2`, a, b); err != nil {
		t.Fatal(err)
	}
	err := WithSalt(ctx, d.Pool, key(1), b, func(Hasher) error { return nil })
	if err == nil {
		t.Fatal("a ciphertext from another settlement decrypted; the AAD binding is absent")
	}
}

func TestSalt_WrongKeyIsRefusedAndSaysSo(t *testing.T) {
	d := dbtest.DB(t)
	id := settlement(t, d)
	if err := Create(context.Background(), d.Pool, key(1), id); err != nil {
		t.Fatal(err)
	}
	err := WithSalt(context.Background(), d.Pool, key(2), id, func(Hasher) error { return nil })
	if err == nil {
		t.Fatal("the wrong key decrypted the salt")
	}
}

// "Never had one" and "had one and we destroyed it" are different facts.
func TestSalt_DestroyedIsDistinguishableFromAbsent(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	id := settlement(t, d)

	if err := WithSalt(ctx, d.Pool, key(1), id, func(Hasher) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent salt: want ErrNotFound, got %v", err)
	}
	if err := Create(ctx, d.Pool, key(1), id); err != nil {
		t.Fatal(err)
	}
	if err := Destroy(ctx, d.Pool, id, "event settled, all claims made"); err != nil {
		t.Fatal(err)
	}
	if err := WithSalt(ctx, d.Pool, key(1), id, func(Hasher) error { return nil }); !errors.Is(err, ErrDestroyed) {
		t.Fatalf("destroyed salt: want ErrDestroyed, got %v", err)
	}
}

func TestSalt_DestroyRequiresAReason(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	id := settlement(t, d)
	if err := Create(ctx, d.Pool, key(1), id); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"", "   ", "\t\n"} {
		if err := Destroy(ctx, d.Pool, id, r); !errors.Is(err, ErrNoReason) {
			t.Errorf("blank reason %q: want ErrNoReason, got %v", r, err)
		}
	}
}

// The tombstone survives, so "did this event have a salt, and when did we
// destroy it, and why" stays answerable.
func TestSalt_DestructionLeavesATombstoneNotAHole(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	id := settlement(t, d)
	if err := Create(ctx, d.Pool, key(1), id); err != nil {
		t.Fatal(err)
	}
	if err := Destroy(ctx, d.Pool, id, "window closed, all claimed"); err != nil {
		t.Fatal(err)
	}
	var n int
	var reason *string
	if err := d.Pool.QueryRow(ctx, `SELECT count(*), max(destroyed_reason) FROM payout_event_salts WHERE settlement_id=$1 AND destroyed_at IS NOT NULL`, id).Scan(&n, &reason); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("tombstone row count = %d, want 1", n)
	}
	if reason == nil || *reason != "window closed, all claimed" {
		t.Fatalf("reason not recorded next to the tombstone: %v", reason)
	}
}

// Neither replacing a live salt nor resurrecting a destroyed one.
func TestSalt_CannotOverwriteOrResurrect(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	id := settlement(t, d)
	if err := Create(ctx, d.Pool, key(1), id); err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, d.Pool, key(1), id); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("overwriting a live salt: want ErrAlreadyExists, got %v", err)
	}
	if err := Destroy(ctx, d.Pool, id, "done"); err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, d.Pool, key(1), id); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("resurrecting a destroyed salt: want ErrAlreadyExists, got %v", err)
	}
}

// A Hasher smuggled out of its closure must FAIL, not hash against a zeroed
// salt - which would be a wrong answer shaped exactly like a right one.
func TestSalt_AHasherThatEscapesItsClosureFailsClosed(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	id := settlement(t, d)
	if err := Create(ctx, d.Pool, key(1), id); err != nil {
		t.Fatal(err)
	}
	var escaped Hasher
	var inside [][32]byte
	if err := WithSalt(ctx, d.Pool, key(1), id, func(h Hasher) error {
		escaped = h
		var err error
		inside, err = h.IdentityHashes([]string{"alice"})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	after, err := escaped.IdentityHashes([]string{"alice"})
	if err == nil {
		zeroSalt, _ := chain.IdentityHash("alice", make([]byte, SaltLen))
		if len(after) > 0 && after[0] == zeroSalt {
			t.Fatal("the escaped hasher hashed against a ZEROED salt and returned it as an answer")
		}
		t.Fatal("the escaped hasher still worked outside its closure")
	}
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	if len(inside) != 1 {
		t.Fatal("the in-closure call should have worked")
	}
}

// The plaintext must be zeroed when the closure returns, not merely go out of
// scope. Reaching into the concrete hasher is deliberate: this is the only
// assertion that can see the buffer, and without it a mutation removing the
// zeroing survives - the fail-closed flag alone keeps every other test green.
func TestSalt_PlaintextIsZeroedWhenTheClosureReturns(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	id := settlement(t, d)
	if err := Create(ctx, d.Pool, key(1), id); err != nil {
		t.Fatal(err)
	}
	var h *hasher
	if err := WithSalt(ctx, d.Pool, key(1), id, func(got Hasher) error {
		h = got.(*hasher)
		if allZero(h.salt) {
			t.Fatal("the salt was already zeroed INSIDE the closure")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(h.salt) == 0 {
		t.Fatal("cannot tell: the buffer was replaced rather than zeroed")
	}
	if !allZero(h.salt) {
		t.Fatal("the plaintext salt is still in memory after the closure returned")
	}
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func TestSalt_RotateRewrapsWithoutChangingHashes(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	id := settlement(t, d)
	if err := Create(ctx, d.Pool, key(1), id); err != nil {
		t.Fatal(err)
	}
	before := hashesFor(t, d, key(1), id, "alice")

	n, err := Rotate(ctx, d.Pool, key(1), key(9), 2)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if n < 1 {
		t.Fatal("Rotate re-wrapped nothing")
	}
	if after := hashesFor(t, d, key(9), id, "alice"); after[0] != before[0] {
		t.Fatal("rotation changed the identity hash; every published leaf would be invalidated")
	}
	if err := WithSalt(ctx, d.Pool, key(1), id, func(Hasher) error { return nil }); err == nil {
		t.Fatal("the OLD key still decrypts after rotation")
	}
}

// Rotation must not resurrect tombstones.
func TestSalt_RotateSkipsDestroyedSalts(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	id := settlement(t, d)
	if err := Create(ctx, d.Pool, key(1), id); err != nil {
		t.Fatal(err)
	}
	if err := Destroy(ctx, d.Pool, id, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := Rotate(ctx, d.Pool, key(1), key(9), 2); err != nil {
		t.Fatalf("Rotate over a tombstone: %v", err)
	}
	var ct []byte
	if err := d.Pool.QueryRow(ctx, `SELECT ciphertext FROM payout_event_salts WHERE settlement_id=$1`, id).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if ct != nil {
		t.Fatal("rotation wrote a ciphertext back into a destroyed row")
	}
}

func TestSalt_KeyIsValidatedNotStretched(t *testing.T) {
	for _, k := range []string{
		"",
		"not-base64!!",
		base64.StdEncoding.EncodeToString([]byte("too short")),
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 64)),
		// 16 and 24 bytes are VALID AES key sizes, so aes.NewCipher accepts
		// them and the explicit length check is the only thing standing between
		// a misconfigured key and silent AES-128. A mutation removing that check
		// survived until these two cases existed.
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16)),
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 24)),
	} {
		if _, err := decodeKey(k); err == nil {
			t.Errorf("a bad key was accepted: %q", k)
		}
	}
}
