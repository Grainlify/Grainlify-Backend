package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
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
