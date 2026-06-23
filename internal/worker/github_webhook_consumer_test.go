package worker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/jagadeesh/grainlify/backend/internal/events"
)

func TestGitHubWebhookQueueGroupDefault(t *testing.T) {
	if got := githubWebhookQueueGroup(""); got != GitHubWebhookQueueGroup {
		t.Fatalf("githubWebhookQueueGroup empty = %q, want %q", got, GitHubWebhookQueueGroup)
	}
	if got := githubWebhookQueueGroup("custom-workers"); got != "custom-workers" {
		t.Fatalf("githubWebhookQueueGroup custom = %q, want custom-workers", got)
	}
}

func TestGitHubWebhookConsumerUsesSubscriptionContextForIngest(t *testing.T) {
	type contextKey string

	const key contextKey = "shutdown-marker"
	ingestor := &recordingIngestor{}
	consumer := &GitHubWebhookConsumer{Ingest: ingestor}
	ctx := context.WithValue(context.Background(), key, "from-root")

	payload, err := json.Marshal(events.GitHubWebhookReceived{
		DeliveryID: "delivery-1",
		Event:      "issues",
		Payload:    json.RawMessage(`{"action":"opened"}`),
	})
	if err != nil {
		t.Fatalf("marshal webhook event: %v", err)
	}

	consumer.handleMessage(ctx, &nats.Msg{Data: payload})

	if !ingestor.called {
		t.Fatal("expected ingest to be called")
	}
	if got := ingestor.ctx.Value(key); got != "from-root" {
		t.Fatalf("ingest context marker = %v, want from-root", got)
	}
}

type recordingIngestor struct {
	called bool
	ctx    context.Context
}

func (r *recordingIngestor) Ingest(ctx context.Context, _ events.GitHubWebhookReceived) error {
	r.called = true
	r.ctx = ctx
	return nil
}
