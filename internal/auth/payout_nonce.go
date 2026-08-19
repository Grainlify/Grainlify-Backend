// Payout-purpose nonces.
//
// # This file cannot create a user, and that is the point
//
// It has no import of anything that writes to `users`, and contains no INSERT or
// UPDATE against that table. `payout_nonce_surface_test.go` parses this file and
// fails if either appears.
//
// The alternative - a function that happens not to call the upsert - is not
// enough. `ConsumeNonceAndUpsertUser` does exactly what its name says, and on the
// sign-in path that is correct: a wallet nobody has seen becomes an account. On a
// payout path the caller is ALREADY AUTHENTICATED, so minting an account there
// gives somebody a second identity that owns their payout address while their
// real account owns their contributions - and nothing surfaces that until the two
// are compared.
//
// A function that CAN mint an account, sitting on a payout path, is the kind of
// thing that gets reused in six months because the name looked close enough and
// the signature fitted. So the capability is absent rather than unused, in the
// same way internal/salt has no accessor rather than a carefully-unused one.
//
// Nor is this the same function behind a flag. A boolean deciding whether to
// create an account is one edit from being passed the wrong way, and the wrong
// way is silent.
//
//	Authentication mints identities. A payout path must never be able to.
package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// Purpose separates what a signature authorises.
//
// Without it, a signature collected to sign in can be replayed to register a
// payout destination: same wallet, same nonce table, different meaning. The two
// now mean different things and the challenge has to say which.
type Purpose string

const (
	PurposeSignIn        Purpose = "signin"
	PurposePayoutAddress Purpose = "payout_address"
)

var (
	ErrNonceUnknown      = errors.New("nonce_unknown")
	ErrNonceExpired      = errors.New("nonce_expired")
	ErrNonceUsed         = errors.New("nonce_used")
	ErrNonceWrongPurpose = errors.New("nonce_wrong_purpose")
)

// CreateNonceForPurpose issues a single-use nonce scoped to one purpose.
func CreateNonceForPurpose(ctx context.Context, pool db.DBPool, walletType WalletType, address string, purpose Purpose, ttl time.Duration) (Nonce, error) {
	if pool == nil {
		return Nonce{}, fmt.Errorf("db not configured")
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	nonce := randomNonce(32)
	expiresAt := time.Now().UTC().Add(ttl)

	if _, err := pool.Exec(ctx, `
		INSERT INTO auth_nonces (wallet_type, address, nonce, purpose, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		string(walletType), address, nonce, string(purpose), expiresAt); err != nil {
		return Nonce{}, err
	}
	return Nonce{Nonce: nonce, ExpiresAt: expiresAt}, nil
}

// ConsumeNonceForPurpose marks a nonce used and returns nothing but an error.
//
// It creates nothing, updates nothing outside auth_nonces, and reports one named
// error per cause - "never had one", "it expired", "already used" and "issued for
// something else" are four different facts, and an operator reading a log needs
// to know which.
func ConsumeNonceForPurpose(ctx context.Context, pool db.DBPool, walletType WalletType, address string, nonce string, purpose Purpose) error {
	if pool == nil {
		return fmt.Errorf("db not configured")
	}

	var expiresAt time.Time
	var usedAt *time.Time
	var gotPurpose string
	err := pool.QueryRow(ctx, `
		SELECT expires_at, used_at, purpose FROM auth_nonces
		WHERE wallet_type = $1 AND address = $2 AND nonce = $3`,
		string(walletType), address, nonce).Scan(&expiresAt, &usedAt, &gotPurpose)
	if err != nil {
		return ErrNonceUnknown
	}
	// Purpose first: a nonce issued for sign-in being presented here is a
	// different problem from an expired one, and the more interesting of the two.
	if Purpose(gotPurpose) != purpose {
		return fmt.Errorf("%w: issued for %q, presented for %q", ErrNonceWrongPurpose, gotPurpose, purpose)
	}
	if usedAt != nil {
		return ErrNonceUsed
	}
	if time.Now().UTC().After(expiresAt) {
		return ErrNonceExpired
	}

	// Single-use is enforced by the UPDATE's own WHERE clause, not by the read
	// above: two requests arriving together both pass the check, and only one
	// can win this.
	tag, err := pool.Exec(ctx, `
		UPDATE auth_nonces SET used_at = now()
		WHERE wallet_type = $1 AND address = $2 AND nonce = $3 AND purpose = $4 AND used_at IS NULL`,
		string(walletType), address, nonce, string(purpose))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNonceUsed
	}
	return nil
}
