package handlers_test

import (
	"context"
	"testing"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// ---------------------------------------------------------------------------
// GitHubAppCleanupHandler is confirmed dead/unwired in production today (no
// caller in internal/api/api.go or cmd/ constructs or starts it), so this
// suite stays intentionally small: the priority is proving
// RunPeriodicCleanup respects context cancellation and returns promptly
// rather than blocking forever. Helpers are prefixed ghAppCleanupSuite to
// stay unique across the concurrently-written test files in this package.
//
// checkInstallations/checkSingleInstallation are unexported methods on
// *GitHubAppCleanupHandler; this file is package handlers_test (external),
// so they are not reachable here at all (the file wouldn't compile if it
// tried to call them). Exercising their "zero seeded installations" no-op
// query behavior would require a white-box (package handlers) test file,
// which is out of scope for this already-lowest-priority handler - see the
// final report for this note instead of a test.
// ---------------------------------------------------------------------------

func ghAppCleanupSuiteAwait(t *testing.T, fn func(), timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("call did not return within the expected timeout")
	}
}

func TestGitHubAppCleanupRunPeriodicCleanup_CancelledContextReturnsPromptly(t *testing.T) {
	// A non-empty GitHubAppID/GitHubAppPrivateKey is required here so
	// RunPeriodicCleanup doesn't take its "GitHub App not configured, skip"
	// early return (github_app_cleanup.go:29-32) - that would make this
	// test pass trivially without ever exercising the ctx.Done() branch of
	// the select loop (github_app_cleanup.go:39-47), which is what we
	// actually want to verify. The private key value is never parsed as a
	// real PEM here because checkInstallations (the only code path that
	// would touch it) is gated behind the 5-minute ticker, which the
	// already-cancelled context preempts before it can ever fire.
	cfg := config.Config{
		GitHubAppID:         "ghapp-cleanup-suite-app-id",
		GitHubAppPrivateKey: "ghapp-cleanup-suite-fake-private-key",
	}
	h := handlers.NewGitHubAppCleanupHandler(cfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before RunPeriodicCleanup is ever called

	ghAppCleanupSuiteAwait(t, func() { h.RunPeriodicCleanup(ctx) }, 2*time.Second)
}

// TestGitHubAppCleanupRunPeriodicCleanup_UnconfiguredAppReturnsImmediately
// documents the separate early-return guard (github_app_cleanup.go:29-32):
// when the GitHub App isn't configured, RunPeriodicCleanup returns
// immediately regardless of context state - note ctx here is NOT cancelled,
// unlike the test above, so a prompt return can only be explained by that
// guard, not by ctx.Done().
func TestGitHubAppCleanupRunPeriodicCleanup_UnconfiguredAppReturnsImmediately(t *testing.T) {
	cfg := config.Config{} // GitHubAppID and GitHubAppPrivateKey both empty
	h := handlers.NewGitHubAppCleanupHandler(cfg, nil)

	ghAppCleanupSuiteAwait(t, func() { h.RunPeriodicCleanup(context.Background()) }, 2*time.Second)
}
