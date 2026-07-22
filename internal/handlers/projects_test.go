package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// Helper response envelope representation
type projectsListResponse struct {
	Projects []struct {
		ID            string `json:"id"`
		FullName      string `json:"github_full_name"`
		EcosystemName string `json:"ecosystem_name"`
	} `json:"projects"`
	Limit   int  `json:"limit"`
	Offset  int  `json:"offset"`
	Total   int  `json:"total"`
	HasMore bool `json:"has_more"`
}

// mockRows implements pgx.Rows for testing
type mockRows struct {
	rows   [][]any
	index  int
	closed bool
}

func (r *mockRows) Close() {
	r.closed = true
}

func (r *mockRows) Err() error {
	return nil
}

func (r *mockRows) CommandTag() pgconn.CommandTag {
	return pgconn.CommandTag{}
}

func (r *mockRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}

func (r *mockRows) Next() bool {
	if r.closed || r.index >= len(r.rows) {
		return false
	}
	r.index++
	return true
}

func (r *mockRows) Scan(dest ...any) error {
	if r.index <= 0 || r.index > len(r.rows) {
		return fmt.Errorf("Scan called before Next or after EOF")
	}
	row := r.rows[r.index-1]
	for i, val := range row {
		if i >= len(dest) {
			break
		}
		if err := assignValue(dest[i], val); err != nil {
			return err
		}
	}
	return nil
}

func (r *mockRows) Values() ([]any, error) {
	if r.index <= 0 || r.index > len(r.rows) {
		return nil, fmt.Errorf("Values called before Next or after EOF")
	}
	return r.rows[r.index-1], nil
}

func (r *mockRows) RawValues() [][]byte {
	return nil
}

func (r *mockRows) Conn() *pgx.Conn {
	return nil
}

// mockRow implements pgx.Row for testing
type mockRow struct {
	values []any
	err    error
}

func (r mockRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, val := range r.values {
		if i >= len(dest) {
			break
		}
		if err := assignValue(dest[i], val); err != nil {
			return err
		}
	}
	return nil
}

// mockDBPool implements db.DBPool for testing
type mockDBPool struct {
	queryFn    func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row
}

func (m *mockDBPool) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (m *mockDBPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, sql, args...)
	}
	return &mockRows{}, nil
}

func (m *mockDBPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, sql, args...)
	}
	return mockRow{}
}

func (m *mockDBPool) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	return nil, nil
}

func (m *mockDBPool) Ping(ctx context.Context) error { return nil }
func (m *mockDBPool) Close()                     {}
func (m *mockDBPool) Config() *pgxpool.Config    { return nil }

func assignValue(dest any, src any) error {
	destVal := reflect.ValueOf(dest)
	if destVal.Kind() != reflect.Ptr {
		return fmt.Errorf("destination must be a pointer")
	}
	if destVal.IsNil() {
		return nil
	}

	srcVal := reflect.ValueOf(src)
	if !srcVal.IsValid() {
		destVal.Elem().Set(reflect.Zero(destVal.Elem().Type()))
		return nil
	}

	elem := destVal.Elem()
	if srcVal.Type().AssignableTo(elem.Type()) {
		elem.Set(srcVal)
		return nil
	}

	if elem.Kind() == reflect.Ptr {
		newVal := reflect.New(elem.Type().Elem())
		newVal.Elem().Set(srcVal)
		elem.Set(newVal)
		return nil
	}

	return fmt.Errorf("cannot assign %T to %T", src, dest)
}

func createMockProjectRow(id uuid.UUID, fullName string, ecoName, ecoSlug string) []any {
	instID := "install-123"
	lang := "Go"
	tagsJSON := []byte(`["tag1", "tag2"]`)
	category := "category1"
	stars := 10
	forks := 5
	openIssues := 1
	openPRs := 2
	contributors := 3
	now := time.Now()
	desc := "My description"

	return []any{
		id,
		fullName,
		&instID,
		&lang,
		tagsJSON,
		&category,
		&stars,
		&forks,
		openIssues,
		openPRs,
		contributors,
		now,
		now,
		&ecoName,
		&ecoSlug,
		&desc,
	}
}

func newMockProjectsApp(pool db.DBPool) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	h := handlers.NewProjectsPublicHandler(config.Config{}, &db.DB{Pool: pool})
	app.Get("/projects", h.List())
	return app
}

func TestProjectsPagination_EdgeCases(t *testing.T) {
	mockDB := &mockDBPool{}

	mockDB.queryRowFn = func(ctx context.Context, sql string, args ...any) pgx.Row {
		count := 3
		for _, arg := range args {
			if s, ok := arg.(string); ok {
				if strings.EqualFold(s, "Go") {
					count = 2
				} else if strings.EqualFold(s, "Rust") {
					count = 1
				}
			}
		}
		return mockRow{values: []any{count}}
	}

	mockDB.queryFn = func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
		limitVal := args[len(args)-2].(int)
		offsetVal := args[len(args)-1].(int)

		var filtered []map[string]any
		ecosystemFilter := ""
		if len(args) > 2 {
			if s, ok := args[0].(string); ok {
				ecosystemFilter = s
			}
		}

		allProjects := []map[string]any{
			{"id": uuid.MustParse("11111111-1111-1111-1111-111111111111"), "fullName": "owner/proj1", "eco": "Go"},
			{"id": uuid.MustParse("22222222-2222-2222-2222-222222222222"), "fullName": "owner/proj2", "eco": "Go"},
			{"id": uuid.MustParse("33333333-3333-3333-3333-333333333333"), "fullName": "owner/proj3", "eco": "Rust"},
		}

		for _, p := range allProjects {
			if ecosystemFilter == "" || strings.EqualFold(p["eco"].(string), ecosystemFilter) {
				filtered = append(filtered, p)
			}
		}

		var paginated [][]any
		for i := offsetVal; i < len(filtered) && len(paginated) < limitVal; i++ {
			p := filtered[i]
			paginated = append(paginated, createMockProjectRow(p["id"].(uuid.UUID), p["fullName"].(string), p["eco"].(string), strings.ToLower(p["eco"].(string))))
		}

		return &mockRows{rows: paginated}, nil
	}

	app := newMockProjectsApp(mockDB)

	t.Run("page=0/offset=0 behaves correctly", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/projects?limit=2&offset=0", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var body projectsListResponse
		err = json.NewDecoder(resp.Body).Decode(&body)
		require.NoError(t, err)

		assert.Equal(t, 2, body.Limit)
		assert.Equal(t, 0, body.Offset)
		assert.Equal(t, 3, body.Total)
		assert.True(t, body.HasMore)
		require.Len(t, body.Projects, 2)
		assert.Equal(t, "owner/proj1", body.Projects[0].FullName)
		assert.Equal(t, "owner/proj2", body.Projects[1].FullName)
	})

	t.Run("negative offset/page parameter is rejected cleanly with 400", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/projects?limit=2&offset=-1", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		var body map[string]string
		err = json.NewDecoder(resp.Body).Decode(&body)
		require.NoError(t, err)
		assert.Equal(t, "offset must be non-negative", body["error"])
	})

	t.Run("limit exceeding max-allowed value is clamped to 200", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/projects?limit=999&offset=0", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var body projectsListResponse
		err = json.NewDecoder(resp.Body).Decode(&body)
		require.NoError(t, err)

		assert.Equal(t, 200, body.Limit)
		assert.Equal(t, 0, body.Offset)
		assert.Equal(t, 3, body.Total)
		assert.False(t, body.HasMore)
		require.Len(t, body.Projects, 3)
	})

	t.Run("last page returns partial page correctly without duplicate or empty results", func(t *testing.T) {
		// First page
		req1 := httptest.NewRequest("GET", "/projects?limit=2&offset=0", nil)
		resp1, err := app.Test(req1)
		require.NoError(t, err)
		defer resp1.Body.Close()

		var body1 projectsListResponse
		require.NoError(t, json.NewDecoder(resp1.Body).Decode(&body1))
		require.Len(t, body1.Projects, 2)

		// Last page (offset = 2)
		req2 := httptest.NewRequest("GET", "/projects?limit=2&offset=2", nil)
		resp2, err := app.Test(req2)
		require.NoError(t, err)
		defer resp2.Body.Close()

		assert.Equal(t, http.StatusOK, resp2.StatusCode)

		var body2 projectsListResponse
		err = json.NewDecoder(resp2.Body).Decode(&body2)
		require.NoError(t, err)

		assert.Equal(t, 2, body2.Limit)
		assert.Equal(t, 2, body2.Offset)
		assert.Equal(t, 3, body2.Total)
		assert.False(t, body2.HasMore)

		// Partial page should have exactly 1 item
		require.Len(t, body2.Projects, 1)
		assert.Equal(t, "owner/proj3", body2.Projects[0].FullName)

		// No duplicates
		for _, p1 := range body1.Projects {
			for _, p2 := range body2.Projects {
				assert.NotEqual(t, p1.ID, p2.ID, "last page must not contain duplicates from previous pages")
			}
		}
	})

	t.Run("filter change (by ecosystem) resets pagination state appropriately", func(t *testing.T) {
		// Go ecosystem filter
		reqGo := httptest.NewRequest("GET", "/projects?ecosystem=Go&limit=2&offset=0", nil)
		respGo, err := app.Test(reqGo)
		require.NoError(t, err)
		defer respGo.Body.Close()

		assert.Equal(t, http.StatusOK, respGo.StatusCode)
		var bodyGo projectsListResponse
		require.NoError(t, json.NewDecoder(respGo.Body).Decode(&bodyGo))

		assert.Equal(t, 2, bodyGo.Total)
		assert.False(t, bodyGo.HasMore)
		require.Len(t, bodyGo.Projects, 2)
		assert.Equal(t, "Go", bodyGo.Projects[0].EcosystemName)
		assert.Equal(t, "Go", bodyGo.Projects[1].EcosystemName)

		// Rust ecosystem filter
		reqRust := httptest.NewRequest("GET", "/projects?ecosystem=Rust&limit=2&offset=0", nil)
		respRust, err := app.Test(reqRust)
		require.NoError(t, err)
		defer respRust.Body.Close()

		assert.Equal(t, http.StatusOK, respRust.StatusCode)
		var bodyRust projectsListResponse
		require.NoError(t, json.NewDecoder(respRust.Body).Decode(&bodyRust))

		assert.Equal(t, 1, bodyRust.Total)
		assert.False(t, bodyRust.HasMore)
		require.Len(t, bodyRust.Projects, 1)
		assert.Equal(t, "Rust", bodyRust.Projects[0].EcosystemName)
	})
}

func seedProjectsDataset(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	resetLeaderboardTables(t, pool)
	ctx := t.Context()
	suffix := uuid.NewString()

	var ownerID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO users (display_name) VALUES ($1) RETURNING id`, "projects owner "+suffix).Scan(&ownerID))

	var goEcosystemID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO ecosystems (slug, name, status) VALUES ($1, $2, 'active') RETURNING id`, "go-"+suffix, "Go "+suffix).Scan(&goEcosystemID))

	var rustEcosystemID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO ecosystems (slug, name, status) VALUES ($1, $2, 'active') RETURNING id`, "rust-"+suffix, "Rust "+suffix).Scan(&rustEcosystemID))

	// Insert projects in specific creation time order to verify paging logic
	_, err := pool.Exec(ctx, `
		INSERT INTO projects (owner_user_id, github_full_name, status, ecosystem_id, needs_metadata, created_at)
		VALUES ($1, $2, 'verified', $3, false, now() - interval '2 minutes')`,
		ownerID, "owner/proj1-"+suffix, goEcosystemID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		INSERT INTO projects (owner_user_id, github_full_name, status, ecosystem_id, needs_metadata, created_at)
		VALUES ($1, $2, 'verified', $3, false, now() - interval '1 minute')`,
		ownerID, "owner/proj2-"+suffix, goEcosystemID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		INSERT INTO projects (owner_user_id, github_full_name, status, ecosystem_id, needs_metadata, created_at)
		VALUES ($1, $2, 'verified', $3, false, now())`,
		ownerID, "owner/proj3-"+suffix, rustEcosystemID)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `TRUNCATE github_issues, github_pull_requests, projects, ecosystems, github_accounts, wallets, users CASCADE`)
	})

	return goEcosystemID, rustEcosystemID
}

func TestProjectsPagination_Integration(t *testing.T) {
	pool := openTestPool(t)
	if pool == nil {
		return
	}

	goEcoID, rustEcoID := seedProjectsDataset(t, pool)

	var goEcoName, rustEcoName string
	ctx := t.Context()
	require.NoError(t, pool.QueryRow(ctx, `SELECT name FROM ecosystems WHERE id = $1`, goEcoID).Scan(&goEcoName))
	require.NoError(t, pool.QueryRow(ctx, `SELECT name FROM ecosystems WHERE id = $1`, rustEcoID).Scan(&rustEcoName))

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	h := handlers.NewProjectsPublicHandler(config.Config{}, &db.DB{Pool: pool})
	app.Get("/projects", h.List())

	t.Run("page=0/offset=0 behaves correctly", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/projects?limit=2&offset=0", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var body projectsListResponse
		err = json.NewDecoder(resp.Body).Decode(&body)
		require.NoError(t, err)

		assert.Equal(t, 2, body.Limit)
		assert.Equal(t, 0, body.Offset)
		assert.Equal(t, 3, body.Total)
		assert.True(t, body.HasMore)
		require.Len(t, body.Projects, 2)
	})

	t.Run("negative offset/page parameter is rejected cleanly with 400", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/projects?limit=2&offset=-1", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		var body map[string]string
		err = json.NewDecoder(resp.Body).Decode(&body)
		require.NoError(t, err)
		assert.Equal(t, "offset must be non-negative", body["error"])
	})

	t.Run("limit exceeding max-allowed value is clamped to 200", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/projects?limit=999&offset=0", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var body projectsListResponse
		err = json.NewDecoder(resp.Body).Decode(&body)
		require.NoError(t, err)

		assert.Equal(t, 200, body.Limit)
		assert.Equal(t, 0, body.Offset)
		assert.Equal(t, 3, body.Total)
		assert.False(t, body.HasMore)
		require.Len(t, body.Projects, 3)
	})

	t.Run("last page returns partial page correctly without duplicate or empty results", func(t *testing.T) {
		// First page
		req1 := httptest.NewRequest("GET", "/projects?limit=2&offset=0", nil)
		resp1, err := app.Test(req1)
		require.NoError(t, err)
		defer resp1.Body.Close()

		var body1 projectsListResponse
		require.NoError(t, json.NewDecoder(resp1.Body).Decode(&body1))
		require.Len(t, body1.Projects, 2)

		// Last page (offset = 2)
		req2 := httptest.NewRequest("GET", "/projects?limit=2&offset=2", nil)
		resp2, err := app.Test(req2)
		require.NoError(t, err)
		defer resp2.Body.Close()

		assert.Equal(t, http.StatusOK, resp2.StatusCode)

		var body2 projectsListResponse
		err = json.NewDecoder(resp2.Body).Decode(&body2)
		require.NoError(t, err)

		assert.Equal(t, 2, body2.Limit)
		assert.Equal(t, 2, body2.Offset)
		assert.Equal(t, 3, body2.Total)
		assert.False(t, body2.HasMore)

		// Partial page should have exactly 1 item
		require.Len(t, body2.Projects, 1)

		// No duplicates
		for _, p1 := range body1.Projects {
			for _, p2 := range body2.Projects {
				assert.NotEqual(t, p1.ID, p2.ID, "last page must not contain duplicates from previous pages")
			}
		}
	})

	t.Run("filter change (by ecosystem) resets pagination state appropriately", func(t *testing.T) {
		// Go ecosystem filter
		reqGo := httptest.NewRequest("GET", fmt.Sprintf("/projects?ecosystem=%s&limit=2&offset=0", goEcoName), nil)
		respGo, err := app.Test(reqGo)
		require.NoError(t, err)
		defer respGo.Body.Close()

		assert.Equal(t, http.StatusOK, respGo.StatusCode)
		var bodyGo projectsListResponse
		require.NoError(t, json.NewDecoder(respGo.Body).Decode(&bodyGo))

		assert.Equal(t, 2, bodyGo.Total)
		assert.False(t, bodyGo.HasMore)
		require.Len(t, bodyGo.Projects, 2)
		assert.Equal(t, goEcoName, bodyGo.Projects[0].EcosystemName)
		assert.Equal(t, goEcoName, bodyGo.Projects[1].EcosystemName)

		// Rust ecosystem filter
		reqRust := httptest.NewRequest("GET", fmt.Sprintf("/projects?ecosystem=%s&limit=2&offset=0", rustEcoName), nil)
		respRust, err := app.Test(reqRust)
		require.NoError(t, err)
		defer respRust.Body.Close()

		assert.Equal(t, http.StatusOK, respRust.StatusCode)
		var bodyRust projectsListResponse
		require.NoError(t, json.NewDecoder(respRust.Body).Decode(&bodyRust))

		assert.Equal(t, 1, bodyRust.Total)
		assert.False(t, bodyRust.HasMore)
		require.Len(t, bodyRust.Projects, 1)
		assert.Equal(t, rustEcoName, bodyRust.Projects[0].EcosystemName)
	})
}
