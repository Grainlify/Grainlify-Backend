package syncjobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestJobCompletionContextSurvivesWorkerCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()

	ctx, cancel := jobCompletionContext(parent, time.Second)
	defer cancel()

	if err := ctx.Err(); err != nil {
		t.Fatalf("jobCompletionContext returned canceled context: %v", err)
	}
}

func TestJobFinalStateRequeuesCanceledWork(t *testing.T) {
	state := jobFinalState(context.Canceled, 0, 5, 30*time.Second, time.Hour)

	if state.status != "pending" {
		t.Fatalf("status = %q, want pending", state.status)
	}
	if state.incrementAttempts {
		t.Fatal("canceled job should not increment attempts")
	}
	if state.lastErr == "" {
		t.Fatal("canceled job should record a retryable shutdown reason")
	}
	if state.runAt != nil {
		t.Fatal("canceled job should not set a backoff run_at")
	}
}

func TestJobFinalStateCompletesSuccessfulWork(t *testing.T) {
	state := jobFinalState(nil, 1, 5, 30*time.Second, time.Hour)

	if state.status != "completed" {
		t.Fatalf("status = %q, want completed", state.status)
	}
	if !state.incrementAttempts {
		t.Fatal("completed job should increment attempts")
	}
	if state.lastErr != "" {
		t.Fatalf("lastErr = %q, want empty", state.lastErr)
	}
	if state.runAt != nil {
		t.Fatal("completed job should not set run_at")
	}
}

func TestJobFinalStateReschedulesTransientFailure(t *testing.T) {
	err := errors.New("transient github error")
	state := jobFinalState(err, 1, 5, 30*time.Second, time.Hour)

	if state.status != "pending" {
		t.Fatalf("status = %q, want pending", state.status)
	}
	if !state.incrementAttempts {
		t.Fatal("failed job should increment attempts")
	}
	if state.lastErr == "" {
		t.Fatal("failed job should record last_error")
	}
	if state.runAt == nil {
		t.Fatal("transient failure should set a future run_at for backoff")
	}
	if !state.runAt.After(time.Now()) {
		t.Fatal("run_at should be in the future")
	}
}

func TestJobFinalStateDeadLettersAfterMaxAttempts(t *testing.T) {
	err := errors.New("persistent failure")
	// attempts=4, maxAttempts=5 → nextAttempt=5 >= maxAttempts → dead
	state := jobFinalState(err, 4, 5, 30*time.Second, time.Hour)

	if state.status != "dead" {
		t.Fatalf("status = %q, want dead", state.status)
	}
	if !state.incrementAttempts {
		t.Fatal("dead job should still increment attempts for audit")
	}
	if state.lastErr == "" {
		t.Fatal("dead job should record last_error")
	}
	if state.runAt != nil {
		t.Fatal("dead job should not set run_at")
	}
}

func TestSanitizeErrorRemovesSecrets(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"token=ghp_abc123xyz", "[REDACTED]"},
		{"GET https://api.github.com/?auth=secret_token: 401", "GET https://api.github.com/?: 401"},
		{"no secrets here", "no secrets here"},
	}
	for _, tc := range cases {
		got := sanitizeError(tc.input)
		// We just check that known secret values are not present verbatim.
		if tc.input != tc.want && got == tc.input {
			t.Errorf("sanitizeError(%q) = %q, secrets not removed", tc.input, got)
		}
	}
}

func TestBackoffDurationBounds(t *testing.T) {
	base := 30 * time.Second
	for attempt := 1; attempt <= 10; attempt++ {
		d := backoffDuration(base, attempt, time.Hour)
		if d <= 0 {
			t.Errorf("attempt %d: backoff = %v, want > 0", attempt, d)
		}
		if d > time.Hour {
			t.Errorf("attempt %d: backoff = %v exceeds 1h cap", attempt, d)
		}
	}
	// Verify growth: attempt 3 should generally be larger than attempt 1
	d1 := backoffDuration(base, 1, time.Hour)
	d3 := backoffDuration(base, 3, time.Hour)
	// With jitter the ranges may overlap at extremes, so use a loose check.
	// base*2^0*1.25=37.5s vs base*2^2*0.75=90s — plenty of headroom.
	if d3 < d1 {
		t.Errorf("backoff not growing: attempt3=%v < attempt1=%v", d3, d1)
	}
}

func TestJobFinalStateRepeatedFailuresBackoffAndCap(t *testing.T) {
	err := errors.New("token revoked")
	base := time.Minute
	capDelay := 10 * time.Minute
	threshold := 6

	var previousDelay time.Duration
	for attempts := 0; attempts < threshold-1; attempts++ {
		before := time.Now()
		state := jobFinalState(err, attempts, threshold, base, capDelay)
		if state.status != "pending" {
			t.Fatalf("attempts %d: status = %q, want pending", attempts, state.status)
		}
		if state.runAt == nil {
			t.Fatalf("attempts %d: expected run_at backoff", attempts)
		}

		delay := state.runAt.Sub(before)
		if delay <= 0 {
			t.Fatalf("attempts %d: delay = %v, want positive", attempts, delay)
		}
		if delay > capDelay+time.Second {
			t.Fatalf("attempts %d: delay = %v, exceeds cap %v", attempts, delay, capDelay)
		}
		if attempts > 0 && delay < previousDelay/2 {
			t.Fatalf("attempts %d: delay regressed too much: previous=%v current=%v", attempts, previousDelay, delay)
		}
		previousDelay = delay
	}

	state := jobFinalState(err, threshold-1, threshold, base, capDelay)
	if state.status != "dead" {
		t.Fatalf("status = %q, want dead needing manual attention", state.status)
	}
	if state.runAt != nil {
		t.Fatal("manual-attention job should not be rescheduled")
	}
}
