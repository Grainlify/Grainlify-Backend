package github

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockRow implements pgx.Row
type mockRow struct {
	err  error
	vals []any
}

func (r *mockRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return fmt.Errorf("scan destination count mismatch: got %d, want %d", len(dest), len(r.vals))
	}
	for i, val := range r.vals {
		switch d := dest[i].(type) {
		case *int64:
			*d = val.(int64)
		case *string:
			*d = val.(string)
		case *[]byte:
			*d = val.([]byte)
		default:
			return fmt.Errorf("unsupported type in scan: %T", d)
		}
	}
	return nil
}

// mockDBPool implements db.DBPool
type mockDBPool struct {
	mu   sync.RWMutex
	data map[uuid.UUID]*mockRowData
}

type mockRowData struct {
	UserID       uuid.UUID
	GitHubUserID int64
	Login        string
	AvatarURL    string
	AccessToken  []byte
	TokenType    string
	Scope        string
}

func newMockDBPool() *mockDBPool {
	return &mockDBPool{
		data: make(map[uuid.UUID]*mockRowData),
	}
}

func (m *mockDBPool) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Simulating INSERT INTO github_accounts ... ON CONFLICT
	// arguments: userID, githubUserID, login, avatarURL, encToken, tokenType, scope
	if len(arguments) < 7 {
		return pgconn.CommandTag{}, fmt.Errorf("mockDBPool: expected at least 7 arguments for Exec, got %d", len(arguments))
	}

	userID := arguments[0].(uuid.UUID)
	githubUserID := arguments[1].(int64)
	login := arguments[2].(string)
	avatarURL := arguments[3].(string)
	encToken := arguments[4].([]byte)
	tokenType := arguments[5].(string)
	scope := arguments[6].(string)

	m.data[userID] = &mockRowData{
		UserID:       userID,
		GitHubUserID: githubUserID,
		Login:        login,
		AvatarURL:    avatarURL,
		AccessToken:  encToken,
		TokenType:    tokenType,
		Scope:        scope,
	}

	return pgconn.NewCommandTag("INSERT 1"), nil
}

func (m *mockDBPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, fmt.Errorf("not implemented in mock")
}

func (m *mockDBPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(args) == 0 {
		return &mockRow{err: fmt.Errorf("missing query argument")}
	}

	userID, ok := args[0].(uuid.UUID)
	if !ok {
		return &mockRow{err: fmt.Errorf("argument is not uuid.UUID")}
	}

	row, exists := m.data[userID]
	if !exists {
		return &mockRow{err: pgx.ErrNoRows}
	}

	// SELECT github_user_id, login, access_token
	return &mockRow{
		vals: []any{row.GitHubUserID, row.Login, row.AccessToken},
	}
}

func (m *mockDBPool) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	return nil, fmt.Errorf("not implemented in mock")
}

func (m *mockDBPool) Ping(ctx context.Context) error {
	return nil
}

func (m *mockDBPool) Close() {}

func (m *mockDBPool) Config() *pgxpool.Config {
	return nil
}

func generateTestKeyB64(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(key)
}

func TestTokenStore_Roundtrip(t *testing.T) {
	pool := newMockDBPool()
	keyB64 := generateTestKeyB64(t)
	userID := uuid.New()
	githubUserID := int64(12345)
	login := "testuser"
	avatarURL := "https://avatar.url"
	token := "gho_test_access_token_value_1234567890"
	tokenType := "bearer"
	scope := "read:user"

	ctx := context.Background()

	// 1. Store
	err := StoreLinkedAccount(ctx, pool, userID, githubUserID, login, avatarURL, token, tokenType, scope, keyB64)
	require.NoError(t, err)

	// Verify encryption directly in "database" (mock map)
	pool.mu.RLock()
	storedRow := pool.data[userID]
	pool.mu.RUnlock()
	require.NotNil(t, storedRow)
	assert.NotEqual(t, token, string(storedRow.AccessToken), "Token should be encrypted in DB")

	// 2. Read and Decrypt
	account, err := GetLinkedAccount(ctx, pool, userID, keyB64)
	require.NoError(t, err)
	assert.Equal(t, githubUserID, account.GitHubUserID)
	assert.Equal(t, login, account.Login)
	assert.Equal(t, token, account.AccessToken, "Decrypted token should match original token")
}

func TestTokenStore_DecryptFailure_LegacyOrCorrupt(t *testing.T) {
	pool := newMockDBPool()
	keyB64 := generateTestKeyB64(t)
	userID := uuid.New()
	githubUserID := int64(67890)
	login := "legacyuser"
	token := "gho_legacy_plaintext_token_not_encrypted"

	pool.mu.Lock()
	pool.data[userID] = &mockRowData{
		UserID:       userID,
		GitHubUserID: githubUserID,
		Login:        login,
		AccessToken:  []byte(token), // Manually store plaintext bytes
	}
	pool.mu.Unlock()

	ctx := context.Background()
	_, err := GetLinkedAccount(ctx, pool, userID, keyB64)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decrypt github token failed")
}

func TestTokenStore_KeyRotation(t *testing.T) {
	pool := newMockDBPool()
	keyAB64 := generateTestKeyB64(t)
	keyBB64 := generateTestKeyB64(t)
	userID := uuid.New()
	githubUserID := int64(1111)
	login := "rotuser"
	token := "gho_token_under_key_a"

	ctx := context.Background()

	// Store with Key A
	err := StoreLinkedAccount(ctx, pool, userID, githubUserID, login, "", token, "", "", keyAB64)
	require.NoError(t, err)

	// Try reading with Key B
	_, err = GetLinkedAccount(ctx, pool, userID, keyBB64)
	require.Error(t, err, "Should fail to decrypt with wrong key")
	assert.Contains(t, err.Error(), "decrypt github token failed")
}

func TestTokenStore_Concurrency(t *testing.T) {
	pool := newMockDBPool()
	keyB64 := generateTestKeyB64(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	workers := 10
	iterations := 50

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				// Use different users to simulate concurrent operations
				userID := uuid.New()
				githubUserID := int64(workerID*1000 + j)
				login := fmt.Sprintf("user-%d-%d", workerID, j)
				token := fmt.Sprintf("gho_token-%d-%d", workerID, j)

				// Store
				err := StoreLinkedAccount(ctx, pool, userID, githubUserID, login, "", token, "", "", keyB64)
				assert.NoError(t, err)

				// Read
				account, err := GetLinkedAccount(ctx, pool, userID, keyB64)
				assert.NoError(t, err)
				assert.Equal(t, token, account.AccessToken)
			}
		}(i)
	}

	wg.Wait()
}
