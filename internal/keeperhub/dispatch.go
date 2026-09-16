package keeperhub

import (
	"context"
	"encoding/json"
	"fmt"
)

// Recipient is one leg of a payout run.
//
// Amount is a STRING of exact minor units, never a float and never a number
// this package computes. The amounts come from hackathon.SettlementFor, where
// the apportionment is exact and asserted against the pool; re-deriving or
// re-rounding them here would produce a second rounding that cannot be
// reconciled against the first.
type Recipient struct {
	// Address is the frozen, EIP-55 checksummed destination for this leg.
	Address string `json:"address"`

	// AmountMinor is exact integer minor units, as a string.
	AmountMinor string `json:"amountMinor"`

	// LegID correlates this recipient with the keeperhub_payout_legs row it
	// came from, so a result can be attributed back to a leg without matching
	// on address - two legs in different runs can share an address, and
	// matching on money by string is how the wrong row gets marked paid.
	LegID string `json:"legId,omitempty"`
}

// DispatchAck is what the webhook endpoint returns. It is an ACKNOWLEDGEMENT,
// not a result.
//
// Verbatim, on HTTP 200:
//
//	{"executionId":"03xy32xnfx8rkqvjh0vjj","status":"running"}
//
// # "running" means accepted, and nothing else
//
// Measured on a real webhook-fired run: the response returned while the run was
// still going, and the execution finished roughly 0.9 seconds LATER
// (startedAt 09:38:31.839Z, completedAt 09:38:32.739Z). Nothing in the envelope
// said so.
//
// So this type deliberately has no Paid, Succeeded or Complete method, and
// nothing in this package infers an outcome from it. Completion is established
// only by polling Execution. A "dispatched therefore paid" inference anywhere
// would mark legs settled that may still fail, and those legs would then never
// be retried because only unpaid legs are ever sent.
type DispatchAck struct {
	ExecutionID string `json:"executionId"`
	Status      string `json:"status"`

	// IdempotencyKey is the key this attempt was sent under, returned so the
	// caller can record it against the dispatch attempt. Two attempts that
	// share a key share an execution, so storing it is how a confusing replay
	// is later explained.
	IdempotencyKey string `json:"-"`
}

// Dispatch fires the payout workflow with a recipient list.
//
// Spends the WEBHOOK key: this is the call that moves money. Every other method
// in this package spends the org key and only reads.
//
// The recipient list is delivered at the trigger's top level, so the workflow
// reads it as {{@<triggerNodeId>:Trigger.recipients}}.
func (c *Client) Dispatch(ctx context.Context, recipients []Recipient) (DispatchAck, error) {
	// An empty list is refused rather than sent. KeeperHub accepts an empty
	// object happily and returns an execution id, so dispatching nothing
	// produces a real, billable execution that pays nobody - and a caller
	// recording that id has recorded an attempt that never had any work in it.
	if len(recipients) == 0 {
		return DispatchAck{}, ErrNothingToDispatch
	}

	// A FRESH idempotency key for THIS ATTEMPT. Never one per payout run, never
	// reused across a retry or a resume.
	//
	// The endpoint dedupes on scope `webhook:<workflowId>`: an attempt carrying
	// a key and body it has seen before is not executed at all, and the
	// ORIGINAL executionId is replayed instead.
	//
	// That is precisely the failure this rail exists to prevent, seen from the
	// other side. We proved on chain that a partially failed For Each payout
	// re-pays settled legs when re-run, which is why only unpaid legs are ever
	// sent. A resume therefore carries a DIFFERENT, shorter recipient list and
	// must be a genuinely new execution.
	//
	// Reuse a key across that boundary and the dedupe hands back the FAILED
	// run's id. The resume pays nobody, reports an executionId that looks like
	// success, and Grainlify records an attempt that never happened - while the
	// unpaid legs stay unpaid and now appear to have been dispatched. The
	// people owed money are the ones who never find out.
	//
	// Per attempt is therefore the only correct scope. If a caller ever needs
	// to deduplicate a payout run, that belongs in the run and leg tables,
	// where "already paid" is a fact we hold, rather than in a remote cache
	// keyed on a request body.
	idempotencyKey := c.newIdempotencyKey()

	url := fmt.Sprintf("%s/api/workflows/%s/webhook", c.BaseURL, c.workflowID)
	payload := map[string]any{"recipients": recipients}

	status, raw, _, err := c.postJSON(ctx, url, c.webhookKey, payload, map[string]string{
		"Idempotency-Key": idempotencyKey,
	})
	failed := DispatchAck{IdempotencyKey: idempotencyKey}
	if err != nil {
		// No response. Rejected only if the failure provably happened before
		// the request left (the name did not resolve, the connection was
		// refused); otherwise KeeperHub may have received it and the run may be
		// executing. See dispatch_error.go.
		if status != 0 {
			// A response arrived and its body could not be read.
			return failed, &DispatchError{Class: DispatchIndeterminate, HTTPStatus: status, Err: err}
		}
		return failed, &DispatchError{Class: classifyTransport(err), Err: err}
	}
	if status < 200 || status >= 300 {
		code := remoteCode(raw)
		de := &DispatchError{
			Class:      classifyStatus(status, code),
			HTTPStatus: status,
			Code:       code,
			// Recorded even on a rejection: a 402 returns the id of an
			// execution it created and never started.
			ExecutionID: remoteExecutionID(raw),
			Err:         fmt.Errorf("%s", remoteError(status, raw)),
		}
		failed.ExecutionID = de.ExecutionID
		return failed, de
	}

	var ack DispatchAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		// Accepted, then unreadable: the run may well be executing.
		return failed, &DispatchError{Class: DispatchIndeterminate, HTTPStatus: status,
			Err: fmt.Errorf("decode dispatch response: %w", err)}
	}
	if ack.ExecutionID == "" {
		// A 2xx is ACCEPTANCE, so the run may be executing - but with no
		// execution id there is nothing to poll it by. Indeterminate, not
		// rejected: calling it a rejection would make legs resumable while an
		// accepted run could be paying them.
		return failed, &DispatchError{Class: DispatchIndeterminate, HTTPStatus: status,
			Err: fmt.Errorf("accepted with no executionId, so the run cannot be followed")}
	}
	ack.IdempotencyKey = idempotencyKey
	return ack, nil
}
