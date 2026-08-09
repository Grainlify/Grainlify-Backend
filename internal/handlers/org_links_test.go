package handlers_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

const orgLinksSuiteJWTSecret = "org-links-suite-test-secret"

func orgLinksSuiteApp(cfg config.Config, d *db.DB) *fiber.App {
	h := handlers.NewOrgLinksHandler(d)
	app := fiber.New()
	app.Get("/orgs/:login/links", h.Get())
	app.Put("/orgs/:login/links", auth.RequireAuth(cfg.JWTSecret), h.Update())
	return app
}

func orgLinksSuiteToken(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	tok, err := auth.IssueJWT(orgLinksSuiteJWTSecret, userID, "contributor", "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	return tok
}

type orgLinksResponse struct {
	Telegram *string `json:"telegram"`
	LinkedIn *string `json:"linkedin"`
	WhatsApp *string `json:"whatsapp"`
	Twitter  *string `json:"twitter"`
	Discord  *string `json:"discord"`
}

func TestOrgLinksHandler_Get_AllNilWhenNeverConfigured(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: orgLinksSuiteJWTSecret}
	app := orgLinksSuiteApp(cfg, d)

	resp, body := notifSuiteDo(t, app, "GET", "/orgs/never-configured-"+uuid.New().String()[:8]+"/links", "", nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.StatusCode, body)
	}
	var got orgLinksResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, body)
	}
	if got.Telegram != nil || got.LinkedIn != nil || got.WhatsApp != nil || got.Twitter != nil || got.Discord != nil {
		t.Errorf("expected all-nil links, got %+v", got)
	}
}

func TestOrgLinksHandler_Update_RequiresAuth(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: orgLinksSuiteJWTSecret}
	app := orgLinksSuiteApp(cfg, d)

	resp, _ := notifSuiteDo(t, app, "PUT", "/orgs/some-org/links", "", []byte(`{"telegram":"foo"}`))
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestOrgLinksHandler_Update_ForbiddenForNonOwner(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: orgLinksSuiteJWTSecret}
	app := orgLinksSuiteApp(cfg, d)

	userID := projectsFxUser(t, d.Pool)
	token := orgLinksSuiteToken(t, userID)
	orgLogin := "not-mine-" + uuid.New().String()[:8]

	resp, body := notifSuiteDo(t, app, "PUT", "/orgs/"+orgLogin+"/links", token, []byte(`{"telegram":"foo"}`))
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", resp.StatusCode, body)
	}
}

func TestOrgLinksHandler_Update_OwnerCanSetThenPartiallyUpdateThenClear(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: orgLinksSuiteJWTSecret}
	app := orgLinksSuiteApp(cfg, d)

	ownerID := projectsFxUser(t, d.Pool)
	token := orgLinksSuiteToken(t, ownerID)
	orgLogin := "links-org-" + uuid.New().String()[:8]
	projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID:    ownerID,
		GitHubFullName: orgLogin + "/repo-" + uuid.New().String()[:8],
	})

	// Initial set: two fields, INSERT path (no existing row).
	resp1, body1 := notifSuiteDo(t, app, "PUT", "/orgs/"+orgLogin+"/links", token, []byte(`{"telegram":"acme_tg","linkedin":"acme"}`))
	if resp1.StatusCode != fiber.StatusOK {
		t.Fatalf("initial set status = %d, want 200, body = %s", resp1.StatusCode, body1)
	}

	resp2, body2 := notifSuiteDo(t, app, "GET", "/orgs/"+orgLogin+"/links", "", nil)
	if resp2.StatusCode != fiber.StatusOK {
		t.Fatalf("get status = %d, want 200, body = %s", resp2.StatusCode, body2)
	}
	var got orgLinksResponse
	if err := json.Unmarshal(body2, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Telegram == nil || *got.Telegram != "acme_tg" {
		t.Errorf("telegram = %v, want acme_tg", got.Telegram)
	}
	if got.LinkedIn == nil || *got.LinkedIn != "acme" {
		t.Errorf("linkedin = %v, want acme", got.LinkedIn)
	}

	// Partial update: only twitter provided - telegram/linkedin must survive untouched (UPDATE path).
	resp3, body3 := notifSuiteDo(t, app, "PUT", "/orgs/"+orgLogin+"/links", token, []byte(`{"twitter":"acme_hq"}`))
	if resp3.StatusCode != fiber.StatusOK {
		t.Fatalf("partial update status = %d, want 200, body = %s", resp3.StatusCode, body3)
	}
	_, body4 := notifSuiteDo(t, app, "GET", "/orgs/"+orgLogin+"/links", "", nil)
	var got2 orgLinksResponse
	if err := json.Unmarshal(body4, &got2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got2.Telegram == nil || *got2.Telegram != "acme_tg" {
		t.Errorf("after partial update, telegram = %v, want unchanged acme_tg", got2.Telegram)
	}
	if got2.Twitter == nil || *got2.Twitter != "acme_hq" {
		t.Errorf("twitter = %v, want acme_hq", got2.Twitter)
	}

	// Clear: explicit empty string must NULL the column, not leave it or error.
	resp5, body5 := notifSuiteDo(t, app, "PUT", "/orgs/"+orgLogin+"/links", token, []byte(`{"telegram":""}`))
	if resp5.StatusCode != fiber.StatusOK {
		t.Fatalf("clear status = %d, want 200, body = %s", resp5.StatusCode, body5)
	}
	_, body6 := notifSuiteDo(t, app, "GET", "/orgs/"+orgLogin+"/links", "", nil)
	var got3 orgLinksResponse
	if err := json.Unmarshal(body6, &got3); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got3.Telegram != nil {
		t.Errorf("after clear, telegram = %v, want nil", got3.Telegram)
	}
	if got3.LinkedIn == nil || *got3.LinkedIn != "acme" {
		t.Errorf("linkedin should still survive the clear-telegram request, got %v", got3.LinkedIn)
	}
}

func TestOrgLinksHandler_Update_RejectsOverlongValue(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: orgLinksSuiteJWTSecret}
	app := orgLinksSuiteApp(cfg, d)

	ownerID := projectsFxUser(t, d.Pool)
	token := orgLinksSuiteToken(t, ownerID)
	orgLogin := "long-link-org-" + uuid.New().String()[:8]
	projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID:    ownerID,
		GitHubFullName: orgLogin + "/repo-" + uuid.New().String()[:8],
	})

	tooLong := make([]byte, 0, 260)
	for i := 0; i < 250; i++ {
		tooLong = append(tooLong, 'a')
	}
	payload := []byte(`{"discord":"` + string(tooLong) + `"}`)

	resp, body := notifSuiteDo(t, app, "PUT", "/orgs/"+orgLogin+"/links", token, payload)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
}
