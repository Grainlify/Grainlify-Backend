package keeperhub

import (
	"context"
	"errors"
	"testing"
)

// A partial failure, shaped on a real one.
//
// Modelled on execution 8om5c2jiw0jyoxk2k8npq (Base Sepolia, 2026-09-15): the
// first transfer settled, the second failed on balance, the For Each failed
// closed and the post-loop Collect never ran. Extended to three legs so that one
// dispatched leg has NO per-iteration log at all - the case a caller must never
// read as success. Each iteration has two body nodes (the minor->human
// conversion and the transfer), as the real payout workflow does.
//
// No credentials appear in it.
const partialFailure = `{
 "logs": {
  "execution": {
   "id": "exec-partial",
   "status": "error",
   "error": "Insufficient USDC balance. Have: 1.0, Need: 2",
   "triggerSource": "webhook",
   "triggeredByCredentialType": "webhook_key",
   "input": {"recipients": [
     {"address": "0xAaAa000000000000000000000000000000000001", "amountMinor": "1000000", "legId": "leg-a"},
     {"address": "0xBbBb000000000000000000000000000000000002", "amountMinor": "2000000", "legId": "leg-b"},
     {"address": "0xCcCc000000000000000000000000000000000003", "amountMinor": "3000000", "legId": "leg-c"}
   ]},
   "output": {"arrayLength": 3, "iterationsRan": 2, "failedIterations": 1,
              "firstFailureError": "Insufficient USDC balance. Have: 1.0, Need: 2",
              "firstFailureNodeId": "transfer-1"}
  },
  "logs": [
   {"nodeId":"transfer-1","nodeName":"Transfer","status":"error","iterationIndex":1,"forEachNodeId":"foreach-1",
    "error":"Insufficient USDC balance. Have: 1.0, Need: 2",
    "output":{"success":false,"error":"Insufficient USDC balance. Have: 1.0, Need: 2"}},
   {"nodeId":"to-human","nodeName":"To Human","status":"success","iterationIndex":1,"forEachNodeId":"foreach-1",
    "output":{"success":true,"value":"2"}},
   {"nodeId":"transfer-1","nodeName":"Transfer","status":"success","iterationIndex":0,"forEachNodeId":"foreach-1",
    "output":{"success":true,"chainId":84532,"amount":"1",
              "transactionHash":"0x5195212356269cb9f21cb3dd0c4b56497f264cc63298052974a9a45286157725"}},
   {"nodeId":"to-human","nodeName":"To Human","status":"success","iterationIndex":0,"forEachNodeId":"foreach-1",
    "output":{"success":true,"value":"1"}},
   {"nodeId":"foreach-1","nodeName":"For Each","status":"error","iterationIndex":null,"forEachNodeId":null,
    "output":{"success":true}},
   {"nodeId":"trigger-1","nodeName":"Trigger","status":"success","iterationIndex":null,"forEachNodeId":null,
    "output":{}}
  ]
 }
}`

var partialDispatch = []Recipient{
	{Address: "0xAaAa000000000000000000000000000000000001", AmountMinor: "1000000", LegID: "leg-a"},
	{Address: "0xBbBb000000000000000000000000000000000002", AmountMinor: "2000000", LegID: "leg-b"},
	{Address: "0xCcCc000000000000000000000000000000000003", AmountMinor: "3000000", LegID: "leg-c"},
}

func readExecution(t *testing.T, body string) Execution {
	t.Helper()
	c, _ := newServer(t, 200, "", body)
	ex, err := c.Execution(context.Background(), "exec-partial")
	if err != nil {
		t.Fatalf("Execution: %v", err)
	}
	return ex
}

// THE silent-failure mode. A leg that was dispatched and has no result must be
// a typed, hard error - never an outcome list that simply lacks it, which reads
// exactly like a smaller successful run.
func TestReconcile_ALegWithNoResultIsAHardError(t *testing.T) {
	ex := readExecution(t, partialFailure)

	outcomes, err := Reconcile(partialDispatch, ex)
	if err == nil {
		t.Fatalf("leg-c was dispatched and has no result, and Reconcile returned %d outcome(s) "+
			"with no error - a silent partial success", len(outcomes))
	}
	if !errors.Is(err, ErrLegsWithoutResult) {
		t.Fatalf("err = %v, want ErrLegsWithoutResult", err)
	}
	var missing *MissingResultsError
	if !errors.As(err, &missing) {
		t.Fatalf("err is not a *MissingResultsError: %T", err)
	}
	if len(missing.LegIDs) != 1 || missing.LegIDs[0] != "leg-c" {
		t.Errorf("missing legs = %v, want exactly [leg-c]", missing.LegIDs)
	}

	// The legs that DID report still come back, so intake can record them -
	// failing closed on one leg must not discard what is known about the rest.
	byLeg := map[string]LegOutcome{}
	for _, o := range outcomes {
		byLeg[o.LegID] = o
	}
	if a := byLeg["leg-a"]; a.Status != LegConfirmed || a.TxHash == "" {
		t.Errorf("leg-a = %+v, want confirmed with its tx hash", a)
	}
	if b := byLeg["leg-b"]; b.Status != LegFailed || b.Error == "" {
		t.Errorf("leg-b = %+v, want failed with the balance error", b)
	}
	if _, present := byLeg["leg-c"]; present {
		t.Error("leg-c was given an outcome it never had")
	}
}

// The idempotency replay seen from the reading side: an execution whose input is
// not what we sent is somebody else's run, and its results are not ours.
func TestReconcile_RefusesAnExecutionWhoseInputIsNotWhatWasSent(t *testing.T) {
	ex := readExecution(t, partialFailure)

	// A resume sends only leg-c. If the dispatch had been deduped onto the
	// original run, this is the execution we would be reading.
	_, err := Reconcile(partialDispatch[2:], ex)
	if !errors.Is(err, ErrExecutionInputMismatch) {
		t.Fatalf("err = %v, want ErrExecutionInputMismatch - reading the original run's "+
			"results as the resume's would credit leg-c with leg-a's payment", err)
	}
}

func TestReconcile_RefusesARunStillInProgress(t *testing.T) {
	ex := readExecution(t, partialFailure)
	ex.Status = "running"
	if _, err := Reconcile(partialDispatch, ex); !errors.Is(err, ErrExecutionNotTerminal) {
		t.Fatalf("err = %v, want ErrExecutionNotTerminal - a missing result on a running "+
			"execution means not yet, not never", err)
	}
}

// A step that errored AFTER broadcasting may have moved money. That is unknown,
// not failed, because failed legs are retried.
func TestReconcile_AnErrorAfterABroadcastIsUnknown(t *testing.T) {
	ex := readExecution(t, partialFailure)
	for i := range ex.Legs {
		if ex.Legs[i].IterationIndex == 1 && ex.Legs[i].NodeID == "transfer-1" {
			ex.Legs[i].TxHash = "0xdeadbeef"
		}
	}
	outcomes, _ := Reconcile(partialDispatch, ex)
	for _, o := range outcomes {
		if o.LegID == "leg-b" && o.Status != LegUnknown {
			t.Fatalf("leg-b errored with a tx hash on record and was classified %q - "+
				"a retry of it could pay twice", o.Status)
		}
	}
}

// Success with no transaction is a claim without evidence.
func TestReconcile_SuccessWithoutATxHashIsUnknown(t *testing.T) {
	ex := readExecution(t, partialFailure)
	for i := range ex.Legs {
		if ex.Legs[i].IterationIndex == 0 {
			ex.Legs[i].TxHash = ""
		}
	}
	outcomes, _ := Reconcile(partialDispatch, ex)
	for _, o := range outcomes {
		if o.LegID == "leg-a" && o.Status != LegUnknown {
			t.Fatalf("leg-a reported success with no transaction and was classified %q", o.Status)
		}
	}
}

func TestReconcile_RefusesDuplicateLegIDs(t *testing.T) {
	ex := readExecution(t, partialFailure)
	dup := append([]Recipient{}, partialDispatch...)
	dup[2].LegID = "leg-a"
	if _, err := Reconcile(dup, ex); !errors.Is(err, ErrDuplicateLegID) {
		t.Fatalf("err = %v, want ErrDuplicateLegID - reconciliation by legId needs them unique", err)
	}
}

func TestExecution_ReadsTheDispatchedInput(t *testing.T) {
	ex := readExecution(t, partialFailure)
	if len(ex.Input) != 3 || ex.Input[2].LegID != "leg-c" || ex.Input[1].AmountMinor != "2000000" {
		t.Fatalf("input = %+v", ex.Input)
	}
	for _, l := range ex.Legs {
		if l.NodeID == "transfer-1" && l.IterationIndex == 0 && l.TxHash == "" {
			t.Error("the settled transfer's transaction hash was not read")
		}
	}
}
