package soroban

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go/network"
)

func TestNewClient_RequiresRPCURL(t *testing.T) {
	_, err := NewClient(Config{Network: NetworkTestnet})
	if err == nil {
		t.Fatal("NewClient() succeeded with an empty RPCURL, want an error")
	}
	if !strings.Contains(err.Error(), "RPC URL") {
		t.Errorf("NewClient() error = %v, want it to mention the RPC URL", err)
	}
}

func TestNewClient_NetworkPassphraseDefaulting(t *testing.T) {
	tests := []struct {
		name               string
		network            Network
		explicitPassphrase string
		want               string
	}{
		{name: "mainnet defaults to public passphrase", network: NetworkMainnet, want: network.PublicNetworkPassphrase},
		{name: "testnet defaults to test passphrase", network: NetworkTestnet, want: network.TestNetworkPassphrase},
		{name: "zero-value network defaults to test passphrase", network: "", want: network.TestNetworkPassphrase},
		{name: "explicit passphrase respected on mainnet", network: NetworkMainnet, explicitPassphrase: "custom-passphrase", want: "custom-passphrase"},
		{name: "explicit passphrase respected on testnet", network: NetworkTestnet, explicitPassphrase: "custom-passphrase", want: "custom-passphrase"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClient(Config{
				RPCURL:            "http://localhost:8000",
				Network:           tc.network,
				NetworkPassphrase: tc.explicitPassphrase,
			})
			if err != nil {
				t.Fatalf("NewClient() error = %v, want nil", err)
			}
			if got := c.GetNetworkPassphrase(); got != tc.want {
				t.Errorf("GetNetworkPassphrase() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewClient_HTTPTimeoutDefaulting(t *testing.T) {
	t.Run("zero timeout defaults to 30s", func(t *testing.T) {
		c, err := NewClient(Config{RPCURL: "http://localhost:8000", Network: NetworkTestnet})
		if err != nil {
			t.Fatalf("NewClient() error = %v, want nil", err)
		}
		if c.httpClient.Timeout != 30*time.Second {
			t.Errorf("httpClient.Timeout = %v, want 30s", c.httpClient.Timeout)
		}
	})

	t.Run("explicit timeout respected", func(t *testing.T) {
		c, err := NewClient(Config{RPCURL: "http://localhost:8000", Network: NetworkTestnet, HTTPTimeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("NewClient() error = %v, want nil", err)
		}
		if c.httpClient.Timeout != 5*time.Second {
			t.Errorf("httpClient.Timeout = %v, want 5s", c.httpClient.Timeout)
		}
	})
}

func TestGetNetwork(t *testing.T) {
	c, err := NewClient(Config{RPCURL: "http://localhost:8000", Network: NetworkMainnet})
	if err != nil {
		t.Fatalf("NewClient() error = %v, want nil", err)
	}
	if got := c.GetNetwork(); got != NetworkMainnet {
		t.Errorf("GetNetwork() = %q, want %q", got, NetworkMainnet)
	}
}

func TestGetRPCURL(t *testing.T) {
	const rpcURL = "http://localhost:9000/rpc"
	c, err := NewClient(Config{RPCURL: rpcURL, Network: NetworkTestnet})
	if err != nil {
		t.Fatalf("NewClient() error = %v, want nil", err)
	}
	if got := c.GetRPCURL(); got != rpcURL {
		t.Errorf("GetRPCURL() = %q, want %q", got, rpcURL)
	}
}

func TestGetHorizonClient(t *testing.T) {
	c, err := NewClient(Config{RPCURL: "http://localhost:8000", Network: NetworkTestnet})
	if err != nil {
		t.Fatalf("NewClient() error = %v, want nil", err)
	}
	if c.GetHorizonClient() == nil {
		t.Error("GetHorizonClient() = nil, want a configured Horizon client")
	}
}

// TestLogContractInteraction_RedactsSensitiveArgs verifies LogContractInteraction
// delegates to logger.RedactMap for its info-level log, so a future refactor
// can't silently drop that redaction. The debug-level log intentionally
// includes the unredacted map for local troubleshooting and is untouched here.
func TestLogContractInteraction_RedactsSensitiveArgs(t *testing.T) {
	var buf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(origLogger)

	c, err := NewClient(Config{RPCURL: "http://localhost:8000", Network: NetworkTestnet})
	if err != nil {
		t.Fatalf("NewClient() error = %v, want nil", err)
	}

	c.LogContractInteraction("contract123", "deposit", map[string]interface{}{
		"address": "GDR5SECRETADDRESSXYZ",
		"amount":  1000,
		"note":    "safe_value",
	})

	output := buf.String()
	if strings.Contains(output, "GDR5SECRETADDRESSXYZ") {
		t.Errorf("expected address to be redacted, got: %s", output)
	}
	if strings.Contains(output, "1000") {
		t.Errorf("expected amount to be redacted, got: %s", output)
	}
	if !strings.Contains(output, "[REDACTED]") {
		t.Errorf("expected [REDACTED] placeholder in log, got: %s", output)
	}
	if !strings.Contains(output, "safe_value") {
		t.Errorf("expected non-sensitive field to be retained, got: %s", output)
	}
	if !strings.Contains(output, "contract123") || !strings.Contains(output, "deposit") {
		t.Errorf("expected contract_id and function in log, got: %s", output)
	}
}
