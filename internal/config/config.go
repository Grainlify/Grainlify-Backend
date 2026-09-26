package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Env      string
	HTTPAddr string
	Log      string

	DBURL       string
	AutoMigrate bool

	JWTSecret string

	NATSURL string

	GitHubOAuthClientID           string
	GitHubOAuthClientSecret       string
	GitHubOAuthRedirectURL        string // Full callback URL (e.g., http://localhost:8080/auth/github/login/callback)
	GitHubOAuthSuccessRedirectURL string
	GitHubLoginRedirectURL        string // Alternative callback URL (deprecated, use GitHubOAuthRedirectURL)
	GitHubLoginSuccessRedirectURL string

	// GitHub App configuration (for organization installations)
	GitHubAppID         string // GitHub App ID (numeric)
	GitHubAppSlug       string // GitHub App slug (e.g., "grainlify")
	GitHubAppPrivateKey string // GitHub App private key (PEM format, base64 encoded)

	// Used to validate GitHub webhook signatures (X-Hub-Signature-256).
	GitHubWebhookSecret string

	// Discord webhook URL that bug reports (POST /bug-reports) are relayed
	// to. If empty, the endpoint responds 503 rather than silently dropping
	// reports.
	DiscordBugReportWebhookURL string

	// Telegram support sink. Every value is a plain identifier except the
	// token. If any required one is empty the sink reports itself
	// unconfigured and is skipped - it must never crash the endpoint or
	// block Discord.
	TelegramBotToken    string
	TelegramChatID      string
	TelegramAdminUserID string
	TelegramTopicBugs   string
	TelegramTopicKYC    string
	TelegramTopicIdeas  string
	TelegramTopicHelp   string
	TelegramTopicOther  string

	// Public base URL of this backend, used when registering GitHub webhooks.
	PublicBaseURL string

	// Frontend base URL (e.g., http://localhost:5173 or https://yourdomain.com)
	// Used for OAuth redirects and CORS configuration
	FrontendBaseURL string

	// Allowed CORS origins (comma-separated). If empty, uses FrontendBaseURL
	// Example: "http://localhost:5173,https://grainlify.figma.site"
	CORSOrigins string

	// Used to encrypt stored OAuth access tokens at rest. Must be 32 bytes base64 (AES-256-GCM key).
	TokenEncKeyB64 string

	// SaltEncKeyB64 encrypts per-event Merkle identity salts at rest.
	//
	// **Deliberately separate from TokenEncKeyB64**, which protects GitHub
	// access tokens, because the two secrets have opposite recoverability. A
	// leaked token key is survivable: rotate it, re-encrypt, and the exposure
	// ends. A leaked salt is not recoverable by any action - the identity
	// hashes it protects are on a permanent chain, so anyone holding the salt
	// and the public list of GitHub logins can correlate the leaf set in bulk,
	// forever, and no rotation can undo it because a published root cannot be
	// republished.
	//
	// One key protecting both would mean one compromise costs both, and only
	// one of them can be recovered from.
	//
	// 32 bytes, base64-standard-encoded, same shape as TokenEncKeyB64 and
	// validated by cryptox.KeyFromB64. Generated and held outside this
	// application; nothing in this repository should ever produce it.
	SaltEncKeyB64 string

	// Dev/admin convenience: allow promoting a logged-in user to admin via a shared token.
	AdminBootstrapToken string

	// Didit KYC verification
	DiditAPIKey string

	// Anthropic API key for GrainHack's AI fit assessment (AI-specs.md
	// §4.3) and, later, judging. Empty disables the model call: the
	// assignment pipeline then scores every applicant "plausible" and runs
	// entirely deterministically.
	AnthropicAPIKey    string
	DiditWorkflowID    string
	DiditWebhookSecret string

	// KeeperHub, the Base USDC payout rail.
	//
	// Two credentials, and they are kept apart as CODE DISCIPLINE, not as a
	// security boundary. Be precise about this, because the shape invites the
	// wrong reading: both values are org-scoped secrets living in the same
	// environment, read by the same process, and either one leaking is the
	// same incident. Splitting them protects NOTHING and must not be described
	// as if it does.
	//
	// What it does buy is that "fire a payout" and "look at a payout" are
	// different values in the code, so a read path cannot accidentally acquire
	// the ability to move money by reaching for the nearest credential in
	// scope. The compiler enforces the separation the reviewer wants; it is a
	// legibility property, not a containment one.
	//
	// KeeperHubWebhookKey authenticates the webhook call that FIRES a payout
	// run. KeeperHubAPIKey is the org API key and is used ONLY for simulate and
	// for polling execution status - it must never be what dispatches a run.
	//
	// Both empty by default. The client is constructed only when its key is
	// non-empty, mirroring the Didit constructor, so an unset value disables
	// the rail rather than half-enabling it.
	KeeperHubAPIKey     string
	KeeperHubWebhookKey string

	// KeeperHubWorkflowID is the workflow the webhook call fires.
	//
	// Not a credential, and it arrives here now rather than with the keys
	// because this repository's rule is that configuration lands with the code
	// that reads it - internal/keeperhub is that code. It was deliberately left
	// out of the commit that bound the two keys, which had no consumer.
	//
	// Empty disables the rail exactly as an empty key does: keeperhub.New
	// refuses to build a client without it, because a dispatcher that does not
	// know which workflow to fire has nothing to fall back on. There is no
	// default - guessing a workflow id would mean firing somebody else's
	// workflow with our recipient list.
	KeeperHubWorkflowID string

	// KeeperHubPayoutWallet is the address the payout workflow sends from - the
	// KeeperHub org wallet. Display only: the admin screen puts it in the hint
	// for reconciling a leg that may have paid, so the admin knows which
	// sender to look for on the explorer.
	//
	// Nothing reads it to move money, and nothing can derive it: under gas
	// sponsorship the transaction's sender is a relayer, and this backend never
	// asks KeeperHub for its wallet. Empty means the screen says the address
	// is not configured rather than showing a guess.
	KeeperHubPayoutWallet string

	// No chain configuration is read here.
	//
	// Seven keys used to be: four SOROBAN_* including a signing secret, plus
	// three contract ids. Every one had zero consumers - they were read into
	// this struct and used by nothing - and the two contract ids named the
	// escrow and program-escrow contracts whose clients have been deleted for
	// calling functions that exist nowhere.
	//
	// Config that reads a secret and feeds nothing is a liability with no
	// upside: it makes an unused signing key look provisioned, and it invites
	// the next person to wire something to it. When a real adapter needs chain
	// configuration it comes back with the code that consumes it, and the key
	// lives in a signer service rather than in this process.

	// Notification emails, sent via Mailercloud
	// (https://apidoc.mailercloud.com). If MailerCloudAPIKey is empty,
	// email sending is disabled - in-app notifications still work either way.
	MailerCloudAPIKey string
	EmailFromAddress  string
	EmailFromName     string

	// Grainlify Bounties: base64 of a 32-byte ed25519 seed used ONLY to
	// countersign Solana wallet links (POST /me/bounty-wallet/challenge). The
	// bounty agent holds the public half. Empty turns that endpoint off (503);
	// nothing else reads it.
	BountyLinkSigningKey string
	// Where the bounty agent lives, for server-to-server reads.
	BountyAgentURL string
}

func Load() Config {
	env := getEnv("APP_ENV", "dev")
	logLevel := getEnv("LOG_LEVEL", "info")

	// Prefer HTTP_ADDR if provided, otherwise build it from PORT.
	httpAddr := os.Getenv("HTTP_ADDR")
	if strings.TrimSpace(httpAddr) == "" {
		port := getEnv("PORT", "8080")
		httpAddr = ":" + port
	}

	return Config{
		Env:      env,
		HTTPAddr: httpAddr,
		Log:      logLevel,

		DBURL:       getEnv("DB_URL", ""),
		AutoMigrate: getEnvBool("AUTO_MIGRATE", false),

		JWTSecret: getEnv("JWT_SECRET", ""),

		NATSURL: getEnv("NATS_URL", ""),

		GitHubOAuthClientID:           getEnv("GITHUB_OAUTH_CLIENT_ID", ""),
		GitHubOAuthClientSecret:       getEnv("GITHUB_OAUTH_CLIENT_SECRET", ""),
		GitHubOAuthRedirectURL:        getEnv("GITHUB_OAUTH_REDIRECT_URL", ""),
		GitHubOAuthSuccessRedirectURL: getEnv("GITHUB_OAUTH_SUCCESS_REDIRECT_URL", ""),
		GitHubLoginRedirectURL:        getEnv("GITHUB_LOGIN_REDIRECT_URL", ""),
		GitHubLoginSuccessRedirectURL: getEnv("GITHUB_LOGIN_SUCCESS_REDIRECT_URL", ""),

		GitHubAppID:         getEnv("GITHUB_APP_ID", ""),
		GitHubAppSlug:       getEnv("GITHUB_APP_SLUG", ""),
		GitHubAppPrivateKey: getEnv("GITHUB_APP_PRIVATE_KEY", ""),

		GitHubWebhookSecret: getEnv("GITHUB_WEBHOOK_SECRET", ""),

		DiscordBugReportWebhookURL: getEnv("DISCORD_BUG_REPORT_WEBHOOK_URL", ""),

		TelegramBotToken:    getEnv("TELEGRAM_BOT_TOKEN", ""),
		TelegramChatID:      getEnv("TELEGRAM_CHAT_ID", ""),
		TelegramAdminUserID: getEnv("TELEGRAM_ADMIN_USER_ID", ""),
		TelegramTopicBugs:   getEnv("TELEGRAM_TOPIC_BUGS", ""),
		TelegramTopicKYC:    getEnv("TELEGRAM_TOPIC_KYC", ""),
		TelegramTopicIdeas:  getEnv("TELEGRAM_TOPIC_IDEAS", ""),
		TelegramTopicHelp:   getEnv("TELEGRAM_TOPIC_HELP", ""),
		TelegramTopicOther:  getEnv("TELEGRAM_TOPIC_OTHER", ""),

		PublicBaseURL: getEnv("PUBLIC_BASE_URL", ""),

		FrontendBaseURL: getEnv("FRONTEND_BASE_URL", ""),
		CORSOrigins:     getEnv("CORS_ORIGINS", ""),

		TokenEncKeyB64: getEnv("TOKEN_ENC_KEY_B64", ""),
		SaltEncKeyB64:  getEnv("SALT_ENC_KEY_B64", ""),

		AdminBootstrapToken: strings.TrimSpace(getEnv("ADMIN_BOOTSTRAP_TOKEN", "")),

		DiditAPIKey: getEnv("DIDIT_API_KEY", ""),

		AnthropicAPIKey:    getEnv("ANTHROPIC_API_KEY", ""),
		DiditWorkflowID:    getEnv("DIDIT_WORKFLOW_ID", ""),
		DiditWebhookSecret: getEnv("DIDIT_WEBHOOK_SECRET", ""),

		KeeperHubAPIKey:     getEnv("KEEPERHUB_API_KEY", ""),
		KeeperHubWebhookKey: getEnv("KEEPERHUB_WEBHOOK_KEY", ""),
		KeeperHubWorkflowID: getEnv("KEEPERHUB_WORKFLOW_ID", ""),

		KeeperHubPayoutWallet: getEnv("KEEPERHUB_PAYOUT_WALLET_ADDRESS", ""),

		MailerCloudAPIKey: getEnv("MAILERCLOUD_API_KEY", ""),
		EmailFromAddress:  getEnv("EMAIL_FROM_ADDRESS", ""),
		EmailFromName:     getEnv("EMAIL_FROM_NAME", "Grainlify"),

		BountyLinkSigningKey: getEnv("BOUNTY_LINK_SIGNING_KEY", ""),
		BountyAgentURL:       getEnv("BOUNTY_AGENT_URL", "https://agent.grainlify.com"),
	}
}

func (c Config) LogLevel() slog.Leveler {
	switch strings.ToLower(strings.TrimSpace(c.Log)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "info", "":
		return slog.LevelInfo
	default:
		// Allow numeric levels for easy tweaking (-4 debug, 0 info, 4 warn, 8 error).
		if n, err := strconv.Atoi(c.Log); err == nil {
			return slog.Level(n)
		}
		return slog.LevelInfo
	}
}

func getEnv(key, fallback string) string {
	v := os.Getenv(key)
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func getEnvBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "t", "yes", "y", "on":
		return true
	case "0", "false", "f", "no", "n", "off":
		return false
	default:
		return fallback
	}
}
