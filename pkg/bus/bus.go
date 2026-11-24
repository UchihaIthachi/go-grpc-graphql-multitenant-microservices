// Package bus is how services react to each other's events: a Subscriber delivers messages
// from topics to a Handler, at least once. Kafka (pkg/kafka) is the real one; Memory is an
// in-process stand-in for tests that behaves the same way (including redeliveries).
//
// Consume wraps a handler with what every consumer needs: the tenant and order from the
// message headers, retries with backoff, a dead-letter topic for messages that keep failing,
// and Pattern Lab events.
package bus

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/lab"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/outbox"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

// Message is what the outbox relay publishes.
type Message = outbox.Message

// Handler processes one message. Returning an error means "try again later".
type Handler func(ctx context.Context, m Message) error

// Subscriber delivers messages of topics to h, as consumer group `group`, until ctx ends.
type Subscriber interface {
	Subscribe(ctx context.Context, group string, topics []string, h Handler) error
}

// Type is the event type header.
func Type(m Message) string { return m.Headers["event-type"] }

// Decode unmarshals the payload.
func Decode(m Message, v any) error { return json.Unmarshal(m.Value, v) }

// Options for Consume.
type Options struct {
	Service  string           // node name for the lab
	Attempts int              // tries before dead-lettering (default 5)
	Backoff  time.Duration    // first retry delay, doubled each time (default 100 ms)
	DLQ      outbox.Publisher // where dead letters go (topic + ".dlq"); nil: logged and dropped
	Log      *slog.Logger
}

// Consume runs h for every message of topics, with retries and a dead-letter topic.
func Consume(ctx context.Context, sub Subscriber, group string, topics []string, h Handler, o Options) error {
	if o.Attempts == 0 {
		o.Attempts = 5
	}
	if o.Backoff == 0 {
		o.Backoff = 100 * time.Millisecond
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return sub.Subscribe(ctx, group, topics, func(ctx context.Context, m Message) error {
		if t, err := tenancy.Parse(m.Headers["tenant-id"]); err == nil {
			ctx = tenancy.With(ctx, t)
		}
		ctx = lab.WithOrder(ctx, m.Key)
		start := time.Now()
		var err error
		for attempt, wait := 1, o.Backoff; attempt <= o.Attempts; attempt, wait = attempt+1, wait*2 {
			if err = h(ctx, m); err == nil {
				lab.Emit(ctx, lab.Event{Kind: lab.KindConsume, From: lab.Kafka, To: o.Service, Label: Type(m), OK: true, Ms: lab.Since(start)})
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lab.Emit(ctx, lab.Event{Kind: lab.KindRetry, From: lab.Kafka, To: o.Service, Label: Type(m), Detail: err.Error()})
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		// Poison message: park it so the partition isn't blocked forever. Someone has to look.
		o.Log.Error("dead-lettering message", "group", group, "type", Type(m), "event", m.EventID, "err", err)
		lab.Emit(ctx, lab.Event{Kind: lab.KindDeadLetter, From: lab.Kafka, To: o.Service, Label: Type(m), Detail: err.Error()})
		if o.DLQ != nil {
			dl := m
			dl.Topic = m.Topic + ".dlq"
			dl.Headers = map[string]string{}
			for k, v := range m.Headers {
				dl.Headers[k] = v
			}
			dl.Headers["dlq-group"], dl.Headers["dlq-error"] = group, err.Error()
			if perr := o.DLQ.Publish(ctx, []Message{dl}); perr != nil {
				return perr // couldn't park it: don't commit, it will be redelivered
			}
		}
		return nil
	})
}

// Memory is an in-process bus. Each consumer group gets every message of its topics, in
// publish order, at least once. Duplicate re-delivers every message a second time, which is
// what consumers must survive in production too.
type Memory struct {
	Duplicate bool

	mu   sync.Mutex
	subs map[string][]chan Message // topic → group queues
}

// Publish implements outbox.Publisher.
func (b *Memory) Publish(_ context.Context, msgs []Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range msgs {
		for _, q := range b.subs[m.Topic] {
			q <- m
			if b.Duplicate {
				q <- m
			}
		}
	}
	return nil
}

// Subscribe implements Subscriber.
func (b *Memory) Subscribe(ctx context.Context, _ string, topics []string, h Handler) error {
	q := make(chan Message, 100000)
	b.mu.Lock()
	if b.subs == nil {
		b.subs = map[string][]chan Message{}
	}
	for _, t := range topics {
		b.subs[t] = append(b.subs[t], q)
	}
	b.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return nil
		case m := <-q:
			for h(ctx, m) != nil && ctx.Err() == nil { // at least once: keep trying until handled
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
}
