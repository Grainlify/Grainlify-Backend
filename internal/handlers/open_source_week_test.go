package handlers_test

import (
	"context"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// openSourceWeekPublicSuiteApp mounts the public open-source-week routes
// exactly as internal/api/api.go wires OpenSourceWeekHandler: no auth.
func openSourceWeekPublicSuiteApp(d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewOpenSourceWeekHandler(d)
	app.Get("/open-source-week/events", h.ListPublic())
	app.Get("/open-source-week/events/:id", h.GetPublic())
	return app
}

// openSourceWeekAdminSuiteApp mounts /admin/open-source-week/events...
// exactly as internal/api/api.go wires OpenSourceWeekAdminHandler:
// RequireAuth + RequireRole admin on every route. Reuses
// adminSuiteJWTSecret / adminSuiteInsertUser / adminSuiteToken /
// adminSuiteDo from admin_test.go.
func openSourceWeekAdminSuiteApp(d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewOpenSourceWeekAdminHandler(d)
	adminGroup := app.Group("/admin", auth.RequireAuth(adminSuiteJWTSecret))
	adminGroup.Get("/open-source-week/events", auth.RequireRole("admin"), h.List())
	adminGroup.Post("/open-source-week/events", auth.RequireRole("admin"), h.Create())
	adminGroup.Delete("/open-source-week/events/:id", auth.RequireRole("admin"), h.Delete())
	return app
}

// oswSuiteInsertEvent inserts a uniquely-titled open_source_week_events row
// directly via SQL with the given status and returns its id.
func oswSuiteInsertEvent(t *testing.T, d *db.DB, status string) uuid.UUID {
	t.Helper()
	title := "OSW Suite Event " + uuid.NewString()
	start := time.Now().Add(24 * time.Hour).UTC()
	end := start.Add(48 * time.Hour)

	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO open_source_week_events (title, description, location, status, start_at, end_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id
`, title, "suite-generated description", "remote", status, start, end).Scan(&id)
	if err != nil {
		t.Fatalf("insert osw-suite test event: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Public handlers: ListPublic / GetPublic
// ---------------------------------------------------------------------------

func TestOpenSourceWeekListPublic_FiltersDraft(t *testing.T) {
	d := testDB(t)
	app := openSourceWeekPublicSuiteApp(d)

	visibleID := oswSuiteInsertEvent(t, d, "upcoming")
	draftID := oswSuiteInsertEvent(t, d, "draft")

	status, body := adminSuiteDo(t, app, "GET", "/open-source-week/events", "", nil)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want %d", status, fiber.StatusOK)
	}
	events, ok := body["events"].([]any)
	if !ok {
		t.Fatalf("expected \"events\" to be an array, got %v (%T)", body["events"], body["events"])
	}

	var sawVisible, sawDraft bool
	for _, raw := range events {
		ev, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch ev["id"] {
		case visibleID.String():
			sawVisible = true
		case draftID.String():
			sawDraft = true
		}
	}
	if !sawVisible {
		t.Errorf("expected non-draft event %s in public list, was absent", visibleID)
	}
	if sawDraft {
		t.Errorf("draft event %s leaked into public list", draftID)
	}
}

func TestOpenSourceWeekGetPublic(t *testing.T) {
	d := testDB(t)
	app := openSourceWeekPublicSuiteApp(d)

	t.Run("nonexistent id is 404", func(t *testing.T) {
		status, body := adminSuiteDo(t, app, "GET", "/open-source-week/events/"+uuid.NewString(), "", nil)
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want %d", status, fiber.StatusNotFound)
		}
		if body["error"] != "event_not_found" {
			t.Errorf("error = %v, want event_not_found", body["error"])
		}
	})

	t.Run("malformed id is 400", func(t *testing.T) {
		status, body := adminSuiteDo(t, app, "GET", "/open-source-week/events/not-a-uuid", "", nil)
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want %d", status, fiber.StatusBadRequest)
		}
		if body["error"] != "invalid_event_id" {
			t.Errorf("error = %v, want invalid_event_id", body["error"])
		}
	})

	t.Run("draft event is also filtered from direct lookup", func(t *testing.T) {
		draftID := oswSuiteInsertEvent(t, d, "draft")
		status, body := adminSuiteDo(t, app, "GET", "/open-source-week/events/"+draftID.String(), "", nil)
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want %d", status, fiber.StatusNotFound)
		}
		if body["error"] != "event_not_found" {
			t.Errorf("error = %v, want event_not_found", body["error"])
		}
	})

	t.Run("non-draft event is returned", func(t *testing.T) {
		visibleID := oswSuiteInsertEvent(t, d, "upcoming")
		status, body := adminSuiteDo(t, app, "GET", "/open-source-week/events/"+visibleID.String(), "", nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want %d", status, fiber.StatusOK)
		}
		ev, ok := body["event"].(map[string]any)
		if !ok {
			t.Fatalf("expected \"event\" object in response, got %v", body)
		}
		if ev["id"] != visibleID.String() {
			t.Errorf("event id = %v, want %v", ev["id"], visibleID.String())
		}
		if ev["status"] != "upcoming" {
			t.Errorf("event status = %v, want upcoming", ev["status"])
		}
	})
}

// ---------------------------------------------------------------------------
// Admin handlers: List / Create / Delete
// ---------------------------------------------------------------------------

func TestOpenSourceWeekAdminList_RBAC(t *testing.T) {
	d := testDB(t)
	app := openSourceWeekAdminSuiteApp(d)

	t.Run("no auth is rejected", func(t *testing.T) {
		status, _ := adminSuiteDo(t, app, "GET", "/admin/open-source-week/events", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("authenticated non-admin is forbidden", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "contributor")
		tok := adminSuiteToken(t, uid, "contributor")
		status, _ := adminSuiteDo(t, app, "GET", "/admin/open-source-week/events", tok, nil)
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want %d", status, fiber.StatusForbidden)
		}
	})

	t.Run("authenticated admin succeeds", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "admin")
		tok := adminSuiteToken(t, uid, "admin")
		status, body := adminSuiteDo(t, app, "GET", "/admin/open-source-week/events", tok, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want %d", status, fiber.StatusOK)
		}
		if _, ok := body["events"]; !ok {
			t.Errorf("expected \"events\" key in response, got %v", body)
		}
	})
}

func validOSWCreatePayload() map[string]string {
	start := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	end := time.Now().Add(96 * time.Hour).UTC().Format(time.RFC3339)
	return map[string]string{
		"title":       "OSW Admin Create " + uuid.NewString(),
		"description": "created by admin_ecosystems suite",
		"location":    "remote",
		"status":      "upcoming",
		"start_at":    start,
		"end_at":      end,
	}
}

func TestOpenSourceWeekAdminCreate_RBAC(t *testing.T) {
	d := testDB(t)
	app := openSourceWeekAdminSuiteApp(d)

	t.Run("no auth is rejected", func(t *testing.T) {
		status, _ := adminSuiteDo(t, app, "POST", "/admin/open-source-week/events", "", validOSWCreatePayload())
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("authenticated non-admin is forbidden", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "maintainer")
		tok := adminSuiteToken(t, uid, "maintainer")
		status, _ := adminSuiteDo(t, app, "POST", "/admin/open-source-week/events", tok, validOSWCreatePayload())
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want %d", status, fiber.StatusForbidden)
		}
	})

	t.Run("authenticated admin succeeds", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "admin")
		tok := adminSuiteToken(t, uid, "admin")
		status, body := adminSuiteDo(t, app, "POST", "/admin/open-source-week/events", tok, validOSWCreatePayload())
		if status != fiber.StatusCreated {
			t.Fatalf("status = %d, want %d (body=%v)", status, fiber.StatusCreated, body)
		}
		if id, _ := body["id"].(string); id == "" {
			t.Errorf("expected non-empty id in response, got %v", body)
		}
	})
}

func TestOpenSourceWeekAdminCreate_Validation(t *testing.T) {
	d := testDB(t)
	app := openSourceWeekAdminSuiteApp(d)
	uid := adminSuiteInsertUser(t, d, "admin")
	tok := adminSuiteToken(t, uid, "admin")

	t.Run("missing title is rejected", func(t *testing.T) {
		payload := validOSWCreatePayload()
		delete(payload, "title")
		status, body := adminSuiteDo(t, app, "POST", "/admin/open-source-week/events", tok, payload)
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want %d", status, fiber.StatusBadRequest)
		}
		if body["error"] != "title_required" {
			t.Errorf("error = %v, want title_required", body["error"])
		}
	})

	t.Run("invalid status is rejected", func(t *testing.T) {
		payload := validOSWCreatePayload()
		payload["status"] = "cancelled"
		status, body := adminSuiteDo(t, app, "POST", "/admin/open-source-week/events", tok, payload)
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want %d", status, fiber.StatusBadRequest)
		}
		if body["error"] != "invalid_status" {
			t.Errorf("error = %v, want invalid_status", body["error"])
		}
	})

	t.Run("invalid start_at is rejected", func(t *testing.T) {
		payload := validOSWCreatePayload()
		payload["start_at"] = "not-a-timestamp"
		status, body := adminSuiteDo(t, app, "POST", "/admin/open-source-week/events", tok, payload)
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want %d", status, fiber.StatusBadRequest)
		}
		if body["error"] != "invalid_start_at" {
			t.Errorf("error = %v, want invalid_start_at", body["error"])
		}
	})

	t.Run("invalid end_at is rejected", func(t *testing.T) {
		payload := validOSWCreatePayload()
		payload["end_at"] = "not-a-timestamp"
		status, body := adminSuiteDo(t, app, "POST", "/admin/open-source-week/events", tok, payload)
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want %d", status, fiber.StatusBadRequest)
		}
		if body["error"] != "invalid_end_at" {
			t.Errorf("error = %v, want invalid_end_at", body["error"])
		}
	})

	t.Run("end_at before start_at is rejected", func(t *testing.T) {
		payload := validOSWCreatePayload()
		payload["start_at"], payload["end_at"] = payload["end_at"], payload["start_at"]
		status, body := adminSuiteDo(t, app, "POST", "/admin/open-source-week/events", tok, payload)
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want %d", status, fiber.StatusBadRequest)
		}
		if body["error"] != "end_at_must_be_after_start_at" {
			t.Errorf("error = %v, want end_at_must_be_after_start_at", body["error"])
		}
	})
}

func TestOpenSourceWeekAdminDelete(t *testing.T) {
	d := testDB(t)
	adminApp := openSourceWeekAdminSuiteApp(d)
	publicApp := openSourceWeekPublicSuiteApp(d)

	t.Run("no auth is rejected", func(t *testing.T) {
		status, _ := adminSuiteDo(t, adminApp, "DELETE", "/admin/open-source-week/events/"+uuid.NewString(), "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("authenticated non-admin is forbidden", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "contributor")
		tok := adminSuiteToken(t, uid, "contributor")
		status, _ := adminSuiteDo(t, adminApp, "DELETE", "/admin/open-source-week/events/"+uuid.NewString(), tok, nil)
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want %d", status, fiber.StatusForbidden)
		}
	})

	t.Run("admin deletes an existing event and it 404s afterward", func(t *testing.T) {
		adminUID := adminSuiteInsertUser(t, d, "admin")
		adminTok := adminSuiteToken(t, adminUID, "admin")
		eventID := oswSuiteInsertEvent(t, d, "upcoming")

		delStatus, delBody := adminSuiteDo(t, adminApp, "DELETE", "/admin/open-source-week/events/"+eventID.String(), adminTok, nil)
		if delStatus != fiber.StatusOK {
			t.Fatalf("delete status = %d, want %d (body=%v)", delStatus, fiber.StatusOK, delBody)
		}
		if delBody["ok"] != true {
			t.Errorf("delete ok = %v, want true", delBody["ok"])
		}

		// Cross-check removal through the public handler too.
		getStatus, getBody := adminSuiteDo(t, publicApp, "GET", "/open-source-week/events/"+eventID.String(), "", nil)
		if getStatus != fiber.StatusNotFound {
			t.Errorf("post-delete public GET status = %d, want %d", getStatus, fiber.StatusNotFound)
		}
		if getBody["error"] != "event_not_found" {
			t.Errorf("post-delete public GET error = %v, want event_not_found", getBody["error"])
		}
	})

	t.Run("admin gets 404 deleting a nonexistent event", func(t *testing.T) {
		adminUID := adminSuiteInsertUser(t, d, "admin")
		adminTok := adminSuiteToken(t, adminUID, "admin")
		status, body := adminSuiteDo(t, adminApp, "DELETE", "/admin/open-source-week/events/"+uuid.NewString(), adminTok, nil)
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want %d", status, fiber.StatusNotFound)
		}
		if body["error"] != "event_not_found" {
			t.Errorf("error = %v, want event_not_found", body["error"])
		}
	})
}
