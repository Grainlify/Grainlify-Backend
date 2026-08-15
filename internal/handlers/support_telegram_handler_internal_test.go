package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// The reporter's 200 does not depend on Telegram.
//
// These run through the real handler against a real database, because the
// property being asserted spans both: a delivery failure must leave the row
// saved, the caller answered, and the delivery columns honest about what did
// not happen. Sink-level routing is covered in support_sink_telegram_test.go;
// this is about what the person who clicked Send experiences when the sink is
// broken, and what an operator can later tell from the row.
//
// Internal package so the test can point the sink's baseURL at httptest.
// baseURL is not settable from the environment on purpose - a configurable API
// host is a way to send the bot token somewhere else.

func telegramSupportApp(t *testing.T, d *db.DB, respond func(chatID, threadID string) (int, string)) (*fiber.App, func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		calls++
		mu.Unlock()
		status, body := respond(r.Form.Get("chat_id"), r.Form.Get("message_thread_id"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	h := NewSupportRequestsHandler(config.Config{
		JWTSecret:           "support-test-secret",
		TelegramBotToken:    "test-token",
		TelegramChatID:      "-100999",
		TelegramAdminUserID: "42",
		TelegramTopicBugs:   "1700",
		TelegramTopicKYC:    "1701",
	}, d)

	found := false
	for _, s := range h.sinks {
		if tg, ok := s.(*telegramSupportSink); ok {
			tg.baseURL = srv.URL
			found = true
		}
	}
	if !found {
		t.Fatal("the handler built no telegram sink; the rest of this test would pass for the wrong reason")
	}

	app := fiber.New()
	app.Post("/support-requests", h.Create())
	return app, func() int { mu.Lock(); defer mu.Unlock(); return calls }
}

func postTelegramSupport(t *testing.T, app *fiber.App, body string) (int, uuid.UUID) {
	t.Helper()
	req := httptest.NewRequest("POST", "/support-requests", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, 15000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	idStr, _ := out["support_id"].(string)
	id, _ := uuid.Parse(idStr)
	return resp.StatusCode, id
}

type supportDeliveryRow struct {
	Category            string
	DiscordAt           *time.Time
	TopicAt             *time.Time
	AdminDMAt           *time.Time
	RoutedToFallback    bool
	MessageWasPersisted bool
}

func readDeliveryRow(t *testing.T, d *db.DB, id uuid.UUID) supportDeliveryRow {
	t.Helper()
	var row supportDeliveryRow
	var message string
	if err := d.Pool.QueryRow(t.Context(), `
SELECT category, message,
       discord_delivered_at,
       telegram_delivered_at,
       telegram_admin_dm_delivered_at,
       telegram_routed_to_fallback
FROM support_requests WHERE id = $1`, id,
	).Scan(&row.Category, &message, &row.DiscordAt, &row.TopicAt, &row.AdminDMAt, &row.RoutedToFallback); err != nil {
		t.Fatalf("row was not saved: %v", err)
	}
	row.MessageWasPersisted = message != ""
	return row
}

// The case the whole persist-first design exists for: the KYC DM is attempted
// first and 403s because the admin never started a chat with the bot. Nobody is
// notified - and the reporter must still be told their report went through,
// because it did.
func TestSupportHandler_AdminDMForbiddenStillReturns200AndSavesTheRow(t *testing.T) {
	d := dbtest.DB(t)
	app, callCount := telegramSupportApp(t, d, func(chatID, threadID string) (int, string) {
		return 403, `{"ok":false,"error_code":403,"description":"Forbidden: bot can't initiate conversation with a user"}`
	})

	code, id := postTelegramSupport(t, app,
		`{"category":"kyc","message":"my verification has been in_review for six days","page_url":"https://grainlify.com/settings"}`)

	if code != fiber.StatusOK {
		t.Fatalf("returned %d; a failed DM is our problem, not the reporter's", code)
	}
	if id == uuid.Nil {
		t.Fatal("no support_id returned")
	}

	row := readDeliveryRow(t, d, id)
	if !row.MessageWasPersisted {
		t.Error("the message was not saved")
	}
	// Neither column: the request reached nobody, and the row must say so
	// rather than look delivered.
	if row.AdminDMAt != nil {
		t.Errorf("telegram_admin_dm_delivered_at = %v after a 403", *row.AdminDMAt)
	}
	if row.TopicAt != nil {
		t.Errorf("telegram_delivered_at = %v; KYC never posts to a topic", *row.TopicAt)
	}
	if row.RoutedToFallback {
		t.Error("telegram_routed_to_fallback = true; a DM has no fallback to route to")
	}
	if callCount() != 1 {
		t.Errorf("%d telegram calls; a forbidden DM must not retry into the public group", callCount())
	}
}

func TestSupportHandler_KYCSuccessStampsOnlyTheAdminDMColumn(t *testing.T) {
	d := dbtest.DB(t)
	app, _ := telegramSupportApp(t, d, func(chatID, threadID string) (int, string) {
		if chatID != "42" || threadID != "" {
			t.Errorf("KYC was sent to chat_id=%q thread=%q, want the admin DM", chatID, threadID)
		}
		return 200, `{"ok":true,"result":{"message_id":1}}`
	})

	code, id := postTelegramSupport(t, app, `{"category":"kyc","message":"verification refused, no reason given"}`)
	if code != fiber.StatusOK {
		t.Fatalf("returned %d", code)
	}

	row := readDeliveryRow(t, d, id)
	if row.AdminDMAt == nil {
		t.Error("telegram_admin_dm_delivered_at is NULL after a successful DM")
	}
	if row.TopicAt != nil {
		t.Error("telegram_delivered_at was stamped for a KYC request; the two must never share a column")
	}
	// supportDelivered is the single definition of the rule - assert the row
	// satisfies it rather than restating the rule here.
	if !supportTelegramDelivered("kyc", row.TopicAt, row.AdminDMAt) {
		t.Error("supportTelegramDelivered says this KYC row is undelivered")
	}
}

// A dead topic must not lose a report, and the row must record that it landed
// somewhere other than where it was addressed.
func TestSupportHandler_TopicFailureFallsBackAndRecordsIt(t *testing.T) {
	d := dbtest.DB(t)
	app, callCount := telegramSupportApp(t, d, func(chatID, threadID string) (int, string) {
		if threadID != "" {
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: message thread not found"}`
		}
		return 200, `{"ok":true,"result":{"message_id":2}}`
	})

	code, id := postTelegramSupport(t, app, `{"category":"bug","message":"the rank badge is unreadable in light mode"}`)
	if code != fiber.StatusOK {
		t.Fatalf("returned %d", code)
	}

	row := readDeliveryRow(t, d, id)
	if row.TopicAt == nil {
		t.Error("telegram_delivered_at is NULL; the General fallback did reach a human and counts as delivered")
	}
	if !row.RoutedToFallback {
		t.Error("telegram_routed_to_fallback = false; a report sitting in General while its topic is empty is invisible otherwise")
	}
	if row.AdminDMAt != nil {
		t.Error("a bug report stamped the KYC admin-DM column")
	}
	if callCount() != 2 {
		t.Errorf("%d telegram calls, want the topic attempt plus the General retry", callCount())
	}
}

// A clean bug report leaves the fallback flag false - otherwise the flag above
// would be meaningless.
func TestSupportHandler_CleanTopicDeliveryIsNotMarkedAsFallback(t *testing.T) {
	d := dbtest.DB(t)
	app, _ := telegramSupportApp(t, d, func(chatID, threadID string) (int, string) {
		if chatID != "-100999" || threadID != "1700" {
			t.Errorf("bug went to chat_id=%q thread=%q, want the group's bugs topic", chatID, threadID)
		}
		return 200, `{"ok":true,"result":{"message_id":3}}`
	})

	code, id := postTelegramSupport(t, app, `{"category":"bug","message":"pagination resets on the second page"}`)
	if code != fiber.StatusOK {
		t.Fatalf("returned %d", code)
	}
	row := readDeliveryRow(t, d, id)
	if row.TopicAt == nil {
		t.Error("telegram_delivered_at is NULL after a successful topic post")
	}
	if row.RoutedToFallback {
		t.Error("a correctly routed post was recorded as a fallback")
	}
}
