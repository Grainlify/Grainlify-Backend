package membus_test

import (
	"context"
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/bus/membus"
)

func TestPublishAndMessages(t *testing.T) {
	b := membus.New()
	ctx := context.Background()

	if err := b.Publish(ctx, "foo", []byte("hello")); err != nil {
		t.Fatalf("Publish failed: %v", err)
	}
	if err := b.Publish(ctx, "bar", []byte("world")); err != nil {
		t.Fatalf("Publish failed: %v", err)
	}

	msgs := b.Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Subject != "foo" || string(msgs[0].Data) != "hello" {
		t.Errorf("unexpected first message: %+v", msgs[0])
	}
	if msgs[1].Subject != "bar" || string(msgs[1].Data) != "world" {
		t.Errorf("unexpected second message: %+v", msgs[1])
	}
}

func TestStatus(t *testing.T) {
	b := membus.New()
	if b.Status() != "OK" {
		t.Errorf("expected OK, got %q", b.Status())
	}
}

func TestClose(t *testing.T) {
	b := membus.New()
	b.Close()

	if b.Status() != "CLOSED" {
		t.Errorf("expected CLOSED after Close(), got %q", b.Status())
	}

	err := b.Publish(context.Background(), "x", []byte("after close"))
	if err == nil {
		t.Error("expected error publishing to closed bus")
	}
}

func TestReset(t *testing.T) {
	b := membus.New()
	_ = b.Publish(context.Background(), "s", []byte("data"))
	b.Reset()
	if len(b.Messages()) != 0 {
		t.Errorf("expected 0 messages after Reset, got %d", len(b.Messages()))
	}
}

func TestPublishCopiesData(t *testing.T) {
	b := membus.New()
	data := []byte("mutable")
	_ = b.Publish(context.Background(), "s", data)
	data[0] = 'X'
	msg := b.Messages()[0]
	if msg.Data[0] == 'X' {
		t.Error("Publish must copy data — mutation of the original slice must not affect the stored message")
	}
}

func TestNATSBusPublishClose(t *testing.T) {
	// Cover the natsbus.Bus interface contract via the membus shim.
	// A real NATS connection is not available in CI, so we verify
	// the membus satisfies the same interface.
	var _ interface {
		Publish(ctx context.Context, subject string, data []byte) error
		Status() string
		Close()
	} = membus.New()
}
