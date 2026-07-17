package config

import (
	"encoding/base64"
	"strings"
	"testing"
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
