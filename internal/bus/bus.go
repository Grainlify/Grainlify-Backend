package bus

import "context"

// Bus publishes events to a subject-based message bus.
//
// The interface deliberately models only publishing; delivery guarantees are
// provided by the concrete implementation and subscription mode. The core NATS
// implementation in internal/bus/natsbus uses plain NATS publish/subscribe: a
// message is delivered at most once to subscribers that are active when the
// message is published, and messages published while a consumer is disconnected
// are not replayed on reconnect. Consumers that require durable at-least-once
// delivery must use the JetStream-backed bus and explicit-ack JetStream
// consumers.
type Bus interface {
	Publish(ctx context.Context, subject string, data []byte) error
	Status() string
	Close()
}
