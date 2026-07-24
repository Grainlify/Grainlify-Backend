package github

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/cryptox"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

type LinkedAccount struct {
	GitHubUserID int64
	Login        string
	AccessToken  string
}

func GetLinkedAccount(ctx context.Context, pool db.DBPool, userID uuid.UUID, tokenEncKeyB64 string) (LinkedAccount, error) {
	if pool == nil {
		return LinkedAccount{}, fmt.Errorf("db not configured")
	}

	var githubUserID int64
	var login string
	var encToken []byte
	err := pool.QueryRow(ctx, `
SELECT github_user_id, login, access_token
FROM github_accounts
WHERE user_id = $1
`, userID).Scan(&githubUserID, &login, &encToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return LinkedAccount{}, fmt.Errorf("github_not_linked")
	}
	if err != nil {
		return LinkedAccount{}, err
	}

	key, err := cryptox.KeyFromB64(tokenEncKeyB64)
	if err != nil {
		return LinkedAccount{}, err
	}
	tokenBytes, err := cryptox.DecryptAESGCM(key, encToken)
	if err != nil {
		return LinkedAccount{}, fmt.Errorf("decrypt github token failed")
	}

	return LinkedAccount{
		GitHubUserID: githubUserID,
		Login:        login,
		AccessToken:  string(tokenBytes),
	}, nil
}

func StoreLinkedAccount(ctx context.Context, pool db.DBPool, userID uuid.UUID, githubUserID int64, login, avatarURL, accessToken, tokenType, scope string, tokenEncKeyB64 string) error {
	if pool == nil {
		return fmt.Errorf("db not configured")
	}

	key, err := cryptox.KeyFromB64(tokenEncKeyB64)
	if err != nil {
		return fmt.Errorf("key from b64: %w", err)
	}

	encToken, err := cryptox.EncryptAESGCM(key, []byte(accessToken))
	if err != nil {
		return fmt.Errorf("encrypt token: %w", err)
	}

	_, err = pool.Exec(ctx, `
INSERT INTO github_accounts (user_id, github_user_id, login, avatar_url, access_token, token_type, scope, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now())
ON CONFLICT (user_id) DO UPDATE SET
  github_user_id = EXCLUDED.github_user_id,
  login = EXCLUDED.login,
  avatar_url = EXCLUDED.avatar_url,
  access_token = EXCLUDED.access_token,
  token_type = EXCLUDED.token_type,
  scope = EXCLUDED.scope,
  updated_at = now()
`, userID, githubUserID, login, avatarURL, encToken, tokenType, scope)
	if err != nil {
		return fmt.Errorf("store github account: %w", err)
	}

	return nil
}

