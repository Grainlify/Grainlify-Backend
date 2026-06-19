package worker

import "testing"

func TestGitHubWebhookQueueGroupDefault(t *testing.T) {
	if got := githubWebhookQueueGroup(""); got != GitHubWebhookQueueGroup {
		t.Fatalf("githubWebhookQueueGroup empty = %q, want %q", got, GitHubWebhookQueueGroup)
	}
	if got := githubWebhookQueueGroup("custom-workers"); got != "custom-workers" {
		t.Fatalf("githubWebhookQueueGroup custom = %q, want custom-workers", got)
	}
}
