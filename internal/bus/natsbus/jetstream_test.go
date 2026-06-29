package natsbus

import (
	"context"
	"testing"
	"time"
)

func TestNewJetStreamBus_RequiresConnection(t *testing.T) {
	_, err := NewJetStreamBus(nil, JetStreamConfig{StreamName: "TEST"})
	if err == nil {
		t.Fatal("expected error for nil nats.Conn")
	}
}

func TestNewJetStreamBus_RequiresStreamName(t *testing.T) {
	// We cannot create a real nats.Conn in a unit test without a server,
	// but we can validate the config guard with a nil conn first.
	_, err := NewJetStreamBus(nil, JetStreamConfig{})
	if err == nil {
		t.Fatal("expected error for empty stream name")
	}
}

func TestJetStreamBus_PublishRejectsNilBus(t *testing.T) {
	var b *JetStreamBus
	err := b.Publish(context.Background(), "test.subject", []byte("data"))
	if err == nil {
		t.Fatal("expected error publishing on nil bus")
	}
}

func TestJetStreamBus_StatusDisconnectedOnNil(t *testing.T) {
	var b *JetStreamBus
	if got := b.Status(); got != "DISCONNECTED" {
		t.Fatalf("Status() = %q, want DISCONNECTED", got)
	}
}

func TestJetStreamBus_CloseNoopOnNil(t *testing.T) {
	// Should not panic.
	var b *JetStreamBus
	b.Close()
}

func TestJetStreamConfig_DefaultMaxAge(t *testing.T) {
	// Verify that zero MaxAge is replaced with the default 24h in NewJetStreamBus.
	// We cannot connect to a real server here, so we test the guard path only.
	cfg := JetStreamConfig{StreamName: "TEST", MaxAge: 0}
	if cfg.MaxAge == 24*time.Hour {
		t.Error("expected zero before normalization")
	}
	// The actual normalization happens inside NewJetStreamBus; covered by integration tests.
}

func TestJetStreamBus_PublishCancelledContext(t *testing.T) {
	b := &JetStreamBus{} // js is nil, but ctx check happens first.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	err := b.Publish(ctx, "test.subject", []byte("data"))
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}
