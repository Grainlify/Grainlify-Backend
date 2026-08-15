package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// A KYC request must never reach Discord.
//
// The Telegram sink already refuses to post one to a group that can be read
// without joining. The Discord embed carries the same message and page PLUS
// the reporter's GitHub login and any screenshot, so sending it there anyway
// would have made that refusal pointless.
//
// The alternative considered - and rejected - was keeping the Discord channel
// private and checking that it was. That makes the privacy of every
// verification request a property of a channel permission: correct today, and
// silently changeable by anyone with Manage Channels, with nothing in the
// system that would notice. Held here instead, where this test notices.

func TestDiscordSink_DoesNotHandleKYC(t *testing.T) {
	s := newDiscordSupportSink("https://discord.example/webhook")

	if s.Handles("kyc") {
		t.Error("the discord sink accepted kyc; verification details would reach the channel with the reporter's login attached")
	}
	for _, cat := range []string{"bug", "idea", "help", "other"} {
		if !s.Handles(cat) {
			t.Errorf("the discord sink refused %q, which it is supposed to take", cat)
		}
	}
}

func TestTelegramSink_HandlesEveryCategory(t *testing.T) {
	s := newTelegramSupportSink(telegramSinkConfig{BotToken: "t", ChatID: "-100999"})
	// Telegram answers the same question differently: it takes KYC and routes
	// it to a DM rather than declining it.
	for _, cat := range []string{"bug", "kyc", "idea", "help", "other"} {
		if !s.Handles(cat) {
			t.Errorf("the telegram sink refused %q; kyc in particular must be routed, not dropped", cat)
		}
	}
}

// Handles is what the handler consults, but a sink that would happily post a
// KYC request if called anyway is one refactor away from doing so. This proves
// the webhook is never hit even when Deliver is called directly.
func TestDiscordSink_NeverPostsKYCEvenIfCalledDirectly(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		posts++
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer srv.Close()

	s := newDiscordSupportSink(srv.URL)
	req := SupportRequest{
		ID: uuid.New(), Category: "kyc",
		Message: "my verification was refused", ReporterLogin: "octocat",
	}

	if _, err := s.Deliver(context.Background(), req); err == nil {
		t.Error("Deliver accepted a kyc request; it must refuse rather than post it")
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 0 {
		t.Errorf("%d requests hit the webhook for a kyc report; it must be zero", posts)
	}
}
