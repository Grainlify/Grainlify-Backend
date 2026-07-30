package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/bus/natsbus"
)

// valid32ByteKey returns a base64-encoded 32-byte key for test use.
func valid32ByteKey() string {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func prodBase() Config {
	return Config{
		Env:            "production",
		HTTPAddr:       ":8080",
		DBURL:          "postgres://user:pass@localhost/db",
		JWTSecret:      strings.Repeat("x", 32),
		TokenEncKeyB64: valid32ByteKey(),
		SorobanNetwork: "testnet",
	}
}

func TestValidate_DevEnvSkipsSecretChecks(t *testing.T) {
	// In dev, missing JWT_SECRET and TOKEN_ENC_KEY_B64 are allowed.
	cfg := Config{
		Env:      "dev",
		HTTPAddr: ":8080",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error in dev env, got: %v", err)
	}
}

func TestValidate_ProdHappyPath(t *testing.T) {
	cfg := prodBase()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error for valid prod config, got: %v", err)
	}
}

func TestValidate_MissingDBURL(t *testing.T) {
	cfg := prodBase()
	cfg.DBURL = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for missing DB_URL")
	} else if !strings.Contains(err.Error(), "DB_URL") {
		t.Fatalf("error should mention DB_URL, got: %v", err)
	}
}

func TestValidate_JWTSecretTooShort(t *testing.T) {
	cfg := prodBase()
	cfg.JWTSecret = "tooshort"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for short JWT_SECRET")
	}
	if !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Fatalf("error should mention JWT_SECRET, got: %v", err)
	}
	// Must not contain the actual secret value.
	if strings.Contains(err.Error(), "tooshort") {
		t.Fatal("error message must not contain the secret value")
	}
}

func TestValidate_MissingTokenEncKey(t *testing.T) {
	cfg := prodBase()
	cfg.TokenEncKeyB64 = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for missing TOKEN_ENC_KEY_B64")
	}
}

func TestValidate_TokenEncKeyInvalidBase64(t *testing.T) {
	cfg := prodBase()
	cfg.TokenEncKeyB64 = "not!!valid%%base64"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for invalid base64 TOKEN_ENC_KEY_B64")
	}
	if !strings.Contains(err.Error(), "TOKEN_ENC_KEY_B64") {
		t.Fatalf("error should mention TOKEN_ENC_KEY_B64, got: %v", err)
	}
}

func TestValidate_TokenEncKeyWrongLength(t *testing.T) {
	// 16-byte key — valid base64 but wrong size for AES-256-GCM.
	key := make([]byte, 16)
	cfg := prodBase()
	cfg.TokenEncKeyB64 = base64.StdEncoding.EncodeToString(key)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for 16-byte TOKEN_ENC_KEY_B64")
	}
	if !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("error should mention 32 bytes, got: %v", err)
	}
}

func TestValidate_InvalidSorobanNetwork(t *testing.T) {
	cfg := prodBase()
	cfg.SorobanNetwork = "devnet"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid SOROBAN_NETWORK")
	}
}

func TestValidate_ValidSorobanNetworks(t *testing.T) {
	for _, net := range []string{"testnet", "mainnet"} {
		cfg := prodBase()
		cfg.SorobanNetwork = net
		if err := cfg.Validate(); err != nil {
			t.Fatalf("network %q should be valid, got: %v", net, err)
		}
	}
}

func TestValidate_PartialSorobanConfigFails(t *testing.T) {
	cfg := prodBase()
	// Set only some Soroban fields — should fail in prod.
	cfg.SorobanRPCURL = "https://soroban-testnet.stellar.org"
	cfg.SorobanSourceSecret = "SABCDEF"
	// EscrowContractID, ProgramEscrowContractID, TokenContractID left empty.
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for incomplete Soroban config")
	}
}

func TestValidate_PartialSorobanConfigErrorOrderIsDeterministic(t *testing.T) {
	// Regression test for #347: the "missing" list must always be in the
	// same order, not the random order Go map iteration would produce.
	const want = "missing: SOROBAN_SOURCE_SECRET, ESCROW_CONTRACT_ID, " +
		"PROGRAM_ESCROW_CONTRACT_ID, TOKEN_CONTRACT_ID"

	for i := 0; i < 10; i++ {
		cfg := prodBase()
		cfg.SorobanRPCURL = "https://soroban-testnet.stellar.org"
		// SorobanSourceSecret, EscrowContractID, ProgramEscrowContractID,
		// TokenContractID left empty so they land in "missing".
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error for incomplete Soroban config")
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("run %d: error message did not contain expected ordered missing list.\ngot:  %s\nwant substring: %s", i, err.Error(), want)
		}
	}
}

func TestValidate_FullSorobanConfigPasses(t *testing.T) {
	cfg := prodBase()
	cfg.SorobanRPCURL = "https://soroban-testnet.stellar.org"
	cfg.SorobanSourceSecret = "SABCDEF"
	cfg.EscrowContractID = "CABC123"
	cfg.ProgramEscrowContractID = "CDEF456"
	cfg.TokenContractID = "CGHI789"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error for complete Soroban config, got: %v", err)
	}
}

func TestValidate_InvalidHTTPAddr(t *testing.T) {
	cfg := prodBase()
	cfg.HTTPAddr = ":::notvalid:::"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid HTTP_ADDR")
	}
}

func TestValidate_MultipleErrors(t *testing.T) {
	cfg := Config{Env: "production", HTTPAddr: ":8080"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected multiple validation errors")
	}
	// Should report DB_URL, JWT_SECRET, TOKEN_ENC_KEY_B64 at minimum.
	msg := err.Error()
	for _, want := range []string{"DB_URL", "JWT_SECRET", "TOKEN_ENC_KEY_B64"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got: %v", want, msg)
		}
	}
}

// TestValidate_AllRequiredFieldsMissing verifies that when all required fields are missing,
// the validation error lists every missing field in a deterministic order.
func TestValidate_AllRequiredFieldsMissing(t *testing.T) {
	cfg := Config{Env: "production", HTTPAddr: ":8080"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for all missing required fields")
	}
	msg := err.Error()
	// Verify all required fields are mentioned.
	for _, field := range []string{"DB_URL", "JWT_SECRET", "TOKEN_ENC_KEY_B64"} {
		if !strings.Contains(msg, field) {
			t.Errorf("error should mention %q, got: %v", field, msg)
		}
	}
	// Verify the error is a single aggregated message (not multiple separate errors).
	if !strings.Contains(msg, "invalid configuration") {
		t.Errorf("error should have aggregated message prefix, got: %v", msg)
	}
}

// TestValidate_OptionalFieldsAtZeroValue verifies that optional fields at their zero value
// do not trigger validation errors.
func TestValidate_OptionalFieldsAtZeroValue(t *testing.T) {
	cfg := prodBase()
	// Set optional fields to zero values.
	cfg.NATSURL = ""
	cfg.GitHubOAuthClientID = ""
	cfg.GitHubOAuthClientSecret = ""
	cfg.GitHubAppID = ""
	cfg.GitHubAppPrivateKey = ""
	cfg.GitHubWebhookSecret = ""
	cfg.PublicBaseURL = ""
	cfg.FrontendBaseURL = ""
	cfg.CORSOrigins = ""
	cfg.AdminBootstrapToken = ""
	cfg.DiditAPIKey = ""
	cfg.DiditWorkflowID = ""
	cfg.DiditWebhookSecret = ""
	cfg.SorobanRPCURL = ""
	cfg.SorobanSourceSecret = ""
	cfg.EscrowContractID = ""
	cfg.ProgramEscrowContractID = ""
	cfg.TokenContractID = ""
	cfg.MetricsToken = ""

	if err := cfg.Validate(); err != nil {
		t.Fatalf("optional fields at zero value should not trigger errors, got: %v", err)
	}
}

// TestValidate_ErrorMessageDeterministicOrder verifies that when multiple required fields
// are missing, they are listed in a deterministic order (struct order in this case).
func TestValidate_ErrorMessageDeterministicOrder(t *testing.T) {
	cfg := Config{Env: "production", HTTPAddr: ":8080"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	// DB_URL should appear before JWT_SECRET, which should appear before TOKEN_ENC_KEY_B64,
	// following the order they are validated in the Validate() function.
	dbIdx := strings.Index(msg, "DB_URL")
	jwtIdx := strings.Index(msg, "JWT_SECRET")
	tokenIdx := strings.Index(msg, "TOKEN_ENC_KEY_B64")
	if dbIdx == -1 || jwtIdx == -1 || tokenIdx == -1 {
		t.Fatalf("all required fields should be present in error, got: %v", msg)
	}
	if !(dbIdx < jwtIdx && jwtIdx < tokenIdx) {
		t.Errorf("expected order DB_URL < JWT_SECRET < TOKEN_ENC_KEY_B64, got indices: %d, %d, %d", dbIdx, jwtIdx, tokenIdx)
	}
}

// TestValidate_ErrorDoesNotExposeSecretValues verifies that the error message lists only
// the environment variable names, not their values (even when the values are invalid).
func TestValidate_ErrorDoesNotExposeSecretValues(t *testing.T) {
	cfg := prodBase()
	secretValue := "my-secret-jwt-key"
	cfg.JWTSecret = secretValue
	err := cfg.Validate()
	// JWTSecret is too short, so validation should fail.
	if err == nil {
		t.Fatal("expected validation error for short JWT_SECRET")
	}
	msg := err.Error()
	if strings.Contains(msg, secretValue) {
		t.Fatalf("error message must not contain secret value %q, got: %v", secretValue, msg)
	}
	// Should mention the variable name instead.
	if !strings.Contains(msg, "JWT_SECRET") {
		t.Fatalf("error should mention JWT_SECRET variable name, got: %v", msg)
	}
}

func TestLoad_DefaultShutdownTimeout(t *testing.T) {
	t.Setenv("SHUTDOWN_TIMEOUT", "")

	cfg := Load()
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Fatalf("expected default shutdown timeout 10s, got %s", cfg.ShutdownTimeout)
	}
}

func TestLoad_ShutdownTimeoutFromEnv(t *testing.T) {
	t.Setenv("SHUTDOWN_TIMEOUT", "45s")

	cfg := Load()
	if cfg.ShutdownTimeout != 45*time.Second {
		t.Fatalf("expected shutdown timeout 45s, got %s", cfg.ShutdownTimeout)
	}
}

func TestLoad_RepoMetadataCacheTTLZeroDisablesCaching(t *testing.T) {
	t.Setenv("GITHUB_REPO_CACHE_TTL", "0")

	cfg := Load()
	if cfg.GitHubRepoMetadataCacheTTL != 0 {
		t.Fatalf("expected explicit zero TTL to be honored, got %s", cfg.GitHubRepoMetadataCacheTTL)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit zero TTL should be valid, got: %v", err)
	}
}

func TestLoad_RepoMetadataCacheTTLDefaultsAndRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		want       time.Duration
		wantErrKey bool
	}{
		{name: "unset", raw: "", want: 60 * time.Second},
		{name: "valid", raw: "15s", want: 15 * time.Second},
		{name: "malformed", raw: "not-a-duration", want: 60 * time.Second, wantErrKey: true},
		{name: "negative", raw: "-1s", want: 60 * time.Second, wantErrKey: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITHUB_REPO_CACHE_TTL", tt.raw)
			cfg := Load()
			if cfg.GitHubRepoMetadataCacheTTL != tt.want {
				t.Fatalf("expected TTL %s, got %s", tt.want, cfg.GitHubRepoMetadataCacheTTL)
			}
			err := cfg.Validate()
			if tt.wantErrKey && (err == nil || !strings.Contains(err.Error(), "GITHUB_REPO_CACHE_TTL")) {
				t.Fatalf("expected validation error mentioning GITHUB_REPO_CACHE_TTL, got: %v", err)
			}
			if !tt.wantErrKey && err != nil {
				t.Fatalf("expected valid configuration, got: %v", err)
			}
		})
	}
}

// TestLoad_JetStreamAckWaitDefault verifies that leaving JS_ACK_WAIT unset is a
// no-op: it falls back to natsbus.DefaultAckWait and never fails Validate().
func TestLoad_JetStreamAckWaitDefault(t *testing.T) {
	t.Setenv("JS_ACK_WAIT", "")

	cfg := Load()
	if cfg.JetStreamAckWait != natsbus.DefaultAckWait {
		t.Fatalf("expected default ack-wait %s, got %s", natsbus.DefaultAckWait, cfg.JetStreamAckWait)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unset JS_ACK_WAIT should not fail validation, got: %v", err)
	}
}

// TestLoad_JetStreamAckWaitFromEnv verifies a valid JS_ACK_WAIT overrides the default.
func TestLoad_JetStreamAckWaitFromEnv(t *testing.T) {
	t.Setenv("JS_ACK_WAIT", "90s")

	cfg := Load()
	if cfg.JetStreamAckWait != 90*time.Second {
		t.Fatalf("expected ack-wait 90s, got %s", cfg.JetStreamAckWait)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid JS_ACK_WAIT should not fail validation, got: %v", err)
	}
}

// TestValidate_JetStreamAckWaitMalformedFailsFast covers an unparseable duration string.
func TestValidate_JetStreamAckWaitMalformedFailsFast(t *testing.T) {
	t.Setenv("JS_ACK_WAIT", "not-a-duration")

	cfg := Load()
	// Behavior is unchanged even with bad input: the default is still used.
	if cfg.JetStreamAckWait != natsbus.DefaultAckWait {
		t.Fatalf("expected fallback to default ack-wait, got %s", cfg.JetStreamAckWait)
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for malformed JS_ACK_WAIT")
	}
	if !strings.Contains(err.Error(), "JS_ACK_WAIT") {
		t.Fatalf("error should mention JS_ACK_WAIT, got: %v", err)
	}
}

// TestValidate_JetStreamAckWaitZeroFailsFast covers an explicit zero duration,
// which must be rejected rather than silently treated as "unset".
func TestValidate_JetStreamAckWaitZeroFailsFast(t *testing.T) {
	t.Setenv("JS_ACK_WAIT", "0s")

	cfg := Load()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for zero JS_ACK_WAIT")
	}
	if !strings.Contains(err.Error(), "JS_ACK_WAIT") {
		t.Fatalf("error should mention JS_ACK_WAIT, got: %v", err)
	}
}

// TestValidate_JetStreamAckWaitNegativeFailsFast covers a negative duration.
func TestValidate_JetStreamAckWaitNegativeFailsFast(t *testing.T) {
	t.Setenv("JS_ACK_WAIT", "-5s")

	cfg := Load()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative JS_ACK_WAIT")
	}
	if !strings.Contains(err.Error(), "JS_ACK_WAIT") {
		t.Fatalf("error should mention JS_ACK_WAIT, got: %v", err)
	}
}

// TestValidate_JetStreamAckWaitCheckedInDev ensures the ack-wait bound is
// enforced in every environment, not just non-dev (unlike JWT/DB checks).
func TestValidate_JetStreamAckWaitCheckedInDev(t *testing.T) {
	t.Setenv("JS_ACK_WAIT", "0s")

	cfg := Config{Env: "dev", HTTPAddr: ":8080"}
	cfg2 := Load()
	cfg.jetStreamAckWaitErr = cfg2.jetStreamAckWaitErr

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected JS_ACK_WAIT validation to run even in dev")
	}
}

// TestGetEnvIntValidated and TestGetEnvInt32Validated cover the shared
// unset/valid/malformed/zero matrix directly against the low-level helpers,
// independent of which Config field consumes them.
func TestGetEnvIntValidated(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		fallback  int
		allowZero bool
		want      int
		wantErr   bool
	}{
		{name: "unset falls back, no error", raw: "", fallback: 60, want: 60},
		{name: "valid positive value", raw: "120", fallback: 60, want: 120},
		{name: "malformed value falls back with error", raw: "1O", fallback: 60, want: 60, wantErr: true},
		{name: "negative value falls back with error", raw: "-1", fallback: 60, want: 60, wantErr: true},
		{name: "explicit zero rejected when disallowed", raw: "0", fallback: 60, allowZero: false, want: 60, wantErr: true},
		{name: "explicit zero accepted when allowed", raw: "0", fallback: 60, allowZero: true, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_INT_VALIDATED", tc.raw)
			got, errMsg := getEnvIntValidated("TEST_INT_VALIDATED", tc.fallback, tc.allowZero)
			if got != tc.want {
				t.Errorf("value = %d, want %d", got, tc.want)
			}
			if tc.wantErr && errMsg == "" {
				t.Error("expected a non-empty error message")
			}
			if !tc.wantErr && errMsg != "" {
				t.Errorf("expected no error message, got %q", errMsg)
			}
		})
	}
}

func TestGetEnvInt32Validated(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		fallback  int32
		allowZero bool
		want      int32
		wantErr   bool
	}{
		{name: "unset falls back, no error", raw: "", fallback: 10, want: 10},
		{name: "valid positive value", raw: "25", fallback: 10, want: 25},
		{name: "malformed value falls back with error", raw: "1O", fallback: 10, want: 10, wantErr: true},
		{name: "out of int32 range falls back with error", raw: "99999999999", fallback: 10, want: 10, wantErr: true},
		{name: "explicit zero rejected when disallowed", raw: "0", fallback: 10, allowZero: false, want: 10, wantErr: true},
		{name: "explicit zero accepted when allowed", raw: "0", fallback: 10, allowZero: true, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_INT32_VALIDATED", tc.raw)
			got, errMsg := getEnvInt32Validated("TEST_INT32_VALIDATED", tc.fallback, tc.allowZero)
			if got != tc.want {
				t.Errorf("value = %d, want %d", got, tc.want)
			}
			if tc.wantErr && errMsg == "" {
				t.Error("expected a non-empty error message")
			}
			if !tc.wantErr && errMsg != "" {
				t.Errorf("expected no error message, got %q", errMsg)
			}
		})
	}
}

// TestValidate_DBMaxConnsMalformedFailsFast covers the acceptance criterion
// that DB_MAX_CONNS rejects a malformed value at Validate() time instead of
// silently keeping the default, mirroring the JS_ACK_WAIT precedent above.
func TestValidate_DBMaxConnsMalformedFailsFast(t *testing.T) {
	t.Setenv("DB_MAX_CONNS", "1O") // letter O, not zero

	cfg := Load()
	if cfg.DBMaxConns != 10 {
		t.Fatalf("expected fallback to default 10, got %d", cfg.DBMaxConns)
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for malformed DB_MAX_CONNS")
	}
	if !strings.Contains(err.Error(), "DB_MAX_CONNS") {
		t.Fatalf("error should mention DB_MAX_CONNS, got: %v", err)
	}
}

// TestValidate_RateLimitPublicPerMinMalformedFailsFast covers the acceptance
// criterion that RATE_LIMIT_PUBLIC_PER_MIN rejects a malformed value at
// Validate() time instead of silently keeping the default.
func TestValidate_RateLimitPublicPerMinMalformedFailsFast(t *testing.T) {
	t.Setenv("RATE_LIMIT_PUBLIC_PER_MIN", "not-a-number")

	cfg := Load()
	if cfg.RateLimitPublicPerMin != 300 {
		t.Fatalf("expected fallback to default 300, got %d", cfg.RateLimitPublicPerMin)
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for malformed RATE_LIMIT_PUBLIC_PER_MIN")
	}
	if !strings.Contains(err.Error(), "RATE_LIMIT_PUBLIC_PER_MIN") {
		t.Fatalf("error should mention RATE_LIMIT_PUBLIC_PER_MIN, got: %v", err)
	}
}

// TestValidate_RateLimitPublicPerMinZeroDisablesLimiterWithoutError documents
// that an explicit 0 is accepted, not rejected: it has real meaning
// (disables that rate limiter, per internal/api/ratelimit.go's `> 0` gate).
func TestValidate_RateLimitPublicPerMinZeroDisablesLimiterWithoutError(t *testing.T) {
	t.Setenv("RATE_LIMIT_PUBLIC_PER_MIN", "0")

	cfg := Load()
	if cfg.RateLimitPublicPerMin != 0 {
		t.Fatalf("expected explicit 0 to be honored, got %d", cfg.RateLimitPublicPerMin)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit RATE_LIMIT_PUBLIC_PER_MIN=0 should not fail validation, got: %v", err)
	}
}

// TestValidate_MaxBodyBytesZeroFailsFast documents the opposite case: unlike
// the rate limits, a body limit of 0 has no valid meaning (no request body
// could ever be accepted), so it must be rejected rather than silently
// coerced to the default.
func TestValidate_MaxBodyBytesZeroFailsFast(t *testing.T) {
	t.Setenv("MAX_BODY_BYTES", "0")

	cfg := Load()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for explicit MAX_BODY_BYTES=0")
	}
	if !strings.Contains(err.Error(), "MAX_BODY_BYTES") {
		t.Fatalf("error should mention MAX_BODY_BYTES, got: %v", err)
	}
}

// TestValidate_NumericFieldsUncheckedInEnvAllValidNoError is a smoke test
// that every converted field loads and validates cleanly when unset,
// consistent with "unset env vars continue to fall back to their documented
// defaults with no error."
func TestValidate_NumericFieldsUnsetLoadCleanly(t *testing.T) {
	for _, key := range []string{
		"DB_MAX_CONNS", "DB_MIN_CONNS", "DB_MAX_CONN_LIFETIME", "DB_MAX_CONN_IDLE_TIME",
		"SYNC_JOBS_MAX_ATTEMPTS", "SYNC_JOBS_BACKOFF_BASE", "SYNC_JOBS_BACKOFF_MAX",
		"SYNC_JOBS_FAILURE_ATTENTION_THRESHOLD", "SHUTDOWN_TIMEOUT", "WORKER_LIVENESS_STALE_THRESHOLD",
		"MAX_BODY_BYTES", "WEBHOOK_MAX_BODY_BYTES", "RATE_LIMIT_AUTH_PER_MIN", "RATE_LIMIT_PUBLIC_PER_MIN",
	} {
		t.Setenv(key, "")
	}

	cfg := Load()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("all-unset numeric config should not fail validation, got: %v", err)
	}
}
