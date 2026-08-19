// Package salt holds per-event Merkle identity salts, and is the only place a
// salt exists in plaintext.
//
// # The constraint
//
// A salt is never returned. There is no accessor, no getter, no Bytes method,
// and no exported type that carries one. Callers get identity hashes and
// nothing else, from inside a closure, and the plaintext is zeroed before that
// closure returns.
//
// # Why not a redacting type
//
// The obvious design is a Salt type whose String and MarshalJSON redact. It was
// considered and rejected, because the rule would be narrower than the mistake
// it answers. Redaction defends the paths somebody thought of; `%v` on an
// enclosing struct is the one they did not, and `%x` on a byte slice is
// another, and a struct dumped into an error message is a third. Each patch
// would be correct and the class would survive.
//
// The general rule, which this package is an instance of: **when a fix prompts a
// rule, ask whether the rule is as general as the mistake.** "Never logged"
// enforced by habit is a habit; enforced by having nothing to log, it is a
// property.
//
// # Why the entry point takes a batch
//
// IdentityHashes takes a slice of logins and returns a slice of hashes, rather
// than hashing one login per call.
//
// This is what makes the constraint hold rather than merely state it. With a
// per-login entry point, somebody with a legitimate need for forty hashes has a
// genuinely reasonable argument for an accessor: give me the salt and I will
// loop. With a batch entry point that argument does not exist, so there is no
// honest case for adding one - which is what stops one being added later by a
// competent person under deadline.
//
// Key rotation lives in here for the same reason: it is the other operation
// that would otherwise need the plaintext outside.
//
// # What this does not protect against
//
// Anyone holding both the database and SALT_ENC_KEY_B64 can decrypt. Those are
// currently both environment variables of the same application, so this defends
// a database dump, a backup leak and the Neon console - not a compromise of the
// environment holding both. Moving the key out is a precondition for mainnet;
// see docs/SCOPE-salt-and-addresses.md.
//
// And a salt does not make a payout anonymous. Our own database reconstructs the
// mapping by joining contributor_addresses to claim_leaves, with no salt
// involved. The salt raises the cost of BULK correlation by someone holding the
// chain and the public list of GitHub logins. Never claim more than that, and in
// particular never that destroying a salt means we cannot link a contributor.
package salt

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/chain"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// SaltLen is the per-event salt size. 32 bytes because the thing it defends is
// a preimage search over a public list of logins.
const SaltLen = 32

var (
	ErrNoKey         = errors.New("SALT_ENC_KEY_B64 is not configured")
	ErrBadKey        = errors.New("SALT_ENC_KEY_B64 must be base64 of exactly 32 bytes")
	ErrNotFound      = errors.New("no salt for this settlement")
	ErrDestroyed     = errors.New("the salt for this settlement has been destroyed")
	ErrAlreadyExists = errors.New("a salt already exists for this settlement")
	ErrExpired       = errors.New("this hasher is no longer valid; it escaped its WithSalt closure")
	ErrNoReason      = errors.New("destroying a salt requires a reason")
)

// decodeKey turns SALT_ENC_KEY_B64 into an AEAD.
//
// Refuses an absent or wrong-sized key rather than deriving something usable
// from it: a key that is silently stretched or truncated encrypts fine and is
// unrecoverable by anyone who configures the value correctly later.
func decodeKey(keyB64 string) (cipher.AEAD, error) {
	if keyB64 == "" {
		return nil, ErrNoKey
	}
	raw, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadKey, err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("%w: got %d bytes", ErrBadKey, len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// aad binds a ciphertext to its settlement.
//
// Without this, a ciphertext copied from one settlement's row into another's
// still decrypts, silently giving two events the same salt - which destroys the
// one property a per-event salt exists to provide.
func aad(settlementID uuid.UUID) []byte { return []byte("payout_event_salt:" + settlementID.String()) }

// Hasher is the only thing a caller ever holds. It is valid solely for the
// duration of the WithSalt closure that produced it.
type Hasher interface {
	// IdentityHashes returns H(lower(login) || salt) for each login, in order.
	IdentityHashes(logins []string) ([][32]byte, error)
}

type hasher struct {
	salt []byte
	dead bool
}

func (h *hasher) IdentityHashes(logins []string) ([][32]byte, error) {
	// Fail closed if the closure smuggled this out and called it later. By then
	// the salt is zeroed, so hashing would silently produce H(login || zeros) -
	// a wrong answer that looks exactly like a right one.
	if h.dead {
		return nil, ErrExpired
	}
	out := make([][32]byte, len(logins))
	for i, login := range logins {
		hh, err := chain.IdentityHash(login, h.salt)
		if err != nil {
			return nil, fmt.Errorf("login %d: %w", i, err)
		}
		out[i] = hh
	}
	return out, nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Create generates and stores a salt for a settlement. It returns nothing: the
// salt this produced is not available to the caller, then or ever.
func Create(ctx context.Context, pool db.DBPool, keyB64 string, settlementID uuid.UUID) error {
	aead, err := decodeKey(keyB64)
	if err != nil {
		return err
	}
	s := make([]byte, SaltLen)
	if _, err := rand.Read(s); err != nil {
		return fmt.Errorf("generate salt: %w", err)
	}
	defer zero(s)

	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	ct := aead.Seal(nil, nonce, s, aad(settlementID))

	tag, err := pool.Exec(ctx, `
		INSERT INTO payout_event_salts (settlement_id, ciphertext, nonce, key_version)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (settlement_id) DO NOTHING`,
		settlementID, ct, nonce)
	if err != nil {
		return fmt.Errorf("store salt: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either a live salt or a tombstone. Both mean "do not overwrite":
		// replacing a live salt invalidates every leaf built from it, and
		// resurrecting a destroyed one undoes a deliberate irreversible act.
		return ErrAlreadyExists
	}
	return nil
}

// WithSalt decrypts the salt for one settlement, hands a Hasher to fn, and
// zeroes the plaintext before returning. Nothing is cached between calls.
func WithSalt(ctx context.Context, pool db.DBPool, keyB64 string, settlementID uuid.UUID, fn func(Hasher) error) error {
	aead, err := decodeKey(keyB64)
	if err != nil {
		return err
	}

	var ct, nonce []byte
	var destroyedAt *string
	err = pool.QueryRow(ctx, `
		SELECT ciphertext, nonce, destroyed_at::text
		FROM payout_event_salts WHERE settlement_id = $1`, settlementID).
		Scan(&ct, &nonce, &destroyedAt)
	if err != nil {
		if isNoRows(err) {
			return ErrNotFound
		}
		return fmt.Errorf("load salt: %w", err)
	}
	// Distinguished from ErrNotFound deliberately: "never had one" and "had one
	// and we destroyed it" are different facts, and an operator reading a log
	// needs to know which.
	if destroyedAt != nil {
		return ErrDestroyed
	}

	s, err := aead.Open(nil, nonce, ct, aad(settlementID))
	if err != nil {
		// Do not include the ciphertext or nonce in the message.
		return fmt.Errorf("decrypt salt for settlement %s: %w "+
			"(wrong SALT_ENC_KEY_B64, or a ciphertext moved between settlements)", settlementID, err)
	}
	defer zero(s)

	h := &hasher{salt: s}
	// Kill the hasher before the salt is zeroed, so a hasher that outlived its
	// closure errors instead of hashing against zeroes.
	defer func() { h.dead = true }()

	return fn(h)
}

// Destroy tombstones the salt. Irreversible, deliberate, and never scheduled.
//
// A reason is required because a destruction with no stated cause cannot be told
// apart from an accident, and this is the one act here that cannot be undone.
func Destroy(ctx context.Context, pool db.DBPool, settlementID uuid.UUID, reason string) error {
	if len(trimSpace(reason)) == 0 {
		return ErrNoReason
	}
	tag, err := pool.Exec(ctx, `
		UPDATE payout_event_salts
		SET ciphertext = NULL, nonce = NULL, destroyed_at = now(), destroyed_reason = $2
		WHERE settlement_id = $1 AND destroyed_at IS NULL`,
		settlementID, reason)
	if err != nil {
		return fmt.Errorf("destroy salt: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Rotate re-wraps every live salt under a new key.
//
// This lives here because it is the other operation that would otherwise need
// the plaintext outside this package - and it is the most likely reason somebody
// would add the accessor this package exists to avoid.
func Rotate(ctx context.Context, pool db.DBPool, oldKeyB64, newKeyB64 string, newVersion int) (int, error) {
	oldAEAD, err := decodeKey(oldKeyB64)
	if err != nil {
		return 0, fmt.Errorf("old key: %w", err)
	}
	newAEAD, err := decodeKey(newKeyB64)
	if err != nil {
		return 0, fmt.Errorf("new key: %w", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT settlement_id, ciphertext, nonce
		FROM payout_event_salts WHERE destroyed_at IS NULL`)
	if err != nil {
		return 0, fmt.Errorf("list salts: %w", err)
	}
	type item struct {
		id        uuid.UUID
		ct, nonce []byte
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.ct, &it.nonce); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	for _, it := range items {
		s, err := oldAEAD.Open(nil, it.nonce, it.ct, aad(it.id))
		if err != nil {
			return n, fmt.Errorf("settlement %s: decrypt under old key: %w", it.id, err)
		}
		nonce := make([]byte, newAEAD.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			zero(s)
			return n, err
		}
		ct := newAEAD.Seal(nil, nonce, s, aad(it.id))
		zero(s)

		if _, err := pool.Exec(ctx, `
			UPDATE payout_event_salts SET ciphertext = $2, nonce = $3, key_version = $4
			WHERE settlement_id = $1 AND destroyed_at IS NULL`,
			it.id, ct, nonce, newVersion); err != nil {
			return n, fmt.Errorf("settlement %s: store re-wrapped salt: %w", it.id, err)
		}
		n++
	}
	return n, nil
}
