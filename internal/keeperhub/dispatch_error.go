package keeperhub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"syscall"
)

// Classifying a failed dispatch.
//
// # Why this exists
//
// A failed dispatch used to be one thing: "unknown", every leg of it blocked
// behind a person reading the chain. That is right when we genuinely do not know
// whether the run started. It is wrong for a 401. KeeperHub refusing the key is
// a certainty - nothing ran - and stranding the legs behind a manual
// reconciliation for a condition we already know the answer to makes the safe
// path so expensive that people learn to skip it.
//
// So a failure is classified, and only two outcomes exist:
//
//	DispatchRejected       nothing was dispatched; the legs are resumable
//	DispatchIndeterminate  the run may have started; the legs are unknown
//
// # The direction every doubt resolves in
//
// An unrecognised status is INDETERMINATE, never rejected. Wrongly calling a
// rejection indeterminate costs a person a chain read. Wrongly calling an
// indeterminate failure a rejection makes its legs resumable while the first
// request may still be paying them, and the resume pays them twice. The two
// mistakes are not symmetric, so the default is not a coin toss.
type DispatchClass string

const (
	DispatchRejected      DispatchClass = "rejected"
	DispatchIndeterminate DispatchClass = "indeterminate"
)

// ErrDispatchIndeterminate is the sentinel for a dispatch whose outcome is
// unknown. ErrDispatchRefused is the sentinel for a definitive rejection.
var ErrDispatchIndeterminate = errors.New("keeperhub: dispatch outcome indeterminate")

// DispatchError is every failure Dispatch returns after the request was built.
type DispatchError struct {
	Class DispatchClass

	// HTTPStatus is 0 when no response arrived.
	HTTPStatus int

	// Code is KeeperHub's machine-readable code when it sent one, e.g.
	// invalid_key_format or idempotency_in_progress.
	Code string

	// ExecutionID can be set on a REJECTED dispatch. A 402 (pay-as-you-go
	// block) creates the execution row and marks it error without starting the
	// run, and returns its id. Having an execution id is therefore not evidence
	// that anything was dispatched, and nothing may infer that it is.
	ExecutionID string

	Err error
}

func (e *DispatchError) Error() string {
	return fmt.Sprintf("keeperhub: dispatch %s (status %d%s): %v", e.Class, e.HTTPStatus, codeSuffix(e.Code), e.Err)
}

func codeSuffix(c string) string {
	if c == "" {
		return ""
	}
	return ", code " + c
}

func (e *DispatchError) Unwrap() []error {
	sentinel := ErrDispatchIndeterminate
	if e.Class == DispatchRejected {
		sentinel = ErrDispatchRefused
	}
	return []error{sentinel, e.Err}
}

// IsRejected reports whether err is a dispatch KeeperHub definitively did not
// run. False for every other error, including ones this package did not
// produce: only a classified rejection may make legs resumable.
func IsRejected(err error) bool {
	var de *DispatchError
	return errors.As(err, &de) && de.Class == DispatchRejected
}

// classifyStatus decides what a non-2xx response means.
func classifyStatus(status int, code string) DispatchClass {
	switch status {
	case 400, // malformed request: validation happens before any run starts
		401, // key refused
		402, // pay-as-you-go block: the row is created and marked error, the run never starts
		403, // key not permitted
		404, // no such workflow
		410, // workflow disabled - observed: {"error":"Workflow is disabled"}, no execution recorded
		422, // payload refused
		429: // rate limited before admission
		return DispatchRejected

	case 409:
		// NOT a clean refusal. idempotency_in_progress means the FIRST request
		// under this key is still running and may still pay. A conflict with a
		// different body means a key is already bound to some request. Neither
		// says nothing ran, so neither may make legs resumable, whatever the code.
		_ = code
		return DispatchIndeterminate

	case 408:
		// A timeout reported by the server: the request may have been processed.
		return DispatchIndeterminate

	default:
		// 5xx, and anything not listed above.
		return DispatchIndeterminate
	}
}

// classifyTransport decides what a failure with no response means.
//
// Only failures that provably happened before the request left are rejections:
// the name did not resolve, or the connection was refused. A timeout, a reset or
// an EOF can all happen after KeeperHub received the request, so they are not.
func classifyTransport(err error) DispatchClass {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return DispatchRejected
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return DispatchRejected
	}
	return DispatchIndeterminate
}

func remoteCode(raw []byte) string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &e)
	return e.Code
}

func remoteExecutionID(raw []byte) string {
	var e struct {
		ExecutionID string `json:"executionId"`
	}
	_ = json.Unmarshal(raw, &e)
	return e.ExecutionID
}
