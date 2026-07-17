package soroban

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/logger"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/network"
)

// Client wraps the Soroban JSON-RPC transport and Horizon client used by the
// backend for Stellar contract interactions.
//
// RPC method helpers implemented in rpc.go use the rpcURL and httpClient fields
// to call Soroban RPC. Those helpers return terminal errors for malformed local
// input, JSON encoding/decoding failures, non-OK HTTP responses, and JSON-RPC
// error payloads; callers should only retry them when the operation is safe to
// repeat and the wrapped cause is transient, such as a context deadline, network
// timeout, or temporary service unavailability. Transaction-building and
// submission retry behavior is implemented separately by TransactionBuilder in
// tx.go.
type Client struct {
	rpcURL            string
	networkPassphrase string
	horizonClient     *horizonclient.Client
	httpClient        *http.Client
	network           Network
}

// Config holds the endpoint, network, and timeout settings used by NewClient.
//
// RPCURL is required and is passed to the JSON-RPC helpers in rpc.go.
// NetworkPassphrase is optional; when empty, NewClient derives the Stellar public
// or test network passphrase from Network. HTTPTimeout controls both the Soroban
// RPC HTTP client and the Horizon client used by TransactionBuilder in tx.go.
// Configuration validation errors returned by NewClient are terminal; retrying
// with the same Config will return the same error.
type Config struct {
	RPCURL            string  // Soroban RPC endpoint
	NetworkPassphrase string  // Network passphrase
	Network           Network // "testnet" or "mainnet"
	HTTPTimeout       time.Duration
}

// NewClient validates cfg and creates a Client for Soroban RPC and Horizon.
//
// RPCURL must be non-empty. If NetworkPassphrase is empty, NewClient defaults it
// from Network: NetworkMainnet selects Stellar's public network passphrase and
// all other values select the test network passphrase. If HTTPTimeout is zero,
// a 30-second timeout is used for both RPC and Horizon requests.
//
// NewClient performs no network I/O, so its errors are terminal configuration
// errors rather than retryable transport failures. Retry policy for RPC calls is
// described on Client and implemented by the rpc.go methods; transaction
// submission retries are handled by TransactionBuilder in tx.go.
func NewClient(cfg Config) (*Client, error) {
	if cfg.RPCURL == "" {
		return nil, fmt.Errorf("RPC URL is required")
	}

	if cfg.NetworkPassphrase == "" {
		// Set default based on network
		if cfg.Network == NetworkMainnet {
			cfg.NetworkPassphrase = network.PublicNetworkPassphrase
		} else {
			cfg.NetworkPassphrase = network.TestNetworkPassphrase
		}
	}

	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 30 * time.Second
	}

	// Create Horizon client
	horizonURL := "https://horizon-testnet.stellar.org"
	if cfg.Network == NetworkMainnet {
		horizonURL = "https://horizon.stellar.org"
	}

	horizonClient := &horizonclient.Client{
		HorizonURL: horizonURL,
		HTTP: &http.Client{
			Timeout: cfg.HTTPTimeout,
		},
	}

	return &Client{
		rpcURL:            cfg.RPCURL,
		networkPassphrase: cfg.NetworkPassphrase,
		horizonClient:     horizonClient,
		httpClient: &http.Client{
			Timeout: cfg.HTTPTimeout,
		},
		network: cfg.Network,
	}, nil
}

// GetNetwork returns the configured Stellar network for this client.
//
// The value controls defaults such as the Horizon URL selected by NewClient and
// is used in diagnostic logging. GetNetwork performs no I/O and never returns an
// error; there are no retry semantics for this accessor. See tx.go for methods
// that use this value during transaction construction and submission.
func (c *Client) GetNetwork() Network {
	return c.network
}

// GetNetworkPassphrase returns the Stellar network passphrase used for signing.
//
// TransactionBuilder in tx.go calls this accessor before signing transactions.
// The method performs no I/O and never returns an error; signing failures in
// tx.go are terminal for the transaction being built, while network submission
// failures may be retried according to the builder's RetryConfig.
func (c *Client) GetNetworkPassphrase() string {
	return c.networkPassphrase
}

// GetHorizonClient returns the Horizon client associated with this Client.
//
// The returned client is used by tx.go to load source accounts, submit
// transactions, and poll for confirmations. GetHorizonClient itself performs no
// I/O and never returns an error. Errors from Horizon operations should be
// classified by the caller; TransactionBuilder.submitWithRetry treats malformed
// transaction state such as bad auth, bad sequence, insufficient balance, and a
// missing source account as terminal, while other submission errors are retried.
func (c *Client) GetHorizonClient() *horizonclient.Client {
	return c.horizonClient
}

// GetRPCURL returns the Soroban JSON-RPC endpoint URL configured for this client.
//
// The rpc.go helpers use this URL for methods such as simulateTransaction,
// sendTransaction, and getTransaction. GetRPCURL performs no I/O and never
// returns an error; connection, timeout, HTTP status, and JSON-RPC errors arise
// from the rpc.go call methods and are only retryable when the underlying cause
// is transient and the specific RPC operation is safe to repeat.
func (c *Client) GetRPCURL() string {
	return c.rpcURL
}

// LogContractInteraction writes structured diagnostics for a contract call.
//
// contractID identifies the Soroban contract, function names the invoked
// contract function, and args contains the argument values. The info-level log
// redacts args through the shared logger, while the debug-level log includes the
// unredacted map for local troubleshooting.
//
// LogContractInteraction performs no RPC call, does not participate in the
// retry behavior described in rpc.go or tx.go, and never returns an error. Avoid
// using it as evidence that a transaction was simulated, submitted, or
// confirmed; use the RPC and transaction helpers for those states.
func (c *Client) LogContractInteraction(contractID, function string, args map[string]interface{}) {
	redactedArgs := logger.RedactMap(args)

	slog.Info("contract interaction",
		"contract_id", contractID,
		"function", function,
		"network", c.network,
		"args", redactedArgs,
	)

	// Detailed debugging includes unredacted args
	slog.Debug("contract interaction detailed",
		"contract_id", contractID,
		"function", function,
		"network", c.network,
		"args", args,
	)
}
