package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env      string
	HTTPAddr string
	Log      string

	DBURL       string
	AutoMigrate bool

	// DBMaxConns is the maximum number of connections in the pool (DB_MAX_CONNS, default 10).
	DBMaxConns int32
	// DBMinConns is the minimum number of idle connections kept open (DB_MIN_CONNS, default 0).
	DBMinConns int32
	// DBMaxConnLifetime is the maximum time a connection may be reused (DB_MAX_CONN_LIFETIME, default 30m).
	DBMaxConnLifetime time.Duration
	// DBMaxConnIdleTime is the maximum idle time before a connection is closed (DB_MAX_CONN_IDLE_TIME, default 5m).
	DBMaxConnIdleTime time.Duration

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

	// Public base URL of this backend, used when registering GitHub webhooks.
	PublicBaseURL string

	// Frontend base URL (e.g., http://localhost:5173 or https://yourdomain.com)
	// Used for OAuth redirects and CORS configuration
	FrontendBaseURL string

	// Allowed CORS origins (comma-separated). If empty, uses FrontendBaseURL
	// Example: "http://localhost:5173,https://grainlify.figma.site"
	CORSOrigins string

	// CORSAllowPreview enables wildcard matching for *.vercel.app and *.0xo.in origins.
	// Off by default; only enable when preview deployments need credentialed CORS access.
	CORSAllowPreview bool

	// Used to encrypt stored OAuth access tokens at rest. Must be 32 bytes base64 (AES-256-GCM key).
	TokenEncKeyB64 string

	// Dev/admin convenience: allow promoting a logged-in user to admin via a shared token.
	AdminBootstrapToken string

	// Didit KYC verification
	DiditAPIKey        string
	DiditWorkflowID    string
	DiditWebhookSecret string

	// Soroban configuration
	SorobanRPCURL            string
	SorobanNetworkPassphrase string
	SorobanNetwork           string // "testnet" or "mainnet"
	SorobanSourceSecret      string
	EscrowContractID         string
	ProgramEscrowContractID  string
	TokenContractID          string

	// SyncJobsMaxAttempts is the maximum number of attempts before a sync job is dead-lettered.
	// Controlled by SYNC_JOBS_MAX_ATTEMPTS, default 5.
	SyncJobsMaxAttempts int
	// SyncJobsBackoffBase is the base duration for exponential backoff between retries.
	// Controlled by SYNC_JOBS_BACKOFF_BASE, default 30s.
	SyncJobsBackoffBase time.Duration

	// MaxBodyBytes is the maximum request body size in bytes (MAX_BODY_BYTES, default 1048576 / 1MB).
	MaxBodyBytes int

	// RateLimitAuthPerMin is the per-minute limit for auth and webhook endpoints.
	// Controlled by RATE_LIMIT_AUTH_PER_MIN, default 60 requests/minute.
	RateLimitAuthPerMin int
	// RateLimitPublicPerMin is the per-minute limit for public read endpoints.
	// Controlled by RATE_LIMIT_PUBLIC_PER_MIN, default 300 requests/minute.
	RateLimitPublicPerMin int
	// TrustedProxies contains the IPs or CIDRs that are allowed to supply
	// X-Forwarded-For values. Controlled by TRUSTED_PROXIES.
	TrustedProxies []string
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

		DBMaxConns:        getEnvInt32("DB_MAX_CONNS", 10),
		DBMinConns:        getEnvInt32("DB_MIN_CONNS", 0),
		DBMaxConnLifetime: getEnvDuration("DB_MAX_CONN_LIFETIME", 30*time.Minute),
		DBMaxConnIdleTime: getEnvDuration("DB_MAX_CONN_IDLE_TIME", 5*time.Minute),

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

		PublicBaseURL: getEnv("PUBLIC_BASE_URL", ""),

		FrontendBaseURL:  getEnv("FRONTEND_BASE_URL", ""),
		CORSOrigins:      getEnv("CORS_ORIGINS", ""),
		CORSAllowPreview: getEnvBool("CORS_ALLOW_PREVIEW", false),

		TokenEncKeyB64: getEnv("TOKEN_ENC_KEY_B64", ""),

		AdminBootstrapToken: strings.TrimSpace(getEnv("ADMIN_BOOTSTRAP_TOKEN", "")),

		DiditAPIKey:        getEnv("DIDIT_API_KEY", ""),
		DiditWorkflowID:    getEnv("DIDIT_WORKFLOW_ID", ""),
		DiditWebhookSecret: getEnv("DIDIT_WEBHOOK_SECRET", ""),

		// Soroban configuration
		SorobanRPCURL:            getEnv("SOROBAN_RPC_URL", ""),
		SorobanNetworkPassphrase: getEnv("SOROBAN_NETWORK_PASSPHRASE", ""),
		SorobanNetwork:           getEnv("SOROBAN_NETWORK", "testnet"),
		SorobanSourceSecret:      getEnv("SOROBAN_SOURCE_SECRET", ""),
		EscrowContractID:         getEnv("ESCROW_CONTRACT_ID", ""),
		ProgramEscrowContractID:  getEnv("PROGRAM_ESCROW_CONTRACT_ID", ""),
		TokenContractID:          getEnv("TOKEN_CONTRACT_ID", ""),

		SyncJobsMaxAttempts: getEnvInt("SYNC_JOBS_MAX_ATTEMPTS", 5),
		SyncJobsBackoffBase: getEnvDuration("SYNC_JOBS_BACKOFF_BASE", 30*time.Second),

		MaxBodyBytes:          getEnvInt("MAX_BODY_BYTES", 1048576),
		RateLimitAuthPerMin:   getEnvInt("RATE_LIMIT_AUTH_PER_MIN", 60),
		RateLimitPublicPerMin: getEnvInt("RATE_LIMIT_PUBLIC_PER_MIN", 300),
		TrustedProxies:        parseTrustedProxies(getEnv("TRUSTED_PROXIES", "127.0.0.1,::1")),
	}
}

// IsDev reports whether the app runs in local development mode.
func (c Config) IsDev() bool {
	return strings.EqualFold(strings.TrimSpace(c.Env), "dev")
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

func getEnvInt32(key string, fallback int32) int32 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n <= 0 {
		return fallback
	}
	return int32(n)
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
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

func getEnvInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func parseTrustedProxies(value string) []string {
	parts := strings.Split(value, ",")
	proxies := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			proxies = append(proxies, part)
		}
	}
	return proxies
}
