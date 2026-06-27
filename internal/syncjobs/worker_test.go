package syncjobs

import (
	"context"
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
	state := jobFinalState(context.Canceled)

	if state.status != "pending" {
		t.Fatalf("status = %q, want pending", state.status)
	}
	if state.incrementAttempts {
		t.Fatal("canceled job should not increment attempts")
	}
	if state.lastErr == "" {
		t.Fatal("canceled job should record a retryable shutdown reason")
	}
}

func TestJobFinalStateCompletesSuccessfulWork(t *testing.T) {
	state := jobFinalState(nil)

	if state.status != "completed" {
		t.Fatalf("status = %q, want completed", state.status)
	}
	if !state.incrementAttempts {
		t.Fatal("completed job should increment attempts")
	}
	if state.lastErr != "" {
		t.Fatalf("lastErr = %q, want empty", state.lastErr)
	}
}
