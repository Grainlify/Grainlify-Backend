// Package chainread answers questions the chain is the authority for.
//
// # Why these are read live and never cached
//
// Two values live here: whether a leaf has been claimed, and when the claim
// window closes. Both are on chain, both can change without us doing anything,
// and a stale copy of either is worse than a round trip.
//
// The deadline is the sharper case. `extend_deadline` is the DOCUMENTED REMEDY
// for a late claimant - we extend the window while somebody recovers a wallet -
// and it is admin-gated and one-directional, so the only thing it ever does is
// give people more time. A cached deadline shows that person the OLD date right
// after we extended it for them: the interface contradicting the fix we just
// applied, at the moment they are deciding whether it is hopeless. That is a
// cache whose staleness works against the remedy it caches around.
//
// So there is no column, no reconciler refresh and no TTL. One endpoint reads
// both in a single round trip, which is what removes the only argument a cache
// had.
package chainread

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	ErrNoEndpoint = errors.New("chain_endpoint_not_configured")
	// ErrNodeFailed is the node being unreachable or refusing to answer.
	ErrNodeFailed = errors.New("chain_node_unreachable")
	// ErrContractAborted is the node answering perfectly with a Move abort.
	//
	// Split from ErrNodeFailed because they are opposite facts wearing the same
	// HTTP status: a view call that aborts comes back 400, and calling that
	// "unreachable" tells an operator to check the network when the answer is
	// that the escrow does not exist. One name per cause.
	ErrContractAborted = errors.New("contract_aborted")
	ErrBadReply        = errors.New("chain_node_returned_something_unexpected")
)

// moveAbort extracts the abort code from a node error body, e.g.
// "E_NOT_INITIALISED(0x2)". Empty when the body is not a Move abort.
var moveAbortRe = regexp.MustCompile(`E_[A-Z_]+\(0x[0-9a-fA-F]+\)`)

// Client reads view functions from an Aptos fullnode.
type Client struct {
	nodeURL string
	http    *http.Client
}

// EndpointFor resolves the node URL for a chain.
//
// chain_configs stores the NAME of an environment variable, not the URL, so that
// no endpoint carrying an API key is ever written into a migration. This is the
// one place that indirection is resolved, and an unset variable is an error
// naming both the chain and the variable - never a silent fallback to a public
// node, which would work in development and mislead in production.
func EndpointFor(envVarName string) (string, error) {
	if envVarName == "" {
		return "", fmt.Errorf("%w: chain has no rpc_endpoint_ref", ErrNoEndpoint)
	}
	v := os.Getenv(envVarName)
	if v == "" {
		return "", fmt.Errorf("%w: %s is not set, so no node can be reached", ErrNoEndpoint, envVarName)
	}
	return v, nil
}

func New(nodeURL string) *Client {
	return &Client{nodeURL: nodeURL, http: &http.Client{Timeout: 10 * time.Second}}
}

// view calls a Move view function and returns the raw JSON array.
func (c *Client) view(ctx context.Context, function string, args []any) ([]json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"function":       function,
		"type_arguments": []string{},
		"arguments":      args,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.nodeURL+"/v1/view", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNodeFailed, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))

	// The upstream status and a bounded body, before returning our own error.
	// A view call that aborts returns 400 with the Move abort in the body, and
	// discarding it turns a precise contract error into "the node said no".
	if res.StatusCode != http.StatusOK {
		// A Move abort is the contract answering, not the node failing. Both
		// arrive as 400, and reporting the first as unreachable sends an
		// operator to check the network when the escrow simply does not exist.
		if ab := moveAbortRe.Find(raw); ab != nil {
			return nil, fmt.Errorf("%w: %s (status %d, %s)", ErrContractAborted, ab, res.StatusCode, function)
		}
		return nil, fmt.Errorf("%w: status %d: %s", ErrNodeFailed, res.StatusCode, string(raw))
	}
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: %v (body: %s)", ErrBadReply, err, string(raw))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: view returned no values", ErrBadReply)
	}
	return out, nil
}

// IsClaimed reports whether one leaf has been claimed.
func (c *Client) IsClaimed(ctx context.Context, module, escrow string, leafHex string) (bool, error) {
	out, err := c.view(ctx, module+"::escrow::is_claimed", []any{escrow, leafHex})
	if err != nil {
		return false, err
	}
	var b bool
	if err := json.Unmarshal(out[0], &b); err != nil {
		return false, fmt.Errorf("%w: is_claimed returned %s", ErrBadReply, out[0])
	}
	return b, nil
}

// ClaimDeadline returns the window close as a unix timestamp.
func (c *Client) ClaimDeadline(ctx context.Context, module, escrow string) (int64, error) {
	out, err := c.view(ctx, module+"::escrow::claim_deadline", []any{escrow})
	if err != nil {
		return 0, err
	}
	// Move u64 comes back as a JSON STRING, not a number: 2^64 does not fit in
	// a float64 and the node knows it. Unmarshalling into an int64 fails, which
	// is the correct failure and an easy one to "fix" by widening the type.
	var s string
	if err := json.Unmarshal(out[0], &s); err != nil {
		return 0, fmt.Errorf("%w: claim_deadline returned %s, expected a quoted u64", ErrBadReply, out[0])
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: claim_deadline %q is not a number", ErrBadReply, s)
	}
	return n, nil
}

// Root returns the published root of an escrow, or false if none is published.
// Balance is what the escrow still holds, and it is how a sweep is observed.
//
// The module has no `swept` flag: sweep_unclaimed withdraws
// fungible_asset::balance(escrow.store) in full and emits SweptUnclaimed, so
// afterwards the balance is zero and there is nothing else to look at.
//
// That is a better signal than a flag would be for the question actually being
// asked. "Was a sweep run" is not what a post-deadline reminder needs to know -
// it needs to know whether this person's money is still there to claim, and a
// balance answers that directly. A fully-claimed escrow reads the same as a
// swept one, which is correct: in both cases there is nothing to tell anybody
// to come and collect.
func (c *Client) Balance(ctx context.Context, module, escrow string) (uint64, error) {
	out, err := c.view(ctx, module+"::escrow::balance", []any{escrow})
	if err != nil {
		return 0, err
	}
	if len(out) == 0 {
		return 0, fmt.Errorf("chainread.Balance: empty view result")
	}
	var raw string
	if err := json.Unmarshal(out[0], &raw); err != nil {
		return 0, fmt.Errorf("chainread.Balance: decode: %w", err)
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("chainread.Balance: parse %q: %w", raw, err)
	}
	return v, nil
}

func (c *Client) Root(ctx context.Context, module, escrow string) ([]byte, bool, error) {
	out, err := c.view(ctx, module+"::escrow::root", []any{escrow})
	if err != nil {
		return nil, false, err
	}
	// Option<vector<u8>> comes back as {"vec":["0x…"]} or {"vec":[]}.
	var opt struct {
		Vec []string `json:"vec"`
	}
	if err := json.Unmarshal(out[0], &opt); err != nil {
		return nil, false, fmt.Errorf("%w: root returned %s", ErrBadReply, out[0])
	}
	if len(opt.Vec) == 0 {
		return nil, false, nil
	}
	b, err := hex.DecodeString(strings.TrimPrefix(opt.Vec[0], "0x"))
	if err != nil {
		return nil, false, fmt.Errorf("%w: root %q is not hex", ErrBadReply, opt.Vec[0])
	}
	return b, true, nil
}
