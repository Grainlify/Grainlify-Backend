// Package membus provides an in-memory Bus implementation for tests.
// It implements the bus.Bus interface without requiring a running NATS server.
package membus

import (
	"context"
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
	mu       sync.Mutex
	messages []Message
	closed   bool
}

// New returns a new, open in-memory Bus.
func New() *Bus {
	return &Bus{}
}

// Publish records a message. Returns an error if the bus is closed.
func (b *Bus) Publish(_ context.Context, subject string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errClosed
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	b.messages = append(b.messages, Message{Subject: subject, Data: cp})
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
	defer b.mu.Unlock()
	b.closed = true
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

var errClosed = closedError{}

type closedError struct{}

func (closedError) Error() string { return "bus: publish on closed bus" }
