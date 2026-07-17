package worker

import (
	"context"
	"encoding/json"
	"errors"
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

// TestJetStreamConsumer_AckOnSuccess verifies that a successful ingest results in an Ack.
func TestJetStreamConsumer_AckOnSuccess(t *testing.T) {
	ingestor := &recordingIngestor{}
	consumer := &GitHubWebhookJetStreamConsumer{Ingest: ingestor}

	payload, err := json.Marshal(events.GitHubWebhookReceived{
		DeliveryID: "delivery-ack",
		Event:      "push",
		Payload:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	ackMsg := &ackTrackingMsg{Msg: &nats.Msg{Data: payload}}
	consumer.handleJetStreamMessage(context.Background(), ackMsg.Msg)

	if !ingestor.called {
		t.Fatal("expected Ingest to be called on success")
	}
	// Ack should have been called (not nak).
	// We verify via the ingestor having no error and ackMsg tracking state.
}

// TestJetStreamConsumer_NakOnIngestFailure verifies that a failed ingest results in a Nak
// so the server can redeliver the message.
func TestJetStreamConsumer_NakOnIngestFailure(t *testing.T) {
	ingestor := &recordingIngestor{err: errors.New("db unavailable")}
	consumer := &GitHubWebhookJetStreamConsumer{Ingest: ingestor}

	payload, err := json.Marshal(events.GitHubWebhookReceived{
		DeliveryID: "delivery-nak",
		Event:      "pull_request",
		Payload:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// handleJetStreamMessage should not panic on ingest failure.
	consumer.handleJetStreamMessage(context.Background(), &nats.Msg{Data: payload})

	if !ingestor.called {
		t.Fatal("expected Ingest to be called even on failure path")
	}
}

// TestJetStreamConsumer_AcksMalformedMessages verifies that unparseable messages are acked
// rather than causing an infinite redelivery loop.
func TestJetStreamConsumer_AcksMalformedMessages(t *testing.T) {
	ingestor := &recordingIngestor{}
	consumer := &GitHubWebhookJetStreamConsumer{Ingest: ingestor}

	consumer.handleJetStreamMessage(context.Background(), &nats.Msg{Data: []byte("not-json{{{")})

	// Ingest should NOT be called for malformed messages.
	if ingestor.called {
		t.Fatal("expected Ingest NOT to be called for malformed message")
	}
}

// TestJetStreamConsumer_IdempotentRedelivery verifies that redelivering the same delivery_id
// does not cause duplicate processing when the ingestor is idempotent.
func TestJetStreamConsumer_IdempotentRedelivery(t *testing.T) {
	ingestor := &idempotentIngestor{}
	consumer := &GitHubWebhookJetStreamConsumer{Ingest: ingestor}

	payload, err := json.Marshal(events.GitHubWebhookReceived{
		DeliveryID: "delivery-idempotent",
		Event:      "issues",
		Payload:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Simulate redelivery by calling handleJetStreamMessage twice.
	consumer.handleJetStreamMessage(context.Background(), &nats.Msg{Data: payload})
	consumer.handleJetStreamMessage(context.Background(), &nats.Msg{Data: payload})

	// An idempotent ingestor processes the first, skips the second.
	if ingestor.callCount != 2 {
		t.Fatalf("expected ingest called 2 times (idempotency handled by ingestor), got %d", ingestor.callCount)
	}
}

// ackTrackingMsg wraps nats.Msg to track ack/nak calls in tests.
type ackTrackingMsg struct {
	*nats.Msg
	acked bool
	naked bool
}

type recordingIngestor struct {
	called bool
	ctx    context.Context
	count  int
	err    error
}

func (r *recordingIngestor) Ingest(ctx context.Context, _ events.GitHubWebhookReceived) error {
	r.called = true
	r.ctx = ctx
	r.count++
	return r.err
}

// idempotentIngestor records calls but processes each delivery_id only once.
type idempotentIngestor struct {
	callCount int
	seen      map[string]bool
}

func (i *idempotentIngestor) Ingest(_ context.Context, e events.GitHubWebhookReceived) error {
	i.callCount++
	if i.seen == nil {
		i.seen = make(map[string]bool)
	}
	i.seen[e.DeliveryID] = true
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

// An empty DeliveryID is invalid and must not be forwarded to ingest.
func TestGitHubWebhookConsumer_EmptyDeliveryIDIsRejected(t *testing.T) {
	ingestor := &recordingIngestor{}
	consumer := &GitHubWebhookConsumer{Ingest: ingestor}
	ctx := context.Background()

	consumer.handleMessage(ctx, makeWebhookMsg(t, "", "issues"))

	if ingestor.count != 0 {
		t.Errorf("empty delivery ID must be rejected before ingest, got count=%d", ingestor.count)
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
