package config

import (
	"strings"
	"testing"
)

// full returns a Config with every gated variable populated, so each test can
// remove exactly one thing and attribute the failure to it.
func full() Config {
	return Config{
		Env:                     "production",
		DBURL:                   "postgres://x",
		JWTSecret:               "s",
		TokenEncKeyB64:          "k",
		GitHubOAuthClientID:     "id",
		GitHubOAuthClientSecret: "secret",
		GitHubOAuthRedirectURL:  "https://api.example.com/cb",
		GitHubAppID:             "1",
		GitHubAppPrivateKey:     "pem",
		GitHubWebhookSecret:     "whs",
		DiditAPIKey:             "dk",
		DiditWorkflowID:         "wf",
		DiditWebhookSecret:      "dws",
		PublicBaseURL:           "https://api.example.com",
		FrontendBaseURL:         "https://example.com",
		TelegramAdminUserID:     "123",
		TelegramBotToken:        "bt",
		TelegramChatID:          "cid",
	}
}

// A fully-populated production config boots. Without this the rest of the file
// could pass while the gate refused every deploy.
func TestMissingRequired_FullConfigIsSatisfied(t *testing.T) {
	if m := full().MissingRequired(); len(m) != 0 {
		var names []string
		for _, r := range m {
			names = append(names, r.Name)
		}
		t.Fatalf("a complete config reported %d missing: %v", len(m), names)
	}
}

// The list is the agreed one. Pinned by name so a variable cannot be dropped
// from the gate silently - removing one is a decision, and this test makes it
// show up as a decision.
func TestRequiredForLiveFeatures_IsTheAgreedList(t *testing.T) {
	want := []string{
		"DB_URL", "JWT_SECRET", "TOKEN_ENC_KEY_B64",
		"GITHUB_OAUTH_CLIENT_ID", "GITHUB_OAUTH_CLIENT_SECRET", "GITHUB_OAUTH_REDIRECT_URL",
		"GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY", "GITHUB_WEBHOOK_SECRET",
		"DIDIT_API_KEY", "DIDIT_WORKFLOW_ID", "DIDIT_WEBHOOK_SECRET",
		"PUBLIC_BASE_URL", "FRONTEND_BASE_URL",
		"TELEGRAM_ADMIN_USER_ID", "TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID",
	}
	got := requiredForLiveFeatures()
	if len(got) != len(want) {
		t.Fatalf("gate has %d variables, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("position %d = %s, want %s", i, got[i].Name, w)
		}
	}
}

// Each variable is independently detected. A table where one accessor is
// wired to the wrong field would otherwise pass every other test here.
func TestMissingRequired_DetectsEachVariableIndividually(t *testing.T) {
	for _, r := range requiredForLiveFeatures() {
		t.Run(r.Name, func(t *testing.T) {
			c := full()
			blank(&c, r.Name)
			missing := c.MissingRequired()
			if len(missing) != 1 {
				t.Fatalf("blanking %s reported %d missing, want exactly 1", r.Name, len(missing))
			}
			if missing[0].Name != r.Name {
				t.Errorf("blanking %s reported %s - an accessor is wired to the wrong field", r.Name, missing[0].Name)
			}
		})
	}
}

// Whitespace is not a value. A dashboard entry of " " is a typo, and treating
// it as present is the exact failure the gate exists to prevent.
func TestMissingRequired_WhitespaceCountsAsEmpty(t *testing.T) {
	c := full()
	c.PublicBaseURL = "   "
	m := c.MissingRequired()
	if len(m) != 1 || m[0].Name != "PUBLIC_BASE_URL" {
		t.Fatalf("whitespace-only value was accepted as set: %v", m)
	}
}

// Every empty variable is reported, not just the first. One at a time turns a
// misconfigured deploy into a sequence of deploy-fail-fix cycles.
func TestMissingRequired_ReportsAllAtOnce(t *testing.T) {
	c := full()
	c.DiditAPIKey = ""
	c.TelegramChatID = ""
	c.PublicBaseURL = ""
	if m := c.MissingRequired(); len(m) != 3 {
		t.Fatalf("reported %d missing, want 3 - the gate is short-circuiting", len(m))
	}
}

// The message names the variable AND the feature. "config invalid" only
// relocates the mystery.
func TestMissingRequiredMessage_NamesVariableAndFeature(t *testing.T) {
	c := full()
	c.TelegramAdminUserID = ""
	msg := MissingRequiredMessage(c.MissingRequired())
	for _, want := range []string{"TELEGRAM_ADMIN_USER_ID", "KYC support delivery", "reach no human"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not contain %q\ngot:\n%s", want, msg)
		}
	}
}

// Dev does not gate. A developer has no Telegram bot token, and a gate that
// stops them running the API locally gets deleted within a week.
func TestGatesBoot_OffInDevOnly(t *testing.T) {
	for env, want := range map[string]bool{"dev": false, "production": true, "staging": true, "": true} {
		c := full()
		c.Env = env
		if got := c.GatesBoot(); got != want {
			t.Errorf("Env=%q GatesBoot()=%v, want %v", env, got, want)
		}
	}
}

// Every entry carries a feature and a consequence. An entry added later with
// an empty Feature would produce exactly the "config invalid" message this
// design rejects.
func TestRequiredForLiveFeatures_EveryEntryExplainsItself(t *testing.T) {
	for _, r := range requiredForLiveFeatures() {
		if strings.TrimSpace(r.Feature) == "" {
			t.Errorf("%s has no Feature", r.Name)
		}
		if strings.TrimSpace(r.Consequence) == "" {
			t.Errorf("%s has no Consequence", r.Name)
		}
	}
}

// blank clears the field behind a given variable name.
func blank(c *Config, name string) {
	switch name {
	case "DB_URL":
		c.DBURL = ""
	case "JWT_SECRET":
		c.JWTSecret = ""
	case "TOKEN_ENC_KEY_B64":
		c.TokenEncKeyB64 = ""
	case "GITHUB_OAUTH_CLIENT_ID":
		c.GitHubOAuthClientID = ""
	case "GITHUB_OAUTH_CLIENT_SECRET":
		c.GitHubOAuthClientSecret = ""
	case "GITHUB_OAUTH_REDIRECT_URL":
		c.GitHubOAuthRedirectURL = ""
	case "GITHUB_APP_ID":
		c.GitHubAppID = ""
	case "GITHUB_APP_PRIVATE_KEY":
		c.GitHubAppPrivateKey = ""
	case "GITHUB_WEBHOOK_SECRET":
		c.GitHubWebhookSecret = ""
	case "DIDIT_API_KEY":
		c.DiditAPIKey = ""
	case "DIDIT_WORKFLOW_ID":
		c.DiditWorkflowID = ""
	case "DIDIT_WEBHOOK_SECRET":
		c.DiditWebhookSecret = ""
	case "PUBLIC_BASE_URL":
		c.PublicBaseURL = ""
	case "FRONTEND_BASE_URL":
		c.FrontendBaseURL = ""
	case "TELEGRAM_ADMIN_USER_ID":
		c.TelegramAdminUserID = ""
	case "TELEGRAM_BOT_TOKEN":
		c.TelegramBotToken = ""
	case "TELEGRAM_CHAT_ID":
		c.TelegramChatID = ""
	default:
		panic("blank: unknown variable " + name + " - add it here when adding it to the gate")
	}
}
