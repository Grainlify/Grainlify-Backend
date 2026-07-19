package soroban

import (
        "bytes"
        "context"
        "encoding/base64"
        "encoding/json"
        "fmt"
        "io"
        "log/slog"
        "net/http"
        "time"

        "github.com/stellar/go/keypair"
        "github.com/stellar/go/txnbuild"
        "github.com/stellar/go/xdr"
)

// RPCRequest represents a Soroban RPC JSON-RPC request
type RPCRequest struct {
        JSONRPC string      `json:"jsonrpc"`
        ID      int         `json:"id"`
        Method  string      `json:"method"`
        Params  interface{} `json:"params"`
}

// RPCResponse represents a Soroban RPC JSON-RPC response
type RPCResponse struct {
        JSONRPC string          `json:"jsonrpc"`
        ID      int             `json:"id"`
        Result  json.RawMessage `json:"result,omitempty"`
        Error   *RPCError       `json:"error,omitempty"`
}

// RPCError represents a Soroban RPC error
type RPCError struct {
        Code    int    `json:"code"`
        Message string `json:"message"`
        Data    string `json:"data,omitempty"`
}

// Call makes a JSON-RPC call to the Soroban RPC endpoint
func (c *Client) Call(ctx context.Context, method string, params interface{}) (*RPCResponse, error) {
        req := RPCRequest{
                JSONRPC: "2.0",
                ID:      1,
                Method:  method,
                Params:  params,
        }

        reqBody, err := json.Marshal(req)
        if err != nil {
                return nil, fmt.Errorf("failed to marshal request: %w", err)
        }

        httpReq, err := http.NewRequestWithContext(ctx, "POST", c.rpcURL, bytes.NewReader(reqBody))
        if err != nil {
                return nil, fmt.Errorf("failed to create request: %w", err)
        }

        httpReq.Header.Set("Content-Type", "application/json")

        resp, err := c.httpClient.Do(httpReq)
        if err != nil {
                return nil, fmt.Errorf("RPC call failed: %w", err)
        }
        defer resp.Body.Close()

        if resp.StatusCode != http.StatusOK {
                body, _ := io.ReadAll(resp.Body)
                return nil, fmt.Errorf("RPC call failed with status %d: %s", resp.StatusCode, string(body))
        }

        var rpcResp RPCResponse
        if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
                return nil, fmt.Errorf("failed to decode RPC response: %w", err)
        }

        if rpcResp.Error != nil {
                return nil, fmt.Errorf("RPC error: %s (code: %d)", rpcResp.Error.Message, rpcResp.Error.Code)
        }

        return &rpcResp, nil
}

// SimulateTransaction simulates a transaction using Soroban RPC
func (c *Client) SimulateTransaction(ctx context.Context, txEnvelopeXDR string) (map[string]interface{}, error) {
        params := map[string]interface{}{
                "transaction": txEnvelopeXDR,
        }

        resp, err := c.Call(ctx, "simulateTransaction", params)
        if err != nil {
                return nil, err
        }

        var result map[string]interface{}
        if err := json.Unmarshal(resp.Result, &result); err != nil {
                return nil, fmt.Errorf("failed to unmarshal result: %w", err)
        }

        return result, nil
}

// SendTransaction sends a transaction using Soroban RPC
func (c *Client) SendTransaction(ctx context.Context, txEnvelopeXDR string) (string, error) {
        params := map[string]interface{}{
                "transaction": txEnvelopeXDR,
        }

        resp, err := c.Call(ctx, "sendTransaction", params)
        if err != nil {
                return "", err
        }

        var result map[string]interface{}
        if err := json.Unmarshal(resp.Result, &result); err != nil {
                return "", fmt.Errorf("failed to unmarshal result: %w", err)
        }

        hash, ok := result["transactionHash"].(string)
        if !ok {
                return "", fmt.Errorf("invalid response: missing transactionHash")
        }

        return hash, nil
}

// GetTransactionStatus gets the status of a transaction
func (c *Client) GetTransactionStatus(ctx context.Context, txHash string) (map[string]interface{}, error) {
        params := map[string]interface{}{
                "hash": txHash,
        }

        resp, err := c.Call(ctx, "getTransaction", params)
        if err != nil {
                return nil, err
        }

        var result map[string]interface{}
        if err := json.Unmarshal(resp.Result, &result); err != nil {
                return nil, fmt.Errorf("failed to unmarshal result: %w", err)
        }

        return result, nil
}

// GetLatestLedger gets the latest ledger information
func (c *Client) GetLatestLedger(ctx context.Context) (map[string]interface{}, error) {
        resp, err := c.Call(ctx, "getLatestLedger", nil)
        if err != nil {
                return nil, err
        }

        var result map[string]interface{}
        if err := json.Unmarshal(resp.Result, &result); err != nil {
                return nil, fmt.Errorf("failed to unmarshal result: %w", err)
        }

        return result, nil
}

// simulateResult is the decoded result entry from simulateTransaction.
type simulateResult struct {
        XDR string `json:"xdr"`
}

// simulateTransactionResponse is the typed Soroban simulateTransaction response.
type simulateTransactionResponse struct {
        Results []simulateResult `json:"results"`
        Error   string           `json:"error,omitempty"`
}

// SimulateAndDecode builds a minimal unsigned transaction for the given operation,
// calls simulateTransaction, and returns the first result as a decoded ScVal.
//
// The transaction is never submitted; it only needs to be structurally valid
// enough for the RPC to simulate it. We use a dummy account/sequence so no
// live account or signing key is required.
func (c *Client) SimulateAndDecode(ctx context.Context, contractAddress xdr.ScAddress, functionName string, args []xdr.ScVal) (xdr.ScVal, error) {
        op, err := BuildInvokeHostFunctionOp(contractAddress, functionName, args)
        if err != nil {
                return xdr.ScVal{}, fmt.Errorf("build op: %w", err)
        }

        // Use a random ephemeral keypair as the source so we never need a real account.
        kp, err := keypair.Random()
        if err != nil {
                return xdr.ScVal{}, fmt.Errorf("keypair: %w", err)
        }

        tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
                SourceAccount:        &txnbuild.SimpleAccount{AccountID: kp.Address(), Sequence: 1},
                IncrementSequenceNum: false,
                BaseFee:              txnbuild.MinBaseFee,
                Operations:           []txnbuild.Operation{op},
                Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewInfiniteTimeout()},
        })
        if err != nil {
                return xdr.ScVal{}, fmt.Errorf("build tx: %w", err)
        }

        txXDR := tx.ToXDR()
        txBase64, err := txXDR.MarshalBinary()
        if err != nil {
                return xdr.ScVal{}, fmt.Errorf("marshal tx: %w", err)
        }

        params := map[string]interface{}{
                "transaction": base64.StdEncoding.EncodeToString(txBase64),
        }

        rpcResp, err := c.Call(ctx, "simulateTransaction", params)
        if err != nil {
                return xdr.ScVal{}, fmt.Errorf("simulateTransaction: %w", err)
        }

        var simResp simulateTransactionResponse
        if err := json.Unmarshal(rpcResp.Result, &simResp); err != nil {
                return xdr.ScVal{}, fmt.Errorf("decode simulate response: %w", err)
        }
        if simResp.Error != "" {
                return xdr.ScVal{}, fmt.Errorf("simulation error: %s", simResp.Error)
        }
        if len(simResp.Results) == 0 {
                return xdr.ScVal{}, fmt.Errorf("simulation returned no results")
        }

        resultBytes, err := base64.StdEncoding.DecodeString(simResp.Results[0].XDR)
        if err != nil {
                return xdr.ScVal{}, fmt.Errorf("decode result XDR: %w", err)
        }

        var scVal xdr.ScVal
        if err := xdr.SafeUnmarshal(resultBytes, &scVal); err != nil {
                return xdr.ScVal{}, fmt.Errorf("unmarshal ScVal: %w", err)
        }

        return scVal, nil
}

// inFlightPoll tracks a single in-progress PollTransactionStatus call so
// concurrent callers polling the same tx hash can share its result instead
// of each hammering the RPC endpoint independently.
type inFlightPoll struct {
        done   chan struct{}
        status map[string]interface{}
        err    error
}

// PollTransactionStatus polls for transaction status until confirmed or timeout.
//
// Concurrent or retried callers polling the *same* txHash share a single
// underlying poll loop: the first caller for a given hash becomes the
// "leader" and does the actual polling; subsequent callers for that same
// hash ("followers") block on the leader's result instead of starting a
// redundant poll loop of their own. Each follower still honors its own
// ctx cancellation/deadline independently of the leader, so a follower
// with a shorter deadline than the leader's poll will still return
// ctx.Err() promptly rather than waiting indefinitely for the leader.
// Once the leader's poll resolves (success or error), all waiters for
// that hash receive the identical result. This has no effect on
// single-caller usage.
func (c *Client) PollTransactionStatus(ctx context.Context, txHash string, timeout time.Duration) (map[string]interface{}, error) {
        c.mu.Lock()
        if existing, ok := c.inFlight[txHash]; ok {
                c.mu.Unlock()
                return waitForPoll(ctx, existing)
        }
        call := &inFlightPoll{done: make(chan struct{})}
        c.inFlight[txHash] = call
        c.mu.Unlock()

        status, err := c.pollTransactionStatusOnce(ctx, txHash, timeout)

        call.status, call.err = status, err
        close(call.done)

        c.mu.Lock()
        delete(c.inFlight, txHash)
        c.mu.Unlock()

        return status, err
}

// waitForPoll blocks until the leader's poll resolves or the caller's own
// context is done, whichever happens first.
func waitForPoll(ctx context.Context, call *inFlightPoll) (map[string]interface{}, error) {
        select {
        case <-call.done:
                return call.status, call.err
        case <-ctx.Done():
                return nil, ctx.Err()
        }
}

// pollTransactionStatusOnce contains the original polling loop, unchanged
// in behavior from before the dedup layer was added.
func (c *Client) pollTransactionStatusOnce(ctx context.Context, txHash string, timeout time.Duration) (map[string]interface{}, error) {
        deadline := time.Now().Add(timeout)
        ticker := time.NewTicker(2 * time.Second)
        defer ticker.Stop()

        for {
                select {
                case <-ctx.Done():
                        return nil, ctx.Err()
                case <-ticker.C:
                        if time.Now().After(deadline) {
                                return nil, fmt.Errorf("timeout waiting for transaction: %s", txHash)
                        }

                        status, err := c.GetTransactionStatus(ctx, txHash)
                        if err != nil {
                                // Transaction not found yet, continue polling
                                slog.Debug("transaction not found, continuing to poll",
                                        "tx_hash", txHash,
                                        "error", err,
                                )
                                continue
                        }

                        // Check status
                        if statusVal, ok := status["status"].(string); ok {
                                if statusVal == "SUCCESS" || statusVal == "FAILED" {
                                        slog.Info("transaction status determined",
                                                "tx_hash", txHash,
                                                "status", statusVal,
                                        )
                                        return status, nil
                                }
                        }

                        // Transaction found but status not final, continue polling
                        continue
                }
        }
}
