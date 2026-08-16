package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// What reaches a PUBLIC group, and what must never.
//
// @Grainlify is readable without joining. KYC requests name individuals and
// mention refusals, so they go to the admin's DM and are not posted at all -
// an earlier design posted a "replied privately" stub, which disclosed that
// somebody asked a verification question at a timestamp and was correlatable
// against anything else in the group at that moment.

type capturedSend struct {
	ChatID   string
	ThreadID string
	Text     string

	// Set for multipart uploads (sendPhoto / sendDocument). Method is the Bot
	// API method from the URL path, so a test can assert WHICH call was made
	// rather than only that something was sent.
	Method    string
	FileField string
	FileName  string
	FileBytes int
	Caption   string
	ReplyTo   string
}

// fakeTelegram stands in for the Bot API. respond decides each reply, so a
// test can make one call fail without affecting the next.
func fakeTelegram(
	t *testing.T,
	respond func(c capturedSend) (int, string),
	overrides ...func(*telegramSinkConfig),
) (*telegramSupportSink, *[]capturedSend, func()) {
	t.Helper()
	var mu sync.Mutex
	sends := []capturedSend{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := capturedSend{}
		if i := strings.LastIndex(r.URL.Path, "/"); i >= 0 {
			c.Method = r.URL.Path[i+1:]
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			// sendPhoto / sendDocument. ParseForm does not read these, so a
			// harness that only called it would record an upload as an empty
			// send and every assertion about it would be vacuous.
			if err := r.ParseMultipartForm(16 << 20); err == nil {
				c.ChatID = r.FormValue("chat_id")
				c.ThreadID = r.FormValue("message_thread_id")
				c.Caption = r.FormValue("caption")
				c.ReplyTo = r.FormValue("reply_to_message_id")
				for field, fhs := range r.MultipartForm.File {
					if len(fhs) > 0 {
						c.FileField, c.FileName, c.FileBytes = field, fhs[0].Filename, int(fhs[0].Size)
					}
				}
			}
		} else {
			_ = r.ParseForm()
			c.ChatID = r.Form.Get("chat_id")
			c.ThreadID = r.Form.Get("message_thread_id")
			c.Text = r.Form.Get("text")
		}
		mu.Lock()
		sends = append(sends, c)
		mu.Unlock()

		status, body := 200, `{"ok":true,"result":{"message_id":1}}`
		if respond != nil {
			status, body = respond(c)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))

	cfg := telegramSinkConfig{
		BotToken: "test-token", ChatID: "-100999", AdminUserID: "42",
		TopicBugs: "1700", TopicKYC: "1701", TopicIdeas: "1702",
		TopicHelp: "1703", TopicOther: "1704",
	}
	for _, o := range overrides {
		o(&cfg)
	}
	sink := newTelegramSupportSink(cfg)
	sink.baseURL = srv.URL
	return sink, &sends, srv.Close
}

// withoutAdminUserID drops TELEGRAM_ADMIN_USER_ID. Worth being able to build:
// it is the configuration in which there is no private destination for an
// image, and "no destination" must never resolve to the public group.
func withoutAdminUserID(c *telegramSinkConfig) { c.AdminUserID = "" }

func sampleRequest(category string) SupportRequest {
	return SupportRequest{
		ID: uuid.New(), Category: category,
		Message: "my verification was refused and I do not know why",
		PageURL: "https://grainlify.com/settings", ReporterLogin: "octocat",
	}
}

func TestTelegramSink_KYCGoesToTheAdminDMOnlyAndNeverToTheGroup(t *testing.T) {
	sink, sends, closeSrv := fakeTelegram(t, nil)
	defer closeSrv()

	req := sampleRequest("kyc")
	if _, err := sink.Deliver(context.Background(), req); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if len(*sends) != 1 {
		t.Fatalf("expected exactly one send (the DM), got %d", len(*sends))
	}
	got := (*sends)[0]
	if got.ChatID != "42" {
		t.Errorf("chat_id = %q, want the admin user id - a KYC request must not reach the group", got.ChatID)
	}
	if got.ThreadID != "" {
		t.Errorf("message_thread_id = %q, want empty - a DM has no topic", got.ThreadID)
	}
	// The DM is private, so it carries the details.
	if !strings.Contains(got.Text, "octocat") {
		t.Error("the admin DM must name the reporter")
	}
	if !strings.Contains(got.Text, req.Message) {
		t.Error("the admin DM must carry the message")
	}
	// And nothing went to the group, at any thread.
	for _, s := range *sends {
		if s.ChatID == "-100999" {
			t.Errorf("a KYC request was posted to the group: %+v", s)
		}
	}
}

func TestTelegramSink_PublicTopicPostsCarryNoIdentity(t *testing.T) {
	sink, sends, closeSrv := fakeTelegram(t, nil)
	defer closeSrv()

	for _, cat := range []string{"bug", "idea", "help", "other"} {
		req := sampleRequest(cat)
		req.ReporterLogin = "octocat"
		if _, err := sink.Deliver(context.Background(), req); err != nil {
			t.Fatalf("%s: %v", cat, err)
		}
		got := (*sends)[len(*sends)-1]

		if got.ChatID != "-100999" {
			t.Errorf("%s: chat_id = %q, want the group", cat, got.ChatID)
		}
		if got.ThreadID == "" {
			t.Errorf("%s: no message_thread_id - the post would land in General", cat)
		}
		// The group is public: the support ID is the link back to identity,
		// and the identity itself stays in the database.
		if strings.Contains(got.Text, "octocat") {
			t.Errorf("%s: the public post leaked the reporter login", cat)
		}
		if !strings.Contains(got.Text, req.ID.String()) {
			t.Errorf("%s: the public post must carry the support ID", cat)
		}
	}
}

func TestTelegramSink_DeadTopicFallsBackToGeneralAndSaysSo(t *testing.T) {
	// First call (to the topic) fails the way a deleted topic does; the retry
	// without a thread id succeeds.
	sink, sends, closeSrv := fakeTelegram(t, func(c capturedSend) (int, string) {
		if c.ThreadID != "" {
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: message thread not found"}`
		}
		return 200, `{"ok":true,"result":{"message_id":2}}`
	})
	defer closeSrv()

	res, err := sink.Deliver(context.Background(), sampleRequest("bug"))
	if err != nil {
		t.Fatalf("a dead topic must not lose the report: %v", err)
	}
	if !res.RoutedToFallback {
		t.Error("RoutedToFallback = false; a report landing in General while its topic sits empty is invisible otherwise")
	}
	if len(*sends) != 2 {
		t.Fatalf("expected a topic attempt then a General retry, got %d sends", len(*sends))
	}
	if (*sends)[1].ThreadID != "" {
		t.Error("the retry must omit message_thread_id so it lands in General")
	}
}

func TestTelegramSink_FailedAdminDMIsAnError(t *testing.T) {
	// 403 is what Telegram returns when the admin has not started the bot.
	sink, sends, closeSrv := fakeTelegram(t, func(c capturedSend) (int, string) {
		return 403, `{"ok":false,"error_code":403,"description":"Forbidden: bot can't initiate conversation with a user"}`
	})
	defer closeSrv()

	res, err := sink.Deliver(context.Background(), sampleRequest("kyc"))
	if err == nil {
		t.Fatal("a failed admin DM must be an error - for KYC there is no public post, so the details reached nobody")
	}
	if res.RoutedToFallback {
		t.Error("a DM cannot route to a fallback")
	}
	// It must NOT quietly post to the group instead.
	for _, s := range *sends {
		if s.ChatID == "-100999" {
			t.Errorf("a failed DM fell back to the public group: %+v", s)
		}
	}
}

func TestTelegramSink_UnconfiguredIsSkippedNotFailed(t *testing.T) {
	for _, cfg := range []telegramSinkConfig{
		{},
		{BotToken: "t"},
		{ChatID: "-100999"},
	} {
		if newTelegramSupportSink(cfg).Configured() {
			t.Errorf("sink reported configured with %+v", cfg)
		}
	}
	if !newTelegramSupportSink(telegramSinkConfig{BotToken: "t", ChatID: "-100999"}).Configured() {
		t.Error("token + chat id should be enough; a missing topic degrades to General per-category")
	}
}

func TestTelegramSink_MigrateToChatIDIsNotFollowedAutomatically(t *testing.T) {
	sink, _, closeSrv := fakeTelegram(t, func(c capturedSend) (int, string) {
		return 400, `{"ok":false,"error_code":400,"description":"Bad Request: group chat was upgraded to a supergroup chat","parameters":{"migrate_to_chat_id":-1009999999999}}`
	})
	defer closeSrv()

	before := sink.chatID
	_, _ = sink.Deliver(context.Background(), sampleRequest("bug"))
	if sink.chatID != before {
		t.Errorf("chat id changed itself to %q; the destination must not move without a decision", sink.chatID)
	}
}
