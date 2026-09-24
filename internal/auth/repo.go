package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

type User struct {
	ID   uuid.UUID `json:"id"`
	Role string    `json:"role"`
}

type Wallet struct {
	WalletType WalletType `json:"wallet_type"`
	Address    string     `json:"address"`
	PublicKey  string     `json:"public_key,omitempty"`
}

type Nonce struct {
	Nonce     string    `json:"nonce"`
	ExpiresAt time.Time `json:"expires_at"`
}

func CreateNonce(ctx context.Context, pool db.DBPool, walletType WalletType, address string, ttl time.Duration) (Nonce, error) {
	if pool == nil {
		return Nonce{}, fmt.Errorf("db not configured")
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}

	nonce := randomNonce(32)
	expiresAt := time.Now().UTC().Add(ttl)

	// The purpose is written explicitly rather than left to the column default.
	// A sign-in nonce must never be usable at the payout-address endpoint, and
	// that guarantee should not depend on what the default happens to be.
	_, err := pool.Exec(ctx, `
INSERT INTO auth_nonces (wallet_type, address, nonce, purpose, expires_at)
VALUES ($1, $2, $3, $4, $5)
`, string(walletType), address, nonce, string(PurposeSignIn), expiresAt)
	if err != nil {
		return Nonce{}, err
	}

	return Nonce{Nonce: nonce, ExpiresAt: expiresAt}, nil
}

type VerifyResult struct {
	User   User   `json:"user"`
	Wallet Wallet `json:"wallet"`
}

func ConsumeNonceAndUpsertUser(ctx context.Context, pool db.DBPool, walletType WalletType, address string, nonce string, publicKey string) (VerifyResult, error) {
	if pool == nil {
		return VerifyResult{}, fmt.Errorf("db not configured")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return VerifyResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Scoped to the sign-in purpose. Without this a nonce issued for payout-address
	// verification would be redeemable here; today the two challenge texts differ,
	// so no signature carries across, but that is one accident away from being the
	// only thing standing between the two flows. A nonce issued for another purpose
	// is not consumed by the attempt -- the row is never reached, so it stays usable
	// for the flow it was meant for.
	var nonceID uuid.UUID
	err = tx.QueryRow(ctx, `
SELECT id
FROM auth_nonces
WHERE wallet_type = $1
  AND address = $2
  AND nonce = $3
  AND purpose = $4
  AND used_at IS NULL
  AND expires_at > now()
FOR UPDATE
`, string(walletType), address, nonce, string(PurposeSignIn)).Scan(&nonceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerifyResult{}, fmt.Errorf("invalid_or_expired_nonce")
	}
	if err != nil {
		return VerifyResult{}, err
	}

	if _, err := tx.Exec(ctx, `UPDATE auth_nonces SET used_at = now() WHERE id = $1`, nonceID); err != nil {
		return VerifyResult{}, err
	}

	var userID uuid.UUID
	var role string
	err = tx.QueryRow(ctx, `
SELECT u.id, u.role
FROM wallets w
JOIN users u ON u.id = w.user_id
WHERE w.wallet_type = $1 AND w.address = $2
`, string(walletType), address).Scan(&userID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		// New user + wallet.
		err = tx.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id, role`).Scan(&userID, &role)
		if err != nil {
			return VerifyResult{}, err
		}

		_, err = tx.Exec(ctx, `
INSERT INTO wallets (user_id, wallet_type, address, public_key)
VALUES ($1, $2, $3, $4)
`, userID, string(walletType), address, nullIfEmpty(publicKey))
		if err != nil {
			return VerifyResult{}, err
		}
	} else if err != nil {
		return VerifyResult{}, err
	} else {
		// Existing wallet: update public key if provided and missing.
		if publicKey != "" {
			_, _ = tx.Exec(ctx, `
UPDATE wallets
SET public_key = COALESCE(public_key, $3)
WHERE wallet_type = $1 AND address = $2
`, string(walletType), address, publicKey)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return VerifyResult{}, err
	}

	return VerifyResult{
		User: User{ID: userID, Role: role},
		Wallet: Wallet{
			WalletType: walletType,
			Address:    address,
			PublicKey:  publicKey,
		},
	}, nil
}

func randomNonce(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Should never happen, but keep it deterministic-ish if entropy fails.
		return uuid.NewString()
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
