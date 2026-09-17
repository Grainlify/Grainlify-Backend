package keeperhub

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Reading a run: status, and the per-leg results inside it.
//
// # Why this speaks MCP JSON-RPC rather than a REST route
//
// Not a preference - there is no per-execution REST route. Probed with the org
// key: /api/executions/<id>, /api/runs/<id>, /api/execution/<id>,
// /api/analytics/runs/<id> and /api/workflows/<id>/executions/<id> all return
// 404.
//
// The one REST route that works, GET /api/analytics/runs, is a SUMMARY LIST and
// carries no For Each iteration data at all, so it cannot answer "what happened
// to each leg". It is also actively dangerous to use for polling: it accepts
// ?executionId=<id> with HTTP 200 and IGNORES THE FILTER, returning every run.
// Code that trusted that parameter and read the first row would poll a
// different execution and report its outcome as this one's.
//
// The MCP endpoint's get_execution returns both the execution record and the
// per-iteration logs, so that is what this uses. Spends the ORG key: reading
// only.
const mcpProtocolVersion = "2024-11-05"

// Execution is one workflow run.
type Execution struct {
	ID          string
	Status      string
	StartedAt   *time.Time
	CompletedAt *time.Time

	// Evidence that this run was fired by the webhook rather than by hand.
	//
	// TriggerSource is "webhook" and CredentialType is "webhook_key" on a
	// webhook-fired run. **dispatchKey is deliberately not surfaced**: it is an
	// internal dedupe column and is null even on a webhook-fired run, so
	// reading it as a webhook marker would classify every real payout as
	// something else.
	TriggerSource  string
	CredentialType string

	// UserAPIKeyID is set and OrgAPIKeyID is empty on a webhook-fired run.
	// Attribution is user-scoped; see the package comment for why that is an
	// operational constraint rather than a detail.
	UserAPIKeyID    string
	OrgAPIKeyID     string
	CredentialLabel string

	// Billable records that webhook-fired runs are charged.
	Billable bool

	// Error is the run-level failure, if any.
	Error string

	// Input is the recipient list this execution was actually given, read back
	// from the execution record. The For Each iterates it in order, so
	// Input[iterationIndex] is the leg an iteration belongs to - and comparing it
	// with what was dispatched is how a replayed or foreign execution is caught.
	Input []Recipient

	// Legs are the per-iteration results of the For Each body - every body node
	// of every iteration that ran. Read from the per-iteration logs and NEVER
	// from the post-loop Collect: when any iteration fails the For Each fails
	// closed and Collect does not run, so the aggregate is absent in exactly the
	// partial-failure case that matters.
	Legs []LegResult
}

// LegResult is one iteration of the payout loop.
type LegResult struct {
	IterationIndex int
	ForEachNodeID  string
	NodeID         string
	NodeName       string
	Status         string
	Error          string

	// TxHash is output.transactionHash when the node reported one. Read
	// separately from Output because it is the one field reconciliation needs
	// regardless of which node paid: a broadcast transaction is evidence money
	// may have moved, whatever the step's status says.
	TxHash string

	// ChainID is output.chainId: the numeric chain KeeperHub reports the
	// transaction was broadcast on. Zero when the step reported none. This is
	// what a run's chain is verified against - what happened, not what was
	// configured.
	ChainID int64

	// Output is left raw. The shape depends on the node the workflow uses to
	// pay, and guessing at it here would bake one workflow's node into the
	// client. The caller that knows the workflow decodes it.
	Output json.RawMessage
}

// Terminal reports whether the run has finished.
//
// The point of the type: a caller must ask this rather than reading a status
// string, and "running" is NOT terminal however long ago it was dispatched.
// Note that "unknown" is not a KeeperHub status - it is the state Grainlify
// records for a leg it could not resolve, and it is deliberately not something
// this function can return.
func (e Execution) Terminal() bool {
	switch e.Status {
	case "success", "error", "system_error", "external_error", "cancelled":
		return true
	default:
		return false
	}
}

// Succeeded reports a run that finished cleanly.
//
// Separate from Terminal because a finished run is not a successful one, and
// collapsing the two is how a failed payout gets recorded as paid.
func (e Execution) Succeeded() bool { return e.Status == "success" }

// Execution reads a run's status and per-leg results. Spends the ORG key.
func (c *Client) Execution(ctx context.Context, executionID string) (Execution, error) {
	if strings.TrimSpace(executionID) == "" {
		return Execution{}, fmt.Errorf("keeperhub: execution id is empty")
	}
	raw, err := c.mcpCall(ctx, "get_execution", map[string]any{
		"executionId": executionID,
		"includeData": true,
	})
	if err != nil {
		return Execution{}, err
	}

	var payload struct {
		Logs struct {
			Execution struct {
				ID                        string  `json:"id"`
				Status                    string  `json:"status"`
				StartedAt                 *string `json:"startedAt"`
				CompletedAt               *string `json:"completedAt"`
				Error                     *string `json:"error"`
				TriggerSource             string  `json:"triggerSource"`
				TriggeredByCredentialType string  `json:"triggeredByCredentialType"`
				TriggeredByUserAPIKeyID   *string `json:"triggeredByUserApiKeyId"`
				TriggeredByOrgAPIKeyID    *string `json:"triggeredByOrgApiKeyId"`
				CredentialLabel           *string `json:"triggeredByCredentialLabel"`
				Billable                  bool    `json:"billable"`
				Input                     struct {
					Recipients []Recipient `json:"recipients"`
				} `json:"input"`
			} `json:"execution"`
			Logs []struct {
				NodeID         string          `json:"nodeId"`
				NodeName       string          `json:"nodeName"`
				Status         string          `json:"status"`
				Error          *string         `json:"error"`
				IterationIndex *int            `json:"iterationIndex"`
				ForEachNodeID  *string         `json:"forEachNodeId"`
				Output         json.RawMessage `json:"output"`
			} `json:"logs"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Execution{}, fmt.Errorf("keeperhub: decode execution: %w", err)
	}

	ex := payload.Logs.Execution
	out := Execution{
		ID:              ex.ID,
		Status:          ex.Status,
		StartedAt:       parseTime(ex.StartedAt),
		CompletedAt:     parseTime(ex.CompletedAt),
		TriggerSource:   ex.TriggerSource,
		CredentialType:  ex.TriggeredByCredentialType,
		UserAPIKeyID:    deref(ex.TriggeredByUserAPIKeyID),
		OrgAPIKeyID:     deref(ex.TriggeredByOrgAPIKeyID),
		CredentialLabel: deref(ex.CredentialLabel),
		Billable:        ex.Billable,
		Error:           deref(ex.Error),
		Input:           ex.Input.Recipients,
	}
	if out.ID == "" {
		out.ID = executionID
	}

	// Only entries that are genuinely loop iterations. A Collect node carries a
	// forEachNodeId but no iterationIndex, and it holds the AGGREGATE of every
	// iteration - counting it as a leg would add a phantom leg whose output is
	// the whole list.
	for _, l := range payload.Logs.Logs {
		if l.IterationIndex == nil {
			continue
		}
		var tx struct {
			TransactionHash string          `json:"transactionHash"`
			ChainID         json.RawMessage `json:"chainId"`
		}
		_ = json.Unmarshal(l.Output, &tx)
		out.Legs = append(out.Legs, LegResult{
			TxHash:         tx.TransactionHash,
			ChainID:        parseChainID(tx.ChainID),
			IterationIndex: *l.IterationIndex,
			ForEachNodeID:  deref(l.ForEachNodeID),
			NodeID:         l.NodeID,
			NodeName:       l.NodeName,
			Status:         l.Status,
			Error:          deref(l.Error),
			Output:         l.Output,
		})
	}
	return out, nil
}

// parseChainID accepts the number KeeperHub sends (observed: "chainId": 84532)
// or the same value as a string. Anything else is 0, which never matches a
// run's chain and therefore fails closed.
func parseChainID(raw json.RawMessage) int64 {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// parseTime returns nil rather than the zero time for an absent timestamp.
//
// A zero time.Time formats as year 1 and compares as "very long ago", so a
// missing completedAt would read as a run that finished before it started.
// Absence stays absence.
func parseTime(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil
	}
	return &t
}

// mcpCall performs one JSON-RPC tools/call against the MCP endpoint.
//
// The handshake is initialize -> notifications/initialized -> tools/call, and
// the session id returned by initialize is reused for later calls.
func (c *Client) mcpCall(ctx context.Context, tool string, args map[string]any) ([]byte, error) {
	if err := c.ensureMCPSession(ctx); err != nil {
		return nil, err
	}
	res, err := c.mcpRPC(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return nil, err
	}

	// A tools/call result carries its payload as TEXT inside content[0], so the
	// real body is a JSON document embedded in a JSON string.
	var wrapper struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &wrapper); err != nil {
		return nil, fmt.Errorf("keeperhub: decode %s result: %w", tool, err)
	}
	if len(wrapper.Content) == 0 {
		return nil, fmt.Errorf("%w: %s returned no content", ErrRemote, tool)
	}
	if wrapper.IsError {
		return nil, fmt.Errorf("%w: %s: %s", ErrRemote, tool, wrapper.Content[0].Text)
	}
	return []byte(wrapper.Content[0].Text), nil
}

func (c *Client) ensureMCPSession(ctx context.Context) error {
	c.mu.Lock()
	have := c.mcpSession != ""
	c.mu.Unlock()
	if have {
		return nil
	}
	if _, err := c.mcpRPC(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "grainlify-backend", "version": "1"},
	}); err != nil {
		return err
	}
	// A notification: no id, and no response is expected.
	_, _ = c.mcpRPC(ctx, "notifications/initialized", map[string]any{})
	return nil
}

func (c *Client) mcpRPC(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	body := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	notification := method == "notifications/initialized"
	if !notification {
		body["id"] = uuidLike()
	}

	c.mu.Lock()
	session := c.mcpSession
	c.mu.Unlock()
	extra := map[string]string{}
	if session != "" {
		extra["Mcp-Session-Id"] = session
	}

	status, raw, header, err := c.postJSON(ctx, c.BaseURL+"/mcp", c.orgAPIKey, body, extra)
	if err != nil {
		return nil, err
	}
	if sid := header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.mcpSession = sid
		c.mu.Unlock()
	}
	if notification {
		return nil, nil
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%w: %s: %s", ErrRemote, method, remoteError(status, raw))
	}

	// The endpoint may answer as JSON or as a single server-sent event, so both
	// are accepted rather than assuming the one seen most recently.
	payload := raw
	if line := sseData(raw); line != nil {
		payload = line
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("keeperhub: decode %s envelope: %w", method, err)
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("%w: %s: %s (code %d)", ErrRemote, method,
			envelope.Error.Message, envelope.Error.Code)
	}
	return envelope.Result, nil
}

// sseData extracts the payload of a text/event-stream response, or nil.
func sseData(raw []byte) []byte {
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "data:") {
			return []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return nil
}
