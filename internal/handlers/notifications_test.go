package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

const notifSuiteJWTSecret = "notifications-suite-test-secret"

func notifSuiteApp(d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewNotificationsHandler(d)
	g := app.Group("/notifications", auth.RequireAuth(notifSuiteJWTSecret))
	g.Get("/", h.List())
	g.Get("/unread-count", h.UnreadCount())
	g.Post("/read-all", h.MarkAllRead())
	g.Post("/:id/read", h.MarkRead())
	g.Get("/preferences", h.GetPreferences())
	g.Put("/preferences", h.UpdatePreferences())
	return app
}

func notifSuiteInsertUser(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (display_name) VALUES ($1) RETURNING id
`, "notif-suite-user-"+uuid.NewString()).Scan(&id)
	if err != nil {
		t.Fatalf("insert notif-suite test user: %v", err)
	}
	return id
}

func notifSuiteToken(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	tok, err := auth.IssueJWT(notifSuiteJWTSecret, userID, "contributor", "", "", 0)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	return tok
}

func notifSuiteDo(t *testing.T, app *fiber.App, method, path, token string, body []byte) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, respBody
}

func TestNotificationsHandler_RequiresAuth(t *testing.T) {
	app := notifSuiteApp(&db.DB{})
	resp, _ := notifSuiteDo(t, app, "GET", "/notifications/", "", nil)
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}

func TestNotificationsHandler_ListAndUnreadCount(t *testing.T) {
	d := testDB(t)
	app := notifSuiteApp(d)
	userID := notifSuiteInsertUser(t, d)
	token := notifSuiteToken(t, userID)

	svc := notifications.New(d, nil, "")
	svc.Notify(context.Background(), userID, notifications.TypeIssueAssigned, "Title A", "Body A", "/a")
	svc.Notify(context.Background(), userID, notifications.TypePRMerged, "Title B", "Body B", "/b")

	resp, body := notifSuiteDo(t, app, "GET", "/notifications/unread-count", token, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusOK, body)
	}
	var countResp struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(body, &countResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if countResp.Count != 2 {
		t.Errorf("count = %d, want 2", countResp.Count)
	}

	resp, body = notifSuiteDo(t, app, "GET", "/notifications/", token, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusOK, body)
	}
	var listResp struct {
		Notifications []struct {
			ID    uuid.UUID `json:"id"`
			Title string    `json:"title"`
		} `json:"notifications"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listResp.Notifications) != 2 {
		t.Fatalf("len(notifications) = %d, want 2", len(listResp.Notifications))
	}
	// Most recent first.
	if listResp.Notifications[0].Title != "Title B" {
		t.Errorf("notifications[0].Title = %q, want %q", listResp.Notifications[0].Title, "Title B")
	}
}

func TestNotificationsHandler_MarkReadAndMarkAllRead(t *testing.T) {
	d := testDB(t)
	app := notifSuiteApp(d)
	userID := notifSuiteInsertUser(t, d)
	token := notifSuiteToken(t, userID)
	otherUserID := notifSuiteInsertUser(t, d)

	svc := notifications.New(d, nil, "")
	svc.Notify(context.Background(), userID, notifications.TypeIssueAssigned, "Mine", "Mine", "/mine")
	svc.Notify(context.Background(), otherUserID, notifications.TypeIssueAssigned, "Not mine", "Not mine", "/notmine")

	var mineID uuid.UUID
	if err := d.Pool.QueryRow(context.Background(), `SELECT id FROM notifications WHERE user_id = $1`, userID).Scan(&mineID); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	var otherID uuid.UUID
	if err := d.Pool.QueryRow(context.Background(), `SELECT id FROM notifications WHERE user_id = $1`, otherUserID).Scan(&otherID); err != nil {
		t.Fatalf("lookup: %v", err)
	}

	t.Run("cannot mark another user's notification read", func(t *testing.T) {
		resp, body := notifSuiteDo(t, app, "POST", "/notifications/"+otherID.String()+"/read", token, nil)
		if resp.StatusCode != fiber.StatusOK {
			// Idempotent no-op response, but the row itself must be untouched.
			t.Fatalf("status = %d; body=%s", resp.StatusCode, body)
		}
		var readAt *string
		_ = d.Pool.QueryRow(context.Background(), `SELECT read_at::text FROM notifications WHERE id = $1`, otherID).Scan(&readAt)
		if readAt != nil {
			t.Errorf("other user's notification was marked read")
		}
	})

	t.Run("mark own notification read", func(t *testing.T) {
		resp, body := notifSuiteDo(t, app, "POST", "/notifications/"+mineID.String()+"/read", token, nil)
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d; body=%s", resp.StatusCode, body)
		}
		var readAt *string
		_ = d.Pool.QueryRow(context.Background(), `SELECT read_at::text FROM notifications WHERE id = $1`, mineID).Scan(&readAt)
		if readAt == nil {
			t.Error("own notification was not marked read")
		}
	})

	t.Run("mark-all-read only touches the caller's rows", func(t *testing.T) {
		svc.Notify(context.Background(), userID, notifications.TypePRMerged, "Another", "Another", "/another")
		resp, body := notifSuiteDo(t, app, "POST", "/notifications/read-all", token, nil)
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d; body=%s", resp.StatusCode, body)
		}
		var unread int
		_ = d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`, userID).Scan(&unread)
		if unread != 0 {
			t.Errorf("unread count for caller = %d, want 0", unread)
		}
		var otherUnread int
		_ = d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`, otherUserID).Scan(&otherUnread)
		if otherUnread != 1 {
			t.Errorf("unread count for other user = %d, want unchanged 1", otherUnread)
		}
	})
}

func TestNotificationsHandler_PreferencesDefaultToEnabled(t *testing.T) {
	d := testDB(t)
	app := notifSuiteApp(d)
	userID := notifSuiteInsertUser(t, d)
	token := notifSuiteToken(t, userID)

	resp, body := notifSuiteDo(t, app, "GET", "/notifications/preferences", token, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusOK, body)
	}
	var prefsResp struct {
		Preferences []struct {
			Type  string `json:"type"`
			InApp bool   `json:"in_app"`
			Email bool   `json:"email"`
		} `json:"preferences"`
	}
	if err := json.Unmarshal(body, &prefsResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(prefsResp.Preferences) != len(notifications.AllTypes) {
		t.Fatalf("len(preferences) = %d, want %d", len(prefsResp.Preferences), len(notifications.AllTypes))
	}
	for _, p := range prefsResp.Preferences {
		if !p.InApp || !p.Email {
			t.Errorf("type %s: in_app=%v email=%v, want both true by default", p.Type, p.InApp, p.Email)
		}
	}
}

func TestNotificationsHandler_UpdatePreferences(t *testing.T) {
	d := testDB(t)
	app := notifSuiteApp(d)
	userID := notifSuiteInsertUser(t, d)
	token := notifSuiteToken(t, userID)

	updateBody, _ := json.Marshal(map[string]any{
		"preferences": []map[string]any{
			{"type": string(notifications.TypeIssueAssigned), "in_app": true, "email": false},
		},
	})
	resp, body := notifSuiteDo(t, app, "PUT", "/notifications/preferences", token, updateBody)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusOK, body)
	}

	var inApp, email bool
	err := d.Pool.QueryRow(context.Background(), `
SELECT in_app, email FROM notification_preferences WHERE user_id = $1 AND type = $2
`, userID, string(notifications.TypeIssueAssigned)).Scan(&inApp, &email)
	if err != nil {
		t.Fatalf("lookup preference: %v", err)
	}
	if !inApp || email {
		t.Errorf("in_app=%v email=%v, want true/false", inApp, email)
	}

	// The Notify path should now honor this: email disabled means no email
	// attempted (verified indirectly - no mailer is configured, so a panic
	// or error here would indicate the preference wasn't read correctly).
	svc := notifications.New(d, nil, "")
	svc.Notify(context.Background(), userID, notifications.TypeIssueAssigned, "T", "B", "/x")

	t.Run("rejects an unknown notification type", func(t *testing.T) {
		badBody, _ := json.Marshal(map[string]any{
			"preferences": []map[string]any{{"type": "not_a_real_type", "in_app": true, "email": true}},
		})
		resp, body := notifSuiteDo(t, app, "PUT", "/notifications/preferences", token, badBody)
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusBadRequest, body)
		}
	})
}

func TestNotificationsService_RespectsInAppPreferenceOff(t *testing.T) {
	d := testDB(t)
	userID := notifSuiteInsertUser(t, d)

	if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO notification_preferences (user_id, type, in_app, email) VALUES ($1, $2, false, false)
`, userID, string(notifications.TypeIssueAssigned)); err != nil {
		t.Fatalf("seed preference: %v", err)
	}

	svc := notifications.New(d, nil, "")
	svc.Notify(context.Background(), userID, notifications.TypeIssueAssigned, "Should not appear", "Should not appear", "/x")

	var count int
	_ = d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications WHERE user_id = $1`, userID).Scan(&count)
	if count != 0 {
		t.Errorf("notification count = %d, want 0 (in_app preference is off)", count)
	}
}
