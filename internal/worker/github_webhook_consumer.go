package worker

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/nats-io/nats.go"

	"github.com/jagadeesh/grainlify/backend/internal/events"
)

type GitHubWebhookIngestor interface {
	Ingest(context.Context, events.GitHubWebhookReceived) error
}

type GitHubWebhookConsumer struct {
	Sub    *nats.Subscription
	Ingest GitHubWebhookIngestor
}

// GitHubWebhookQueueGroup is the shared NATS queue group for webhook workers.
const GitHubWebhookQueueGroup = "grainlify-workers"

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

func (c *GitHubWebhookConsumer) handleMessage(ctx context.Context, msg *nats.Msg) {
	var e events.GitHubWebhookReceived
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		slog.Error("bad github webhook event", "error", err)
		return
	}
	if c.Ingest == nil {
		return
	}
	if err := c.Ingest.Ingest(ctx, e); err != nil {
		slog.Error("webhook ingest failed", "error", err)
	}
}
