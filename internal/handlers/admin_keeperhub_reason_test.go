package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
	"github.com/jagadeesh/grainlify/backend/internal/keeperhub"
	"github.com/jagadeesh/grainlify/backend/internal/keeperhubrail"
)

// The run read's resume.reason promises to be the name the release endpoint
// refuses with. The two names live in different places - RefusalReason and
// keeperhubError - so this pins them together: a screen that says one reason
// while release says another is the drift the shared predicate exists to stop.
func TestResumeReasonMatchesTheReleaseRefusalName(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("%w: shadow mode", hackathon.ErrPayoutNotReleasable),
		fmt.Errorf("%w (got x)", keeperhubrail.ErrUnsupportedPool),
		keeperhubrail.ErrPayoutRunNotCurrent,
		keeperhubrail.ErrNotEVMChain,
		keeperhubrail.ErrSettledOnAptos,
		keeperhubrail.ErrRunFailed,
		&keeperhubrail.UnreconciledLegsError{LegIDs: []uuid.UUID{uuid.New()}},
		keeperhubrail.ErrNothingUnpaid,
		keeperhub.ErrWorkflowDisabled,
	} {
		app := fiber.New()
		app.Get("/", func(c *fiber.Ctx) error { return keeperhubError(c, err, uuid.New()) })
		res, _ := app.Test(httptest.NewRequest("GET", "/", nil), -1)
		b, _ := io.ReadAll(res.Body)
		var body map[string]any
		json.Unmarshal(b, &body)

		if want := keeperhubrail.RefusalReason(err); body["error"] != want {
			t.Errorf("%v: release answers %q, the read would say %q", err, body["error"], want)
		}
	}

	h := &AdminKeeperHubPayoutHandler{configErr: fmt.Errorf("unset")}
	app := fiber.New()
	app.Get("/", func(c *fiber.Ctx) error { return h.unavailable(c) })
	res, _ := app.Test(httptest.NewRequest("GET", "/", nil), -1)
	b, _ := io.ReadAll(res.Body)
	var body map[string]any
	json.Unmarshal(b, &body)
	if body["error"] != keeperhubrail.ReasonNotConfigured {
		t.Errorf("unconfigured release answers %q, the read would say %q", body["error"], keeperhubrail.ReasonNotConfigured)
	}
}

// Wiring the API without a database must not panic at startup - it did, once,
// and took every later test in internal/api with it.
func TestKeeperHubHandler_WithoutADatabaseAnswers503(t *testing.T) {
	h := NewAdminKeeperHubPayoutHandler(nil, config.Config{})
	app := fiber.New()
	app.Get("/admin/hackathons/:id/keeperhub/run", h.Run())
	res, err := app.Test(httptest.NewRequest("GET", "/admin/hackathons/"+uuid.NewString()+"/keeperhub/run", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
}
