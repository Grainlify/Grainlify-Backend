package soroban

import (
	"errors"
	"sync"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/metrics"
)

// breakerState is the current state of a circuitBreaker. The numeric values
// are exported via metrics.SorobanCircuitBreakerState, so they must stay in
// sync with that gauge's documented meaning (0=closed, 1=open, 2=half-open).
type breakerState int

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

// ErrCircuitOpen is returned by Call (rpc.go) when the circuit breaker is
// open, or half-open with a probe already in flight, so the caller fails
// fast instead of attempting another RPC request.
var ErrCircuitOpen = errors.New("soroban: circuit breaker open, failing fast")

// circuitBreaker guards outbound Soroban RPC calls made via Client.Call. It
// trips to open after threshold consecutive failures. Once cooldown has
// elapsed since it opened, the next call is let through as a half-open probe;
// a successful probe closes the breaker, while a failed probe reopens it and
// restarts the cooldown. Only one probe is allowed in flight at a time.
type circuitBreaker struct {
	threshold int
	cooldown  time.Duration

	mu       sync.Mutex
	state    breakerState
	failures int
	openedAt time.Time
}

func newCircuitBreaker(threshold int, cooldown time.Duration) *circuitBreaker {
	return &circuitBreaker{threshold: threshold, cooldown: cooldown, state: breakerClosed}
}

// Allow reports whether a call may proceed, returning ErrCircuitOpen if not.
// When called on an open breaker whose cooldown has elapsed, Allow moves the
// breaker to half-open and lets this single call through as a probe.
func (b *circuitBreaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case breakerClosed:
		return nil
	case breakerOpen:
		if time.Since(b.openedAt) < b.cooldown {
			return ErrCircuitOpen
		}
		b.state = breakerHalfOpen
		b.setStateMetric()
		return nil
	default: // breakerHalfOpen: a probe is already outstanding
		return ErrCircuitOpen
	}
}

// RecordSuccess reports a successful call, closing the breaker and resetting
// its consecutive-failure count.
func (b *circuitBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.state = breakerClosed
	b.setStateMetric()
}

// RecordFailure reports a failed call. A failed half-open probe reopens the
// breaker immediately and restarts the cooldown; otherwise the breaker opens
// once threshold consecutive failures have been recorded.
func (b *circuitBreaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == breakerHalfOpen {
		b.state = breakerOpen
		b.openedAt = time.Now()
		b.setStateMetric()
		return
	}

	b.failures++
	if b.failures >= b.threshold {
		b.state = breakerOpen
		b.openedAt = time.Now()
		b.setStateMetric()
	}
}

// setStateMetric publishes the current state to Prometheus. Callers must hold b.mu.
func (b *circuitBreaker) setStateMetric() {
	metrics.SorobanCircuitBreakerState.Set(float64(b.state))
}
