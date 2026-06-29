package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/jagadeesh/grainlify/backend/internal/events"
)

type GitHubWebhookIngestor interface {
	Ingest(context.Context, events.GitHubWebhookReceived) error
}

// GitHubWebhookConsumer subscribes to the NATS webhook subject and dispatches
// events to the configured Ingestor. Duplicate X-GitHub-Delivery IDs are
// detected and discarded before Ingest is called (in-process de-duplication).
type GitHubWebhookConsumer struct {
	Sub    *nats.Subscription
	Ingest GitHubWebhookIngestor

	// seenMu guards seenDeliveryIDs.
	seenMu           sync.Mutex
	seenDeliveryIDs  map[string]struct{}
	// maxSeenIDs caps the in-memory set size; eviction drops the oldest by insertion-order
	// approximation (reset the map when full). Default 0 means no cap.
	maxSeenIDs int
}

// GitHubWebhookQueueGroup is the shared NATS queue group for webhook workers.
const GitHubWebhookQueueGroup = "grainlify-workers"

// defaultMaxSeenIDs is the default cap for the in-memory seen-set.
// When the set reaches this size it is cleared (cheap, safe approximation of LRU).
const defaultMaxSeenIDs = 10_000

func githubWebhookQueueGroup(queue string) string {
	if queue == "" {
		return GitHubWebhookQueueGroup
	}
	return queue
}

func (c *GitHubWebhookConsumer) Subscribe(ctx context.Context, nc *nats.Conn, queue string) error {
	if nc == nil {
		return nil
	}
	queue = githubWebhookQueueGroup(queue)

	sub, err := nc.QueueSubscribe(events.SubjectGitHubWebhookReceived, queue, func(msg *nats.Msg) {
		c.handleMessage(ctx, msg)
	})
	if err != nil {
		return err
	}
	c.Sub = sub

	go func() {
		<-ctx.Done()
		_ = sub.Unsubscribe()
	}()

	return nil
}

// markSeen returns true if deliveryID has been seen before (duplicate), false if new.
// Thread-safe. An empty deliveryID is never de-duplicated (pass-through).
func (c *GitHubWebhookConsumer) markSeen(deliveryID string) bool {
	if deliveryID == "" {
		return false
	}
	c.seenMu.Lock()
	defer c.seenMu.Unlock()

	if c.seenDeliveryIDs == nil {
		c.seenDeliveryIDs = make(map[string]struct{})
	}

	if _, ok := c.seenDeliveryIDs[deliveryID]; ok {
		return true
	}

	cap := c.maxSeenIDs
	if cap == 0 {
		cap = defaultMaxSeenIDs
	}
	if len(c.seenDeliveryIDs) >= cap {
		c.seenDeliveryIDs = make(map[string]struct{})
	}
	c.seenDeliveryIDs[deliveryID] = struct{}{}
	return false
}

func (c *GitHubWebhookConsumer) handleMessage(ctx context.Context, msg *nats.Msg) {
	var e events.GitHubWebhookReceived
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		slog.Error("bad github webhook event", "error", err)
		return
	}

	// De-duplicate by X-GitHub-Delivery ID before forwarding to Ingest.
	if c.markSeen(e.DeliveryID) {
		slog.Debug("duplicate github webhook delivery discarded", "delivery_id", e.DeliveryID)
		return
	}

	if c.Ingest == nil {
		return
	}
	if err := c.Ingest.Ingest(ctx, e); err != nil {
		slog.Error("webhook ingest failed", "error", err)
	}
}
