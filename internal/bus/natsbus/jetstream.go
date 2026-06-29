// Package natsbus provides NATS-backed implementations of the bus.Bus interface.
// This file contains the JetStream implementation for durable, at-least-once delivery.
package natsbus

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// JetStreamConfig holds configuration for a JetStream-backed bus.
type JetStreamConfig struct {
	// StreamName is the name of the JetStream stream.
	StreamName string
	// Subjects are the NATS subjects this stream captures.
	Subjects []string
	// MaxAge is the maximum age of messages retained in the stream.
	MaxAge time.Duration
}

// JetStreamBus is a bus.Bus implementation backed by NATS JetStream.
// It provides durable, acknowledged publishing with at-least-once delivery guarantees.
type JetStreamBus struct {
	nc  *nats.Conn
	js  nats.JetStreamContext
	cfg JetStreamConfig
}

// NewJetStreamBus creates a new JetStreamBus, ensuring the required stream exists.
// The stream is created or updated to match cfg.
func NewJetStreamBus(nc *nats.Conn, cfg JetStreamConfig) (*JetStreamBus, error) {
	if nc == nil {
		return nil, fmt.Errorf("nats connection is required")
	}
	if cfg.StreamName == "" {
		return nil, fmt.Errorf("stream name is required")
	}

	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream context: %w", err)
	}

	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}

	streamCfg := &nats.StreamConfig{
		Name:     cfg.StreamName,
		Subjects: cfg.Subjects,
		MaxAge:   maxAge,
		Storage:  nats.FileStorage,
		// Discard old messages when limits are reached, preserving newest events.
		Discard: nats.DiscardOld,
	}

	if _, err := js.StreamInfo(cfg.StreamName); err != nil {
		// Stream does not exist; create it.
		if _, err := js.AddStream(streamCfg); err != nil {
			return nil, fmt.Errorf("create jetstream stream %q: %w", cfg.StreamName, err)
		}
		slog.Info("created JetStream stream", "stream", cfg.StreamName, "subjects", cfg.Subjects)
	} else {
		// Stream exists; update to keep config in sync.
		if _, err := js.UpdateStream(streamCfg); err != nil {
			slog.Warn("update JetStream stream config failed (non-fatal)", "stream", cfg.StreamName, "error", err)
		}
	}

	return &JetStreamBus{nc: nc, js: js, cfg: cfg}, nil
}

// Publish publishes data to the given subject using js.Publish, which blocks until
// the server acknowledges persistence. This prevents event loss on publish.
func (b *JetStreamBus) Publish(ctx context.Context, subject string, data []byte) error {
	if b == nil || b.js == nil {
		return fmt.Errorf("jetstream not initialised")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if _, err := b.js.Publish(subject, data, nats.Context(ctx)); err != nil {
		return fmt.Errorf("jetstream publish to %q: %w", subject, err)
	}
	return nil
}

// Status returns the underlying NATS connection status.
func (b *JetStreamBus) Status() string {
	if b == nil || b.nc == nil {
		return "DISCONNECTED"
	}
	return b.nc.Status().String()
}

// Close drains and closes the underlying NATS connection.
func (b *JetStreamBus) Close() {
	if b == nil || b.nc == nil {
		return
	}
	slog.Info("closing JetStream NATS connection")
	_ = b.nc.Drain()
	b.nc.Close()
	slog.Info("JetStream NATS connection closed")
}

// Conn returns the underlying NATS connection (e.g. for consumer setup).
func (b *JetStreamBus) Conn() *nats.Conn { return b.nc }

// JS returns the JetStreamContext for use by consumers.
func (b *JetStreamBus) JS() nats.JetStreamContext { return b.js }
