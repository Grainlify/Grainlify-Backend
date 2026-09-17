package config

import (
	"fmt"
	"strings"
)

// The boot gate: configuration without which a live feature is silently dead.
//
// # Why refuse to start
//
// Every variable below has a live feature behind it, and most fail *quietly*
// when empty. PUBLIC_BASE_URL is the clearest case: Didit is told where to send
// its verification result, so an empty value means verifications complete at the
// provider and never come back, and contributors sit in in_review forever with
// nothing logged as wrong. TELEGRAM_ADMIN_USER_ID is the same shape - KYC
// support delivery is DM-only, so an empty value means those requests reach no
// human at all and no second channel notices.
//
// A process that refuses to start is loud, immediate, and happens before any
// user meets the broken feature. That is only the better trade because Railway
// keeps the previous deployment serving when a new one fails its healthcheck -
// established by deliberately deploying a build that could not boot and watching
// production continue to serve the old one, rather than by reading the docs.
// If that ever stops being true, this gate becomes an outage generator and the
// list should be split.
//
// # Why the whole list at once
//
// MissingRequired returns every empty variable, not the first. Reporting one at
// a time turns a misconfigured deploy into a sequence of deploy-fail-fix cycles,
// each costing a build, and each one hiding the next.
//
// # What is deliberately not here
//
//   - MAILERCLOUD_API_KEY, EMAIL_FROM_* - email reaches nobody by decision, not
//     by accident. Gating would refuse to boot over a feature we chose not to
//     have.
//   - DISCORD_BUG_REPORT_WEBHOOK_URL - a second sink. Telegram is the system of
//     record; losing Discord degrades rather than breaks.
//   - TELEGRAM_TOPIC_* - absence falls back to General and is flagged per row.
//   - SOROBAN_* and the contract IDs - no payout path has ever run. Required in
//     principle, not required for a live feature.
//   - KEEPERHUB_PAYOUT_WALLET_ADDRESS - display-only. It fills in the admin
//     screen's reconciliation hint with the sender address to look for on
//     Basescan; nothing reads it to move money and nothing else depends on it.
//     An empty value degrades LOUDLY and per-request - the screen says the
//     address is not configured, right where an admin would look for it - which
//     is exactly the failure mode (TELEGRAM_TOPIC_*, CORS_ORIGINS) this gate
//     does not exist for. The gate is for a live feature dying with nothing
//     logged as wrong; this is not that.
//   - CORS_ORIGINS - empty is correct. The allowlist is an explicit function in
//     internal/api, and this is the one piece of security-relevant configuration
//     that is not environment-only.

// RequiredVar is one gated variable and the feature that dies without it.
type RequiredVar struct {
	// Name is the environment variable, named exactly as it must be set.
	Name string
	// Feature is what stops working. A boot failure reading "config invalid"
	// only relocates the mystery, so the message has to say what was lost.
	Feature string
	// Consequence is what an empty value does in production - the reason this
	// entry is worth refusing to boot over, stated so the person reading the
	// log at 3am does not have to reconstruct it.
	Consequence string

	value func(Config) string
}

// requiredForLiveFeatures is the gate list: 15 features, 20 variables.
func requiredForLiveFeatures() []RequiredVar {
	return []RequiredVar{
		{"DB_URL", "everything", "no data at all",
			func(c Config) string { return c.DBURL }},
		{"JWT_SECRET", "all authentication", "every session token is unverifiable",
			func(c Config) string { return c.JWTSecret }},
		{"TOKEN_ENC_KEY_B64", "GitHub token storage", "503 on OAuth and on every issue application",
			func(c Config) string { return c.TokenEncKeyB64 }},

		{"GITHUB_OAUTH_CLIENT_ID", "sign-in", "nobody can sign in; it is the only way in",
			func(c Config) string { return c.GitHubOAuthClientID }},
		{"GITHUB_OAUTH_CLIENT_SECRET", "sign-in", "the code exchange fails for every attempt",
			func(c Config) string { return c.GitHubOAuthClientSecret }},
		{"GITHUB_OAUTH_REDIRECT_URL", "sign-in", "GitHub rejects the redirect_uri and refuses the exchange",
			func(c Config) string { return c.GitHubOAuthRedirectURL }},

		{"GITHUB_APP_ID", "issue sync, bot comments, assignment", "installation tokens cannot be minted",
			func(c Config) string { return c.GitHubAppID }},
		{"GITHUB_APP_PRIVATE_KEY", "issue sync, bot comments, assignment", "installation tokens cannot be signed",
			func(c Config) string { return c.GitHubAppPrivateKey }},
		{"GITHUB_WEBHOOK_SECRET", "GitHub webhook authentication", "unsigned webhooks would be accepted",
			func(c Config) string { return c.GitHubWebhookSecret }},

		{"DIDIT_API_KEY", "KYC verification", "no verification can be started",
			func(c Config) string { return c.DiditAPIKey }},
		{"DIDIT_WORKFLOW_ID", "KYC verification", "sessions cannot be created",
			func(c Config) string { return c.DiditWorkflowID }},
		{"DIDIT_WEBHOOK_SECRET", "KYC webhook authentication", "forged verification decisions would be accepted",
			func(c Config) string { return c.DiditWebhookSecret }},

		{"PUBLIC_BASE_URL", "Didit callback", "verifications complete at the provider and never come back; contributors sit in in_review forever",
			func(c Config) string { return c.PublicBaseURL }},
		{"FRONTEND_BASE_URL", "every link the system emits", "bot comments, emails and redirects point nowhere",
			func(c Config) string { return c.FrontendBaseURL }},

		{"TELEGRAM_ADMIN_USER_ID", "KYC support delivery", "KYC requests reach no human, and KYC is DM-only so nothing else notices",
			func(c Config) string { return c.TelegramAdminUserID }},
		{"TELEGRAM_BOT_TOKEN", "all support delivery", "no support report reaches anyone",
			func(c Config) string { return c.TelegramBotToken }},
		{"TELEGRAM_CHAT_ID", "all support delivery", "reports have nowhere to be posted",
			func(c Config) string { return c.TelegramChatID }},

		// KeeperHub payout dispatch. Excluded until #555's release endpoint
		// landed on main, on the grounds that no payout path had ever run - see
		// the "deliberately not here" note above for the history. That premise
		// is gone: the release endpoint is live, so a deployment intending to
		// run payouts must find out at boot that the rail is unusable, not
		// discover it the first time a run tries to fire and silently does
		// nothing. KEEPERHUB_PAYOUT_WALLET_ADDRESS is not here - see the
		// "deliberately not here" note.
		{"KEEPERHUB_API_KEY", "KeeperHub payout simulate and status polling", "a run's simulate step and execution-status polling both fail silently; keeperhub.New refuses to build a client",
			func(c Config) string { return c.KeeperHubAPIKey }},
		{"KEEPERHUB_WEBHOOK_KEY", "KeeperHub payout dispatch", "the webhook call that fires a payout run has no key to authenticate with; the rail is disabled with nothing logged at the call site",
			func(c Config) string { return c.KeeperHubWebhookKey }},
		{"KEEPERHUB_WORKFLOW_ID", "KeeperHub payout dispatch", "there is no workflow to fire; keeperhub.New refuses to build a client, exactly as an empty key does",
			func(c Config) string { return c.KeeperHubWorkflowID }},
	}
}

// MissingRequired returns every gated variable that is empty, in list order.
//
// Whitespace counts as empty: a variable set to " " in a dashboard is a typo,
// not a value, and treating it as present is the failure this gate exists to
// prevent.
func (c Config) MissingRequired() []RequiredVar {
	var missing []RequiredVar
	for _, r := range requiredForLiveFeatures() {
		if strings.TrimSpace(r.value(c)) == "" {
			missing = append(missing, r)
		}
	}
	return missing
}

// GatesBoot reports whether this environment refuses to start on missing
// configuration.
//
// Off in dev, and deliberately: a developer has no Telegram bot token and no
// Didit workflow, and a gate that stops them running the API locally would be
// removed within a week. It follows the same "dev is different" rule already
// used for DB_URL in cmd/api.
func (c Config) GatesBoot() bool { return c.Env != "dev" }

// MissingRequiredMessage renders the failure a human reads in the deploy log.
//
// One line per variable, naming the variable, the feature, and what an empty
// value costs.
func MissingRequiredMessage(missing []RequiredVar) string {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to start: %d required configuration value(s) are empty\n", len(missing))
	for _, r := range missing {
		fmt.Fprintf(&b, "  %s - %s: %s\n", r.Name, r.Feature, r.Consequence)
	}
	b.WriteString("set them and redeploy; the previous deployment keeps serving until this one boots")
	return b.String()
}
