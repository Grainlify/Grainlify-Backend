package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// newSyncJobsApp wires JobsForProject behind a fiber app using a DB with no
// pool. Pagination parameters are validated before the DB availability check,
// so valid params fall through to the db check (503) while out-of-range params
// are rejected up front (400) without requiring a live database.
func newSyncJobsApp() *fiber.App {
	h := handlers.NewSyncHandler(&db.DB{})
	app := fiber.New()
	app.Get("/projects/:id/sync/jobs", h.JobsForProject())
	return app
}

const syncTestProjectPath = "/projects/11111111-1111-1111-1111-111111111111/sync/jobs"

func TestSyncJobsForProject_DefaultParamsAccepted(t *testing.T) {
	app := newSyncJobsApp()
	status, errMsg := decodeError(t, app, syncTestProjectPath)
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Equal(t, "db_not_configured", errMsg)
}

func TestSyncJobsForProject_BoundedParamsAccepted(t *testing.T) {
	app := newSyncJobsApp()
	status, errMsg := decodeError(t, app, syncTestProjectPath+"?limit=10&offset=5")
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Equal(t, "db_not_configured", errMsg)
}

func TestSyncJobsForProject_LimitAboveMaxAccepted(t *testing.T) {
	app := newSyncJobsApp()
	status, errMsg := decodeError(t, app, syncTestProjectPath+"?limit=10000")
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Equal(t, "db_not_configured", errMsg)
}

func TestSyncJobsForProject_ZeroLimitAccepted(t *testing.T) {
	app := newSyncJobsApp()
	status, errMsg := decodeError(t, app, syncTestProjectPath+"?limit=0")
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Equal(t, "db_not_configured", errMsg)
}

func TestSyncJobsForProject_NegativeOffsetRejected(t *testing.T) {
	app := newSyncJobsApp()
	status, errMsg := decodeError(t, app, syncTestProjectPath+"?offset=-3")
	assert.Equal(t, fiber.StatusBadRequest, status)
	assert.Equal(t, "offset must be non-negative", errMsg)
}

func TestSyncIdempotency_Integration(t *testing.T) {
	pool := openTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()

	// Seed user and project
	var userID uuid.UUID
	err := pool.QueryRow(ctx, `INSERT INTO users (display_name, role) VALUES ($1, $2) RETURNING id`, "Sync Owner", "contributor").Scan(&userID)
	require.NoError(t, err)

	var ecoID uuid.UUID
	err = pool.QueryRow(ctx, `INSERT INTO ecosystems (slug, name, status) VALUES ($1, $2, 'active') RETURNING id`, "sync-eco", "Sync Eco").Scan(&ecoID)
	require.NoError(t, err)

	var projectID uuid.UUID
	err = pool.QueryRow(ctx, `
		INSERT INTO projects (owner_user_id, github_full_name, status, ecosystem_id, needs_metadata)
		VALUES ($1, 'test/sync-repo', 'verified', $2, false)
		RETURNING id
	`, userID, ecoID).Scan(&projectID)
	require.NoError(t, err)

	// Clean up after the test runs
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `TRUNCATE sync_jobs, idempotency_keys, projects, ecosystems, users CASCADE`)
	})

	jwtSecret := "sync-jwt-test-secret"
	token, err := auth.IssueJWT(jwtSecret, userID, "contributor", "evm", "0x123", time.Hour)
	require.NoError(t, err)

	// Set up App
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	h := handlers.NewSyncHandler(&db.DB{Pool: pool})
	app.Post("/projects/:id/sync", auth.RequireAuth(jwtSecret), h.EnqueueFullSync())

	// Edge Case 1: Concurrent duplicate requests (same Idempotency-Key)
	t.Run("concurrent requests with same idempotency key", func(t *testing.T) {
		// Truncate jobs and idempotency keys first
		_, err := pool.Exec(ctx, "TRUNCATE sync_jobs, idempotency_keys CASCADE")
		require.NoError(t, err)

		key := "test-idempotency-key-concurrent"
		url := fmt.Sprintf("/projects/%s/sync", projectID)

		var wg sync.WaitGroup
		wg.Add(2)
		statuses := make(chan int, 2)
		errs := make(chan error, 2)

		for i := 0; i < 2; i++ {
			go func() {
				defer wg.Done()
				req := httptest.NewRequest("POST", url, nil)
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Idempotency-Key", key)
				resp, err := app.Test(req, 10000)
				if err != nil {
					errs <- err
					return
				}
				statuses <- resp.StatusCode
				resp.Body.Close()
			}()
		}

		wg.Wait()
		close(statuses)
		close(errs)

		for err := range errs {
			require.NoError(t, err)
		}

		for status := range statuses {
			assert.Equal(t, fiber.StatusAccepted, status)
		}

		// Verify sync_jobs: exactly 2 jobs should have been created (1 sync_issues, 1 sync_prs)
		var jobCount int
		err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM sync_jobs WHERE project_id = $1", projectID).Scan(&jobCount)
		require.NoError(t, err)
		assert.Equal(t, 2, jobCount)
	})

	// Edge Case 2: Retried after first completed
	t.Run("retry after first request already completed", func(t *testing.T) {
		_, err := pool.Exec(ctx, "TRUNCATE sync_jobs, idempotency_keys CASCADE")
		require.NoError(t, err)

		key := "test-idempotency-key-serial"
		url := fmt.Sprintf("/projects/%s/sync", projectID)

		// First request
		req1 := httptest.NewRequest("POST", url, nil)
		req1.Header.Set("Authorization", "Bearer "+token)
		req1.Header.Set("Idempotency-Key", key)
		resp1, err := app.Test(req1)
		require.NoError(t, err)
		assert.Equal(t, fiber.StatusAccepted, resp1.StatusCode)
		resp1.Body.Close()

		// Verify 2 sync jobs created
		var count1 int
		err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM sync_jobs").Scan(&count1)
		require.NoError(t, err)
		assert.Equal(t, 2, count1)

		// Second request (retry)
		req2 := httptest.NewRequest("POST", url, nil)
		req2.Header.Set("Authorization", "Bearer "+token)
		req2.Header.Set("Idempotency-Key", key)
		resp2, err := app.Test(req2)
		require.NoError(t, err)
		assert.Equal(t, fiber.StatusAccepted, resp2.StatusCode)
		resp2.Body.Close()

		// Verify no additional sync jobs were created
		var count2 int
		err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM sync_jobs").Scan(&count2)
		require.NoError(t, err)
		assert.Equal(t, 2, count2)
	})

	// Edge Case 3: Natural key deduplication
	t.Run("natural key deduplication inside time window", func(t *testing.T) {
		_, err := pool.Exec(ctx, "TRUNCATE sync_jobs, idempotency_keys CASCADE")
		require.NoError(t, err)

		url := fmt.Sprintf("/projects/%s/sync", projectID)

		// First request (without header)
		req1 := httptest.NewRequest("POST", url, nil)
		req1.Header.Set("Authorization", "Bearer "+token)
		resp1, err := app.Test(req1)
		require.NoError(t, err)
		assert.Equal(t, fiber.StatusAccepted, resp1.StatusCode)
		resp1.Body.Close()

		// Verify 2 sync jobs created
		var count1 int
		err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM sync_jobs").Scan(&count1)
		require.NoError(t, err)
		assert.Equal(t, 2, count1)

		// Second request (within same 5-minute window, without header)
		req2 := httptest.NewRequest("POST", url, nil)
		req2.Header.Set("Authorization", "Bearer "+token)
		resp2, err := app.Test(req2)
		require.NoError(t, err)
		assert.Equal(t, fiber.StatusAccepted, resp2.StatusCode)
		resp2.Body.Close()

		// Verify no additional sync jobs were created
		var count2 int
		err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM sync_jobs").Scan(&count2)
		require.NoError(t, err)
		assert.Equal(t, 2, count2)
	})

	// Edge Case 4: Idempotency Key Too Long
	t.Run("idempotency key too long rejection", func(t *testing.T) {
		url := fmt.Sprintf("/projects/%s/sync", projectID)
		longKey := strings.Repeat("a", 256)

		req := httptest.NewRequest("POST", url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", longKey)
		resp, err := app.Test(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		var body map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&body)
		require.NoError(t, err)
		assert.Equal(t, "idempotency_key_too_long", body["error"])
	})
}

type mockSyncRow struct {
	scanFunc func(dest ...any) error
}

func (r mockSyncRow) Scan(dest ...any) error {
	if r.scanFunc == nil {
		return pgx.ErrNoRows
	}
	return r.scanFunc(dest...)
}

type mockSyncPool struct {
	ownerUserID       uuid.UUID
	idempotencyCheck  func(key string) (int, string, error)
	insertIdempotency func(key string) (int64, error)
	syncJobsInserted  int
	mu                sync.Mutex
}

func (p *mockSyncPool) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if strings.Contains(sql, "INSERT INTO idempotency_keys") {
		key := arguments[1].(string)
		rows, err := p.insertIdempotency(key)
		if err != nil {
			return pgconn.CommandTag{}, err
		}
		return pgconn.NewCommandTag(fmt.Sprintf("INSERT %d", rows)), nil
	}
	if strings.Contains(sql, "INSERT INTO sync_jobs") {
		p.syncJobsInserted += 2 // sync_issues and sync_prs
		return pgconn.NewCommandTag("INSERT 2"), nil
	}
	if strings.Contains(sql, "DELETE FROM idempotency_keys") {
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	return pgconn.CommandTag{}, nil
}

func (p *mockSyncPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, nil
}

func (p *mockSyncPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	p.mu.Lock()
	defer p.mu.Unlock()

	if strings.Contains(sql, "SELECT owner_user_id FROM projects") {
		return mockSyncRow{
			scanFunc: func(dest ...any) error {
				if len(dest) > 0 {
					if ptr, ok := dest[0].(*uuid.UUID); ok {
						*ptr = p.ownerUserID
						return nil
					}
				}
				return fmt.Errorf("invalid destination type")
			},
		}
	}
	if strings.Contains(sql, "FROM idempotency_keys") {
		key := args[1].(string)
		status, body, err := p.idempotencyCheck(key)
		if err != nil {
			return mockSyncRow{scanFunc: func(dest ...any) error { return err }}
		}
		return mockSyncRow{
			scanFunc: func(dest ...any) error {
				if len(dest) >= 2 {
					if statusPtr, ok := dest[0].(*int); ok {
						*statusPtr = status
					}
					if bodyPtr, ok := dest[1].(*string); ok {
						*bodyPtr = body
					}
					return nil
				}
				return fmt.Errorf("invalid destination type")
			},
		}
	}
	return mockSyncRow{}
}

func (p *mockSyncPool) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	return nil, nil
}
func (p *mockSyncPool) Ping(ctx context.Context) error { return nil }
func (p *mockSyncPool) Close()                     {}
func (p *mockSyncPool) Config() *pgxpool.Config    { return nil }

func TestSyncIdempotency_Mock(t *testing.T) {
	userID := uuid.New()
	projectID := uuid.New()

	jwtSecret := "sync-jwt-test-secret"
	token, err := auth.IssueJWT(jwtSecret, userID, "contributor", "evm", "0x123", time.Hour)
	require.NoError(t, err)

	// Helper to set up the mock App
	setupMockApp := func(pool *mockSyncPool) *fiber.App {
		app := fiber.New(fiber.Config{DisableStartupMessage: true})
		h := handlers.NewSyncHandler(&db.DB{Pool: pool})
		app.Post("/projects/:id/sync", auth.RequireAuth(jwtSecret), h.EnqueueFullSync())
		return app
	}

	t.Run("first request succeeds and enqueues sync jobs", func(t *testing.T) {
		pool := &mockSyncPool{
			ownerUserID: userID,
			idempotencyCheck: func(key string) (int, string, error) {
				return 0, "", pgx.ErrNoRows // cache miss
			},
			insertIdempotency: func(key string) (int64, error) {
				return 1, nil // insert success
			},
		}

		app := setupMockApp(pool)
		req := httptest.NewRequest("POST", "/projects/"+projectID.String()+"/sync", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", "test-key-1")

		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, fiber.StatusAccepted, resp.StatusCode)
		resp.Body.Close()

		assert.Equal(t, 2, pool.syncJobsInserted)
	})

	t.Run("cached response returned on duplicate request", func(t *testing.T) {
		pool := &mockSyncPool{
			ownerUserID: userID,
			idempotencyCheck: func(key string) (int, string, error) {
				return fiber.StatusAccepted, `{"queued":true}`, nil // cache hit
			},
			insertIdempotency: func(key string) (int64, error) {
				return 0, fmt.Errorf("should not call insert")
			},
		}

		app := setupMockApp(pool)
		req := httptest.NewRequest("POST", "/projects/"+projectID.String()+"/sync", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", "test-key-2")

		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, fiber.StatusAccepted, resp.StatusCode)
		resp.Body.Close()

		assert.Equal(t, 0, pool.syncJobsInserted)
	})

	t.Run("concurrent requests - only one inserts sync jobs", func(t *testing.T) {
		// We simulate the race condition using mockSyncPool state.
		// One request will win the insert (rows affected = 1) and the other will conflict (rows affected = 0).
		var keysSeen []string
		var mu sync.Mutex

		pool := &mockSyncPool{
			ownerUserID: userID,
			idempotencyCheck: func(key string) (int, string, error) {
				return 0, "", pgx.ErrNoRows // cache miss for both
			},
			insertIdempotency: func(key string) (int64, error) {
				mu.Lock()
				defer mu.Unlock()
				for _, k := range keysSeen {
					if k == key {
						return 0, nil // Conflict (rows affected = 0)
					}
				}
				keysSeen = append(keysSeen, key)
				return 1, nil // Winner (rows affected = 1)
			},
		}

		app := setupMockApp(pool)
		key := "test-key-concurrent"
		url := "/projects/" + projectID.String() + "/sync"

		var wg sync.WaitGroup
		wg.Add(2)
		statuses := make(chan int, 2)
		errs := make(chan error, 2)

		for i := 0; i < 2; i++ {
			go func() {
				defer wg.Done()
				req := httptest.NewRequest("POST", url, nil)
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Idempotency-Key", key)
				resp, err := app.Test(req, 10000)
				if err != nil {
					errs <- err
					return
				}
				statuses <- resp.StatusCode
				resp.Body.Close()
			}()
		}

		wg.Wait()
		close(statuses)
		close(errs)

		for err := range errs {
			require.NoError(t, err)
		}
		for status := range statuses {
			assert.Equal(t, fiber.StatusAccepted, status)
		}

		// Only the winner should have inserted the jobs
		assert.Equal(t, 2, pool.syncJobsInserted)
	})

	t.Run("natural key deduplication inside window", func(t *testing.T) {
		var keysSeen []string
		var mu sync.Mutex

		pool := &mockSyncPool{
			ownerUserID: userID,
			idempotencyCheck: func(key string) (int, string, error) {
				mu.Lock()
				defer mu.Unlock()
				for _, k := range keysSeen {
					if k == key {
						return fiber.StatusAccepted, `{"queued":true}`, nil
					}
				}
				return 0, "", pgx.ErrNoRows
			},
			insertIdempotency: func(key string) (int64, error) {
				mu.Lock()
				defer mu.Unlock()
				for _, k := range keysSeen {
					if k == key {
						return 0, nil
					}
				}
				keysSeen = append(keysSeen, key)
				return 1, nil
			},
		}

		app := setupMockApp(pool)
		url := "/projects/" + projectID.String() + "/sync"

		// Request 1: should insert
		req1 := httptest.NewRequest("POST", url, nil)
		req1.Header.Set("Authorization", "Bearer "+token)
		resp1, err := app.Test(req1)
		require.NoError(t, err)
		assert.Equal(t, fiber.StatusAccepted, resp1.StatusCode)
		resp1.Body.Close()

		assert.Equal(t, 2, pool.syncJobsInserted)

		// Request 2 (within window): should check cache or conflict and NOT insert again
		req2 := httptest.NewRequest("POST", url, nil)
		req2.Header.Set("Authorization", "Bearer "+token)
		resp2, err := app.Test(req2)
		require.NoError(t, err)
		assert.Equal(t, fiber.StatusAccepted, resp2.StatusCode)
		resp2.Body.Close()

		assert.Equal(t, 2, pool.syncJobsInserted)
	})
}

