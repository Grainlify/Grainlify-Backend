package bus_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/jagadeesh/grainlify/backend/internal/bus/natsbus"
)

func testNATSURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("NATS_TEST_URL")
	if url == "" {
		t.Skip("set NATS_TEST_URL to run NATS delivery-guarantee integration tests")
	}
	return url
}

func TestNATSBusPublishSubscribeDeliversToActiveSubscriber(t *testing.T) {
	url := testNATSURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	publisher, err := natsbus.Connect(url)
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	defer publisher.Close()

	subscriber, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect subscriber: %v", err)
	}
	defer subscriber.Close()

	subject := nats.NewInbox()
	received := make(chan []byte, 1)
	sub, err := subscriber.Subscribe(subject, func(msg *nats.Msg) {
		received <- msg.Data
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}

	want := []byte("hello active subscriber")
	if err := publisher.Publish(ctx, subject, want); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case got := <-received:
		if string(got) != string(want) {
			t.Fatalf("message = %q, want %q", got, want)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for published message")
	}
}

func TestNATSBusDoesNotRedeliverMessagesPublishedWhileConsumerDisconnected(t *testing.T) {
	url := testNATSURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	publisher, err := natsbus.Connect(url)
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	defer publisher.Close()

	subject := nats.NewInbox()

	firstSubscriber, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect first subscriber: %v", err)
	}
	firstSub, err := firstSubscriber.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("first subscribe: %v", err)
	}
	if err := firstSubscriber.Flush(); err != nil {
		t.Fatalf("flush first subscription: %v", err)
	}
	if err := firstSub.Unsubscribe(); err != nil {
		t.Fatalf("unsubscribe first subscriber: %v", err)
	}
	firstSubscriber.Close()

	lost := []byte("published while no consumer is connected")
	if err := publisher.Publish(ctx, subject, lost); err != nil {
		t.Fatalf("publish while disconnected: %v", err)
	}

	reconnected, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect reconnected subscriber: %v", err)
	}
	defer reconnected.Close()
	reconnectedSub, err := reconnected.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("reconnected subscribe: %v", err)
	}
	defer reconnectedSub.Unsubscribe()
	if err := reconnected.Flush(); err != nil {
		t.Fatalf("flush reconnected subscription: %v", err)
	}

	if msg, err := reconnectedSub.NextMsg(200 * time.Millisecond); err == nil {
		t.Fatalf("unexpected redelivery after reconnect: %q", msg.Data)
	} else if err != nats.ErrTimeout {
		t.Fatalf("waiting for absent redelivery: %v", err)
	}

	want := []byte("published after reconnect")
	if err := publisher.Publish(ctx, subject, want); err != nil {
		t.Fatalf("publish after reconnect: %v", err)
	}
	msg, err := reconnectedSub.NextMsg(time.Until(time.Now().Add(5 * time.Second)))
	if err != nil {
		t.Fatalf("receive after reconnect: %v", err)
	}
	if string(msg.Data) != string(want) {
		t.Fatalf("message after reconnect = %q, want %q", msg.Data, want)
	}
}
