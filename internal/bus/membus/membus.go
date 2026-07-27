// Package membus provides an in-memory Bus implementation for tests.
// It implements the bus.Bus interface without requiring a running NATS server.
package membus

import (
	"context"
	"errors"
	"sync"
)

// Message is a single published message captured by the Bus.
type Message struct {
	Subject string
	Data    []byte
}

// Bus is an in-memory implementation of bus.Bus.
// Goroutine-safe; safe to use from concurrent goroutines in tests.
type Bus struct {
	mu          sync.Mutex
	messages    []Message
	subscribers map[string]map[*Subscription]struct{}
	closed      bool
}

// Subscription represents an active subscription on an in-memory Bus.
type Subscription struct {
	bus     *Bus
	subject string
	handler func(Message)
	mu      sync.Mutex
	closed  bool
}

// New returns a new, open in-memory Bus.
func New() *Bus {
	return &Bus{
		subscribers: make(map[string]map[*Subscription]struct{}),
	}
}

// Subscribe registers a callback to be invoked when a message is published to subject.
// Returns an error if the bus is closed or if handler is nil.
func (b *Bus) Subscribe(subject string, handler func(Message)) (*Subscription, error) {
	if handler == nil {
		return nil, errNilHandler
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errClosed
	}
	if b.subscribers == nil {
		b.subscribers = make(map[string]map[*Subscription]struct{})
	}
	if b.subscribers[subject] == nil {
		b.subscribers[subject] = make(map[*Subscription]struct{})
	}
	sub := &Subscription{
		bus:     b,
		subject: subject,
		handler: handler,
	}
	b.subscribers[subject][sub] = struct{}{}
	return sub, nil
}

// removeSubscription removes an unsubscribed subscription from the bus.
func (b *Bus) removeSubscription(sub *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subscribers == nil {
		return
	}
	subs, ok := b.subscribers[sub.subject]
	if !ok {
		return
	}
	delete(subs, sub)
	if len(subs) == 0 {
		delete(b.subscribers, sub.subject)
	}
}

// Unsubscribe removes the subscription from the bus.
// Safe to call concurrently and multiple times.
func (s *Subscription) Unsubscribe() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	s.bus.removeSubscription(s)
	return nil
}

// Subject returns the subject this subscription is listening to.
func (s *Subscription) Subject() string {
	return s.subject
}

// Publish records a message. Returns an error if the bus is closed.
func (b *Bus) Publish(_ context.Context, subject string, data []byte) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return errClosed
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	b.messages = append(b.messages, Message{Subject: subject, Data: cp})

	var activeSubs []*Subscription
	if len(b.subscribers[subject]) > 0 {
		activeSubs = make([]*Subscription, 0, len(b.subscribers[subject]))
		for sub := range b.subscribers[subject] {
			activeSubs = append(activeSubs, sub)
		}
	}
	b.mu.Unlock()

	for _, sub := range activeSubs {
		sub.mu.Lock()
		closed := sub.closed
		handler := sub.handler
		sub.mu.Unlock()
		if !closed && handler != nil {
			subData := make([]byte, len(cp))
			copy(subData, cp)
			handler(Message{Subject: subject, Data: subData})
		}
	}

	return nil
}

// Status returns "OK" when open, "CLOSED" after Close is called.
func (b *Bus) Status() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return "CLOSED"
	}
	return "OK"
}

// Close marks the bus as closed. Subsequent Publish calls return an error.
func (b *Bus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	var allSubs []*Subscription
	for _, subs := range b.subscribers {
		for sub := range subs {
			allSubs = append(allSubs, sub)
		}
	}
	b.subscribers = nil
	b.mu.Unlock()

	for _, sub := range allSubs {
		sub.mu.Lock()
		sub.closed = true
		sub.mu.Unlock()
	}
}

// Messages returns a copy of all published messages in order.
func (b *Bus) Messages() []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Message, len(b.messages))
	copy(out, b.messages)
	return out
}

// Reset clears all captured messages (useful between test cases).
func (b *Bus) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.messages = nil
}

var (
	errClosed     = closedError{}
	errNilHandler = errors.New("bus: nil handler")
)

type closedError struct{}

func (closedError) Error() string { return "bus: publish on closed bus" }
