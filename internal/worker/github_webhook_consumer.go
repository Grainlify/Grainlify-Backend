package worker

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/jagadeesh/grainlify/backend/internal/bus/natsbus"
	"github.com/jagadeesh/grainlify/backend/internal/events"
	"github.com/jagadeesh/grainlify/backend/internal/liveness"
	shutdownwait "github.com/jagadeesh/grainlify/backend/internal/shutdown"
)

type GitHubWebhookIngestor interface {
	Ingest(context.Context, events.GitHubWebhookReceived) error
}

// GitHubWebhookConsumer subscribes to the NATS webhook subject and dispatches
// events to the configured Ingestor. Duplicate X-GitHub-Delivery IDs are
// detected and discarded before Ingest is called (in-process de-duplication).
//
// De-duplication correctness guarantee: a delivery ID is only recorded as
// "seen" after Ingest succeeds. If Ingest fails the ID remains eligible for
// re-processing on the next GitHub redelivery. Concurrent redeliveries of the
// same ID that arrive while one attempt is in flight are detected via the
// inFlightIDs set and discarded for that window; they will be retried on the
// next redelivery once the in-flight attempt has finished (and either succeeded
// or failed).
type GitHubWebhookConsumer struct {
	Sub    *nats.Subscription
	Ingest GitHubWebhookIngestor

	// LivenessTracker is updated on every successfully handled message so that
	// the /healthz endpoint can detect whether the worker is making progress.
	LivenessTracker *liveness.Tracker

	// ShutdownGracePeriod bounds how long shutdown waits for in-flight message
	// handlers before cancelling their contexts. Defaults to
	// defaultShutdownGracePeriod.
	ShutdownGracePeriod time.Duration

	wg sync.WaitGroup

	processingMu     sync.RWMutex
	processingCtx    context.Context
	cancelProcessing context.CancelFunc

	// seenMu guards seenDeliveryIDs, seenDeliveryList, and inFlightIDs.
	seenMu           sync.Mutex
	seenDeliveryIDs  map[string]*list.Element
	seenDeliveryList *list.List
	// inFlightIDs tracks delivery IDs whose Ingest call has started but not yet
	// succeeded or permanently failed. A concurrent redelivery of the same ID
	// is treated like a duplicate for the duration of the in-flight attempt.
	inFlightIDs map[string]struct{}
	// maxSeenIDs caps the in-memory set size; eviction drops the oldest by insertion-order
	// approximation (reset the map when full). Default 0 means no cap.
	maxSeenIDs int
}

// GitHubWebhookQueueGroup is the shared NATS queue group for webhook workers.
const GitHubWebhookQueueGroup = "grainlify-workers"

// defaultMaxSeenIDs is the default cap for the in-memory seen-set.
// When the set reaches this size it is cleared (cheap, safe approximation of LRU).
const defaultMaxSeenIDs = 10_000

const defaultShutdownGracePeriod = 10 * time.Second

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
	processingCtx := c.initProcessingContext(ctx)

	sub, err := nc.QueueSubscribe(events.SubjectGitHubWebhookReceived, queue, func(msg *nats.Msg) {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.handleMessage(processingCtx, msg)
		}()
	})
	if err != nil {
		return err
	}
	c.Sub = sub

	go c.drainOnShutdown(ctx, sub)

	return nil
}

func (c *GitHubWebhookConsumer) initProcessingContext(ctx context.Context) context.Context {
	c.processingMu.Lock()
	defer c.processingMu.Unlock()

	if c.cancelProcessing != nil {
		c.cancelProcessing()
	}
	processingCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.processingCtx = processingCtx
	c.cancelProcessing = cancel
	return processingCtx
}

func (c *GitHubWebhookConsumer) processingContext(ctx context.Context) context.Context {
	c.processingMu.RLock()
	processingCtx := c.processingCtx
	c.processingMu.RUnlock()
	if processingCtx != nil {
		return processingCtx
	}
	return ctx
}

func (c *GitHubWebhookConsumer) shutdownGracePeriod() time.Duration {
	if c.ShutdownGracePeriod > 0 {
		return c.ShutdownGracePeriod
	}
	return defaultShutdownGracePeriod
}

func (c *GitHubWebhookConsumer) drainOnShutdown(ctx context.Context, sub *nats.Subscription) {
	<-ctx.Done()
	if sub != nil {
		_ = sub.Unsubscribe()
	}

	graceCtx, cancelGrace := context.WithTimeout(context.Background(), c.shutdownGracePeriod())
	defer cancelGrace()

	if err := shutdownwait.Wait(graceCtx, &c.wg); err != nil {
		slog.Warn("github webhook consumer shutdown grace period exceeded; cancelling in-flight messages", "error", err)
		c.processingMu.RLock()
		cancelProcessing := c.cancelProcessing
		c.processingMu.RUnlock()
		if cancelProcessing != nil {
			cancelProcessing()
		}
		return
	}

	slog.Info("github webhook consumer drained in-flight messages")
}

// alreadyProcessed returns true if deliveryID has already been successfully
// ingested (permanent duplicate) or is currently being processed by another
// goroutine (transient in-flight duplicate).
// Thread-safe. An empty deliveryID always returns false (pass-through).
func (c *GitHubWebhookConsumer) alreadyProcessed(deliveryID string) bool {
	if deliveryID == "" {
		return false
	}
	c.seenMu.Lock()
	defer c.seenMu.Unlock()

	if c.seenDeliveryIDs != nil {
		if _, ok := c.seenDeliveryIDs[deliveryID]; ok {
			return true // already successfully ingested
		}
	}
	if c.inFlightIDs != nil {
		if _, ok := c.inFlightIDs[deliveryID]; ok {
			return true // currently in flight — treat as transient duplicate
		}
	}
	return false
}

// beginInFlight marks deliveryID as in-flight so that concurrent redeliveries
// of the same ID are treated as transient duplicates while the current attempt
// is running. Call clearInFlight when the attempt finishes.
func (c *GitHubWebhookConsumer) beginInFlight(deliveryID string) {
	if deliveryID == "" {
		return
	}
	c.seenMu.Lock()
	if c.inFlightIDs == nil {
		c.inFlightIDs = make(map[string]struct{})
	}
	c.inFlightIDs[deliveryID] = struct{}{}
	c.seenMu.Unlock()
}

// clearInFlight removes deliveryID from the in-flight set. Always call this
// after an Ingest attempt (success or failure).
func (c *GitHubWebhookConsumer) clearInFlight(deliveryID string) {
	if deliveryID == "" {
		return
	}
	c.seenMu.Lock()
	if c.inFlightIDs != nil {
		delete(c.inFlightIDs, deliveryID)
	}
	c.seenMu.Unlock()
}

// recordProcessed marks deliveryID as permanently processed (successful ingest).
// Subsequent calls to alreadyProcessed for this ID will return true.
func (c *GitHubWebhookConsumer) recordProcessed(deliveryID string) {
	if deliveryID == "" {
		return
	}
	c.seenMu.Lock()
	defer c.seenMu.Unlock()

	if c.seenDeliveryIDs == nil {
		c.seenDeliveryIDs = make(map[string]*list.Element)
		c.seenDeliveryList = list.New()
	}

	if _, ok := c.seenDeliveryIDs[deliveryID]; ok {
		return // already recorded
	}

	cap := c.maxSeenIDs
	if cap == 0 {
		cap = defaultMaxSeenIDs
	}

	elem := c.seenDeliveryList.PushBack(deliveryID)
	c.seenDeliveryIDs[deliveryID] = elem

	if c.seenDeliveryList.Len() > cap {
		oldest := c.seenDeliveryList.Front()
		if oldest != nil {
			c.seenDeliveryList.Remove(oldest)
			delete(c.seenDeliveryIDs, oldest.Value.(string))
		}
	}
}

func (c *GitHubWebhookConsumer) handleMessage(ctx context.Context, msg *nats.Msg) {
	ctx = c.processingContext(ctx)
	var e events.GitHubWebhookReceived
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		slog.Error("bad github webhook event", "error", err)
		return
	}
	if err := e.Validate(); err != nil {
		slog.Error("invalid github webhook event", "error", err)
		return
	}

	// De-duplicate by X-GitHub-Delivery ID.
	// alreadyProcessed covers two cases:
	//   1. Permanent: the ID was successfully ingested before.
	//   2. Transient: a concurrent goroutine is currently ingesting the same ID.
	// In both cases we discard the message here. If the in-flight attempt later
	// fails the ID is cleared from inFlightIDs, so GitHub's redelivery of the
	// same ID will be retried on the next delivery.
	if c.alreadyProcessed(e.DeliveryID) {
		slog.Debug("duplicate github webhook delivery discarded", "delivery_id", e.DeliveryID)
		return
	}

	if c.Ingest == nil {
		return
	}

	// Mark the ID as in-flight so that concurrent redeliveries arriving while
	// Ingest is running are treated as transient duplicates.
	c.beginInFlight(e.DeliveryID)
	err := c.Ingest.Ingest(ctx, e)
	c.clearInFlight(e.DeliveryID)

	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("github webhook message processing cancelled",
				"error", err,
				"delivery_id", e.DeliveryID,
				"event", e.Event,
			)
			return
		}
		// Ingest failed — do NOT record the ID as processed. GitHub's redelivery
		// of the same X-GitHub-Delivery ID will be retried instead of discarded.
		slog.Error("webhook ingest failed", "error", err)
		return
	}

	// Only record the delivery ID as successfully processed after Ingest returns
	// nil. This ensures a failed ingest never permanently blocks redelivery.
	c.recordProcessed(e.DeliveryID)
	if c.LivenessTracker != nil {
		c.LivenessTracker.Tick()
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
	// Pass config.JetStreamAckWait; zero falls back to natsbus.DefaultAckWait.
	AckWait time.Duration
}

// GitHubWebhookJetStreamConsumer is a durable JetStream-backed consumer.
// It acks messages only after successful ingest and naks on failure,
// allowing the server to redeliver up to MaxDeliver times.
type GitHubWebhookJetStreamConsumer struct {
	Sub    *nats.Subscription
	Ingest GitHubWebhookIngestor

	// ShutdownGracePeriod bounds how long shutdown waits for in-flight message
	// handlers before cancelling their contexts. Defaults to
	// defaultShutdownGracePeriod.
	ShutdownGracePeriod time.Duration

	wg sync.WaitGroup

	processingMu     sync.RWMutex
	processingCtx    context.Context
	cancelProcessing context.CancelFunc
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
		ackWait = natsbus.DefaultAckWait
	}

	processingCtx := c.initProcessingContext(ctx)

	sub, err := js.QueueSubscribe(
		events.SubjectGitHubWebhookReceived,
		consumerName,
		func(msg *nats.Msg) {
			c.wg.Add(1)
			go func() {
				defer c.wg.Done()
				c.handleJetStreamMessage(processingCtx, msg)
			}()
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
		if sub != nil {
			_ = sub.Unsubscribe()
		}

		graceCtx, cancelGrace := context.WithTimeout(context.Background(), c.shutdownGracePeriod())
		defer cancelGrace()

		if err := shutdownwait.Wait(graceCtx, &c.wg); err != nil {
			slog.Warn("github webhook jetstream consumer shutdown grace period exceeded; cancelling in-flight messages", "error", err)
			c.processingMu.RLock()
			cancelProcessing := c.cancelProcessing
			c.processingMu.RUnlock()
			if cancelProcessing != nil {
				cancelProcessing()
			}
			return
		}

		slog.Info("github webhook jetstream consumer drained in-flight messages")
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
	if err := e.Validate(); err != nil {
		slog.Error("invalid github webhook event; acking to avoid infinite redelivery",
			"error", err,
			"subject", msg.Subject,
		)
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

func (c *GitHubWebhookJetStreamConsumer) initProcessingContext(ctx context.Context) context.Context {
	c.processingMu.Lock()
	defer c.processingMu.Unlock()

	if c.cancelProcessing != nil {
		c.cancelProcessing()
	}
	processingCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.processingCtx = processingCtx
	c.cancelProcessing = cancel
	return processingCtx
}

func (c *GitHubWebhookJetStreamConsumer) shutdownGracePeriod() time.Duration {
	if c.ShutdownGracePeriod > 0 {
		return c.ShutdownGracePeriod
	}
	return defaultShutdownGracePeriod
}
