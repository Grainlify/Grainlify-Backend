package handlers_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// projectsLedDelayedTransport simulates GitHub API latency for every repo
// metadata request, so the test below can prove ProjectsLed's total wall
// clock time no longer scales linearly with the number of owned projects
// (issue #290).
type projectsLedDelayedTransport struct {
	delay time.Duration
}

func (d *projectsLedDelayedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	time.Sleep(d.delay)
	body := `{"id":1,"full_name":"owner/repo","private":false,"owner":{"login":"owner","avatar_url":"https://avatars.githubusercontent.com/u/1"}}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

// TestProjectsLed_ConcurrentRepoFetchIsFasterThanSequential is an end-to-end
// regression test proving ProjectsLed's wall-clock time for N owned
// projects no longer scales linearly with N, per issue #290's acceptance
// criteria. github.NewClient() builds its own *http.Client with no
// injectable seam, so http.DefaultTransport is swapped for the test's
// duration and restored via t.Cleanup.
func TestProjectsLed_ConcurrentRepoFetchIsFasterThanSequential(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	var ownerID string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('maintainer') RETURNING id`).Scan(&ownerID))
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, ownerID) })

	const n = 5
	const delay = 50 * time.Millisecond
	suffix := strings.ReplaceAll(fmt.Sprintf("%v", time.Now()), " ", "")
	projectIDs := make([]string, n)
	for i := 0; i < n; i++ {
		fullName := fmt.Sprintf("owner/led-repo-%d-%s", i, suffix)
		var projectID string
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO projects (owner_user_id, github_full_name, status)
			VALUES ($1, $2, 'verified') RETURNING id
		`, ownerID, fullName).Scan(&projectID))
		projectIDs[i] = projectID
	}
	t.Cleanup(func() {
		for _, id := range projectIDs {
			_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id)
		}
	})

	origTransport := http.DefaultTransport
	http.DefaultTransport = &projectsLedDelayedTransport{delay: delay}
	t.Cleanup(func() { http.DefaultTransport = origTransport })

	h := handlers.NewUserProfileHandler(config.Config{}, &db.DB{Pool: pool})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/projects-led", func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, ownerID)
		return h.ProjectsLed()(c)
	})

	start := time.Now()
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/projects-led", nil), -1)
	elapsed := time.Since(start)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	// Sequential fetches would take ~n*delay (250ms); with the concurrent
	// fetch (n <= maxConcurrentRepoFetches) this should stay close to one
	// delay period.
	if elapsed > delay*3 {
		t.Fatalf("ProjectsLed() with %d rows and %v GitHub latency each took %v, want well under the sequential time of %v", n, delay, elapsed, delay*time.Duration(n))
	}
}
