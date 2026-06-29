package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

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

// Subscribe creates a core NATS queue subscription (at-most-once, legacy path).
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

// JetStreamConsumerConfig holds configuration for a durable JetStream consumer.
type JetStreamConsumerConfig struct {
	// StreamName is the JetStream stream to subscribe to.
	StreamName string
	// ConsumerName is the durable consumer name (enables redelivery on crash/reconnect).
	ConsumerName string
	// MaxDeliver is the maximum number of delivery attempts before the message is dead-lettered.
	MaxDeliver int
	// AckWait is how long the server waits for an ack before redelivering.
	AckWait time.Duration
}

// GitHubWebhookJetStreamConsumer is a durable JetStream-backed consumer.
// It acks messages only after successful ingest and naks on failure,
// allowing the server to redeliver up to MaxDeliver times.
type GitHubWebhookJetStreamConsumer struct {
	Sub    *nats.Subscription
	Ingest GitHubWebhookIngestor
}

// SubscribeJetStream creates a durable JetStream push consumer for GitHub webhook events.
// The consumer will redeliver messages on failure up to cfg.MaxDeliver times.
func (c *GitHubWebhookJetStreamConsumer) SubscribeJetStream(
	ctx context.Context,
	js nats.JetStreamContext,
	cfg JetStreamConsumerConfig,
) error {
	if js == nil {
		return fmt.Errorf("jetstream context is required")
	}
	if cfg.StreamName == "" {
		return fmt.Errorf("stream name is required")
	}

	consumerName := cfg.ConsumerName
	if consumerName == "" {
		consumerName = GitHubWebhookQueueGroup
	}
	maxDeliver := cfg.MaxDeliver
	if maxDeliver <= 0 {
		maxDeliver = 5
	}
	ackWait := cfg.AckWait
	if ackWait <= 0 {
		ackWait = 30 * time.Second
	}

	sub, err := js.QueueSubscribe(
		events.SubjectGitHubWebhookReceived,
		consumerName,
		func(msg *nats.Msg) {
			c.handleJetStreamMessage(ctx, msg)
		},
		nats.Durable(consumerName),
		nats.AckExplicit(),
		nats.MaxDeliver(maxDeliver),
		nats.AckWait(ackWait),
		nats.BindStream(cfg.StreamName),
		nats.DeliverAll(),
	)
	if err != nil {
		return fmt.Errorf("jetstream queue subscribe: %w", err)
	}
	c.Sub = sub

	go func() {
		<-ctx.Done()
		_ = sub.Unsubscribe()
	}()

	slog.Info("JetStream durable consumer started",
		"stream", cfg.StreamName,
		"consumer", consumerName,
		"max_deliver", maxDeliver,
		"ack_wait", ackWait,
	)
	return nil
}

// handleJetStreamMessage processes a JetStream message with explicit ack/nak.
// On successful ingest the message is acked; on failure it is naked for redelivery.
func (c *GitHubWebhookJetStreamConsumer) handleJetStreamMessage(ctx context.Context, msg *nats.Msg) {
	var e events.GitHubWebhookReceived
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		slog.Error("bad github webhook event; acking to avoid infinite redelivery of unparseable message",
			"error", err,
			"subject", msg.Subject,
		)
		// Ack malformed messages so they don't loop forever.
		_ = msg.Ack()
		return
	}

	if c.Ingest == nil {
		_ = msg.Ack()
		return
	}

	if err := c.Ingest.Ingest(ctx, e); err != nil {
		slog.Error("webhook ingest failed; naking for redelivery",
			"error", err,
			"delivery_id", e.DeliveryID,
			"event", e.Event,
		)
		// Nak signals the server to redeliver up to MaxDeliver times.
		_ = msg.Nak()
		return
	}

	if err := msg.Ack(); err != nil {
		slog.Warn("jetstream ack failed", "error", err, "delivery_id", e.DeliveryID)
	}
}
