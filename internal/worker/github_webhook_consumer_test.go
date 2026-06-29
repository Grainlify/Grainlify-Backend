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
	count  int
}

func (r *recordingIngestor) Ingest(ctx context.Context, _ events.GitHubWebhookReceived) error {
	r.called = true
	r.ctx = ctx
	r.count++
	return nil
}

func makeWebhookMsg(t *testing.T, deliveryID, event string) *nats.Msg {
	t.Helper()
	payload, err := json.Marshal(events.GitHubWebhookReceived{
		DeliveryID: deliveryID,
		Event:      event,
		Payload:    json.RawMessage(`{"action":"opened"}`),
	})
	if err != nil {
		t.Fatalf("marshal webhook event: %v", err)
	}
	return &nats.Msg{Data: payload}
}

// Duplicate X-GitHub-Delivery ID must be discarded — Ingest called only once.
func TestGitHubWebhookConsumer_DeduplicatesByDeliveryID(t *testing.T) {
	ingestor := &recordingIngestor{}
	consumer := &GitHubWebhookConsumer{Ingest: ingestor}
	ctx := context.Background()

	msg := makeWebhookMsg(t, "dup-delivery-1", "issues")
	consumer.handleMessage(ctx, msg)
	consumer.handleMessage(ctx, msg) // exact duplicate

	if ingestor.count != 1 {
		t.Errorf("expected Ingest called once, got %d", ingestor.count)
	}
}

// Different delivery IDs must each be processed.
func TestGitHubWebhookConsumer_DistinctDeliveryIDsAreEachProcessed(t *testing.T) {
	ingestor := &recordingIngestor{}
	consumer := &GitHubWebhookConsumer{Ingest: ingestor}
	ctx := context.Background()

	consumer.handleMessage(ctx, makeWebhookMsg(t, "delivery-A", "issues"))
	consumer.handleMessage(ctx, makeWebhookMsg(t, "delivery-B", "push"))
	consumer.handleMessage(ctx, makeWebhookMsg(t, "delivery-C", "pull_request"))

	if ingestor.count != 3 {
		t.Errorf("expected Ingest called 3 times, got %d", ingestor.count)
	}
}

// An empty DeliveryID must never be de-duplicated (pass-through always).
func TestGitHubWebhookConsumer_EmptyDeliveryIDAlwaysForwarded(t *testing.T) {
	ingestor := &recordingIngestor{}
	consumer := &GitHubWebhookConsumer{Ingest: ingestor}
	ctx := context.Background()

	consumer.handleMessage(ctx, makeWebhookMsg(t, "", "issues"))
	consumer.handleMessage(ctx, makeWebhookMsg(t, "", "issues"))

	if ingestor.count != 2 {
		t.Errorf("empty delivery ID must not be de-duplicated, got count=%d", ingestor.count)
	}
}

// markSeen must be goroutine-safe (no data race).
func TestGitHubWebhookConsumer_MarkSeenIsConcurrentlySafe(t *testing.T) {
	consumer := &GitHubWebhookConsumer{}
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func(n int) {
			consumer.markSeen("delivery-concurrent")
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}
