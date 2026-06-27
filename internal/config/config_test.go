package config

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfigValidate(t *testing.T) {
	// Generate a valid 32-byte base64 key
	validKey := base64.StdEncoding.EncodeToString([]byte("abcdefghijklmnopqrstuvwxyz123456"))

	validConfig := Config{
		JWTSecret:                "super-secret-jwt-key",
		TokenEncKeyB64:           validKey,
		SorobanRPCURL:            "https://rpc.stellar.org",
		SorobanNetworkPassphrase: "Test SDF Network ; September 2015",
		SorobanSourceSecret:      "SCZBYL5W5Q44C27K4B5X6UQDPHH3F32RUX3N7X2HZEQ7J77242F7AQ7X",
		EscrowContractID:         "CCX...",
		TokenContractID:          "CDX...",
		ProgramEscrowContractID:  "CBX...",
	}

	t.Run("valid configuration", func(t *testing.T) {
		err := validConfig.Validate()
		assert.NoError(t, err)
	})

	t.Run("missing JWT_SECRET", func(t *testing.T) {
		cfg := validConfig
		cfg.JWTSecret = ""
		err := cfg.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "JWT_SECRET")
	})

	t.Run("missing TOKEN_ENC_KEY_B64", func(t *testing.T) {
		cfg := validConfig
		cfg.TokenEncKeyB64 = ""
		err := cfg.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "TOKEN_ENC_KEY_B64")
	})

	t.Run("invalid base64 TOKEN_ENC_KEY_B64", func(t *testing.T) {
		cfg := validConfig
		cfg.TokenEncKeyB64 = "invalid-base64-!!!"
		err := cfg.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "TOKEN_ENC_KEY_B64")
	})

	t.Run("short TOKEN_ENC_KEY_B64 (not 32 bytes)", func(t *testing.T) {
		cfg := validConfig
		cfg.TokenEncKeyB64 = base64.StdEncoding.EncodeToString([]byte("short-key"))
		err := cfg.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "TOKEN_ENC_KEY_B64")
	})

	t.Run("missing Soroban secrets", func(t *testing.T) {
		cfg := validConfig
		cfg.SorobanSourceSecret = ""
		err := cfg.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "SOROBAN_SOURCE_SECRET")
	})

	t.Run("invalid Soroban Stellar key prefix", func(t *testing.T) {
		cfg := validConfig
		// Doesn't start with 'S'
		cfg.SorobanSourceSecret = "GCZBYL5W5Q44C27K4B5X6UQDPHH3F32RUX3N7X2HZEQ7J77242F7AQ7X"
		err := cfg.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "SOROBAN_SOURCE_SECRET")
	})

	t.Run("invalid Soroban Stellar key length", func(t *testing.T) {
		cfg := validConfig
		// Length not 56
		cfg.SorobanSourceSecret = "SCZBYL5W5Q44C"
		err := cfg.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "SOROBAN_SOURCE_SECRET")
	})

	t.Run("missing Soroban contracts", func(t *testing.T) {
		cfg := validConfig
		cfg.EscrowContractID = ""
		err := cfg.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "ESCROW_CONTRACT_ID")
	})
}
