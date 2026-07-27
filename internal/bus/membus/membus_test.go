package membus_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestConcurrentPublishSubscribe_NoMessageLoss(t *testing.T) {
	b := membus.New()
	ctx := context.Background()

	topics := []string{"topic.alpha", "topic.beta", "topic.gamma", "topic.delta"}

	const (
		numPublishersPerTopic = 10
		msgsPerPublisher      = 50
		subscribersPerTopic   = 5
	)

	type subTracker struct {
		sub      *membus.Subscription
		received atomic.Int64
	}

	var allTrackers []*subTracker

	// Create overlapping subscribers across shared topics
	for _, topic := range topics {
		for i := 0; i < subscribersPerTopic; i++ {
			tracker := &subTracker{}
			sub, err := b.Subscribe(topic, func(m membus.Message) {
				tracker.received.Add(1)
			})
			if err != nil {
				t.Fatalf("Subscribe failed: %v", err)
			}
			tracker.sub = sub
			allTrackers = append(allTrackers, tracker)
		}
	}

	var wg sync.WaitGroup
	// Spin up concurrent publishers
	for _, topic := range topics {
		for p := 0; p < numPublishersPerTopic; p++ {
			wg.Add(1)
			go func(topicName string, publisherID int) {
				defer wg.Done()
				for m := 0; m < msgsPerPublisher; m++ {
					payload := []byte(fmt.Sprintf("pub-%d-msg-%d", publisherID, m))
					if err := b.Publish(ctx, topicName, payload); err != nil {
						t.Errorf("Publish failed: %v", err)
					}
				}
			}(topic, p)
		}
	}

	wg.Wait()

	expectedPerTopic := int64(numPublishersPerTopic * msgsPerPublisher)
	expectedTotal := int(expectedPerTopic) * len(topics)

	msgs := b.Messages()
	if len(msgs) != expectedTotal {
		t.Fatalf("expected %d total messages in bus history, got %d", expectedTotal, len(msgs))
	}

	for i, tr := range allTrackers {
		if got := tr.received.Load(); got != expectedPerTopic {
			t.Errorf("subscriber %d on topic %q received %d messages, expected %d", i, tr.sub.Subject(), got, expectedPerTopic)
		}
	}
}

func TestConcurrentPublishSubscribe_StressAndUnsubscribe(t *testing.T) {
	b := membus.New()
	ctx := context.Background()

	topics := []string{"stress.0", "stress.1", "stress.2"}

	const (
		numPublishers    = 20
		numSubscribers   = 15
		msgsPerPublisher = 100
	)

	var (
		wgPub          sync.WaitGroup
		wgSub          sync.WaitGroup
		totalPublished atomic.Int64
	)

	// Spin up N goroutines publishing concurrently
	for p := 0; p < numPublishers; p++ {
		wgPub.Add(1)
		go func(id int) {
			defer wgPub.Done()
			for i := 0; i < msgsPerPublisher; i++ {
				topic := topics[i%len(topics)]
				err := b.Publish(ctx, topic, []byte(fmt.Sprintf("pub-%d-msg-%d", id, i)))
				if err != nil {
					t.Errorf("Publish failed: %v", err)
				}
				totalPublished.Add(1)
			}
		}(p)
	}

	// Spin up M goroutines subscribing and unsubscribing concurrently on shared topics
	for s := 0; s < numSubscribers; s++ {
		wgSub.Add(1)
		go func(id int) {
			defer wgSub.Done()
			for i := 0; i < 20; i++ {
				topic := topics[i%len(topics)]
				sub, err := b.Subscribe(topic, func(m membus.Message) {
					// Dummy handler checking payload readability
					_ = len(m.Data)
				})
				if err != nil {
					t.Errorf("Subscribe failed: %v", err)
					return
				}
				time.Sleep(1 * time.Millisecond)
				// Assert subscriber removal mid-publish doesn't panic
				_ = sub.Unsubscribe()
				// Idempotent double unsubscribe
				_ = sub.Unsubscribe()
			}
		}(s)
	}

	wgPub.Wait()
	wgSub.Wait()

	got := len(b.Messages())
	want := int(totalPublished.Load())
	if got != want {
		t.Fatalf("expected %d total published messages, got %d", want, got)
	}
}

func TestSubscriberUnsubscribeMidPublish(t *testing.T) {
	b := membus.New()
	ctx := context.Background()

	const subject = "flight.test"
	var (
		sub1      *membus.Subscription
		sub1Count atomic.Int64
		sub2Count atomic.Int64
		unsubOnce sync.Once
	)

	var err error
	sub1, err = b.Subscribe(subject, func(m membus.Message) {
		sub1Count.Add(1)
		// Unsubscribe immediately mid-publish from within the callback
		unsubOnce.Do(func() {
			_ = sub1.Unsubscribe()
		})
	})
	if err != nil {
		t.Fatalf("sub1 Subscribe failed: %v", err)
	}

	_, err = b.Subscribe(subject, func(m membus.Message) {
		sub2Count.Add(1)
	})
	if err != nil {
		t.Fatalf("sub2 Subscribe failed: %v", err)
	}

	const numPublishers = 10
	const msgsPerPublisher = 20
	var wg sync.WaitGroup

	for p := 0; p < numPublishers; p++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < msgsPerPublisher; i++ {
				_ = b.Publish(ctx, subject, []byte("mid-publish-data"))
			}
		}(p)
	}
	wg.Wait()

	totalExpected := int64(numPublishers * msgsPerPublisher)

	if got := sub1Count.Load(); got != 1 {
		t.Errorf("expected sub1 to receive exactly 1 message before unsubscribing mid-publish, got %d", got)
	}
	if got := sub2Count.Load(); got != totalExpected {
		t.Errorf("expected sub2 to receive all %d messages, got %d", totalExpected, got)
	}
	if len(b.Messages()) != int(totalExpected) {
		t.Errorf("expected %d stored messages, got %d", totalExpected, len(b.Messages()))
	}
}

func TestPublishZeroSubscribers(t *testing.T) {
	b := membus.New()
	ctx := context.Background()

	const numPublishers = 20
	const msgsPerPublisher = 50
	var wg sync.WaitGroup

	for p := 0; p < numPublishers; p++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < msgsPerPublisher; i++ {
				_ = b.Publish(ctx, "no.subs", []byte("orphan"))
			}
		}(p)
	}
	wg.Wait()

	expected := numPublishers * msgsPerPublisher
	if len(b.Messages()) != expected {
		t.Fatalf("expected %d messages with zero subscribers, got %d", expected, len(b.Messages()))
	}

	// Add subscriber afterwards and publish more
	var count atomic.Int64
	_, err := b.Subscribe("no.subs", func(m membus.Message) {
		count.Add(1)
	})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	_ = b.Publish(ctx, "no.subs", []byte("after-sub"))
	if count.Load() != 1 {
		t.Errorf("expected count 1 for subscriber added after initial publish, got %d", count.Load())
	}
	if len(b.Messages()) != expected+1 {
		t.Errorf("expected %d total messages, got %d", expected+1, len(b.Messages()))
	}
}

func TestManyTopicsOverlappingSubscribers(t *testing.T) {
	b := membus.New()
	ctx := context.Background()

	topics := []string{"t.0", "t.1", "t.2", "t.3", "t.4"}

	// subA -> t.0, t.1, t.2
	// subB -> t.1, t.2, t.3
	// subC -> t.2, t.3, t.4
	type overlappingSub struct {
		name     string
		topics   []string
		received atomic.Int64
	}

	subs := []*overlappingSub{
		{name: "subA", topics: []string{"t.0", "t.1", "t.2"}},
		{name: "subB", topics: []string{"t.1", "t.2", "t.3"}},
		{name: "subC", topics: []string{"t.2", "t.3", "t.4"}},
	}

	for _, s := range subs {
		s := s
		for _, top := range s.topics {
			_, err := b.Subscribe(top, func(m membus.Message) {
				s.received.Add(1)
			})
			if err != nil {
				t.Fatalf("Subscribe failed: %v", err)
			}
		}
	}

	const msgsPerTopic = 100
	var wg sync.WaitGroup
	for _, top := range topics {
		wg.Add(1)
		go func(topicName string) {
			defer wg.Done()
			for i := 0; i < msgsPerTopic; i++ {
				_ = b.Publish(ctx, topicName, []byte("data"))
			}
		}(top)
	}
	wg.Wait()

	// Each subscriber listens to 3 topics, 100 messages each = 300 expected messages per subscriber
	for _, s := range subs {
		if got := s.received.Load(); got != 300 {
			t.Errorf("subscriber %s received %d messages, expected 300", s.name, got)
		}
	}

	if len(b.Messages()) != 500 {
		t.Errorf("expected 500 total messages across all topics, got %d", len(b.Messages()))
	}
}

func TestSubscriptionEdgeCases(t *testing.T) {
	b := membus.New()
	ctx := context.Background()

	_, err := b.Subscribe("test", nil)
	if err == nil {
		t.Error("expected error when subscribing with nil handler")
	}

	sub, err := b.Subscribe("test", func(m membus.Message) {})
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	if sub.Subject() != "test" {
		t.Errorf("expected subject 'test', got %q", sub.Subject())
	}

	_ = sub.Unsubscribe()
	_ = sub.Unsubscribe() // Idempotent

	// Test zero-value bus and uninitialized map branches
	var zeroBus membus.Bus
	sZero, err := zeroBus.Subscribe("zero", func(m membus.Message) {})
	if err != nil {
		t.Fatalf("zeroBus Subscribe failed: %v", err)
	}
	_ = sZero.Unsubscribe()
	_ = sZero.Unsubscribe()

	// Test closing bus with active subscribers
	activeSub1, _ := b.Subscribe("active-1", func(m membus.Message) {})
	activeSub2, _ := b.Subscribe("active-2", func(m membus.Message) {})
	_ = activeSub1
	_ = activeSub2

	b.Close()
	b.Close() // Idempotent close

	_, err = b.Subscribe("after-close", func(m membus.Message) {})
	if err == nil {
		t.Error("expected error subscribing to closed bus")
	}
	if err.Error() != "bus: publish on closed bus" {
		t.Errorf("unexpected error string: %q", err.Error())
	}

	_ = b.Publish(ctx, "x", []byte("x"))
}
