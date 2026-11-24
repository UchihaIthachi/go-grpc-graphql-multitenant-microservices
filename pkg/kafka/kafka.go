// Package kafka publishes outbox messages to Kafka (Redpanda in compose) with franz-go.
// The producer is idempotent and waits for all in-sync replicas, so a message the relay marks
// published is durably in the log; a crash between produce and mark re-publishes it, which is
// why every message carries an event-id header and consumers dedupe on it (outbox.FirstDelivery).
package kafka

import (
	"context"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/bus"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/outbox"
)

// Publisher implements outbox.Publisher.
type Publisher struct{ Client *kgo.Client }

// New connects to brokers.
func New(brokers []string, clientID string) (*Publisher, error) {
	c, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ClientID(clientID),
		kgo.RequiredAcks(kgo.AllISRAcks()), kgo.ProducerLinger(5*time.Millisecond), kgo.AllowAutoTopicCreation())
	if err != nil {
		return nil, err
	}
	return &Publisher{Client: c}, nil
}

// Publish sends a batch and returns the first failure, if any. Messages with the same key
// (the order or payment id) go to the same partition, so their events stay in order.
func (p *Publisher) Publish(ctx context.Context, msgs []outbox.Message) error {
	recs := make([]*kgo.Record, 0, len(msgs))
	for _, m := range msgs {
		r := &kgo.Record{Topic: m.Topic, Key: []byte(m.Key), Value: m.Value}
		for k, v := range m.Headers {
			r.Headers = append(r.Headers, kgo.RecordHeader{Key: k, Value: []byte(v)})
		}
		recs = append(recs, r)
	}
	return p.Client.ProduceSync(ctx, recs...).FirstErr()
}

// Close flushes and closes the client.
func (p *Publisher) Close() { p.Client.Close() }

// Subscriber implements bus.Subscriber with a Kafka consumer group. Offsets are committed only
// after the handler returns, so a crash redelivers: at-least-once, which is why consumers
// dedupe (inbox) or are idempotent by design.
type Subscriber struct {
	Brokers  []string
	ClientID string
}

// Subscribe consumes until ctx ends.
func (s Subscriber) Subscribe(ctx context.Context, group string, topics []string, h bus.Handler) error {
	c, err := kgo.NewClient(kgo.SeedBrokers(s.Brokers...), kgo.ClientID(s.ClientID), kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topics...), kgo.DisableAutoCommit(), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.AllowAutoTopicCreation())
	if err != nil {
		return err
	}
	defer c.Close()
	for {
		fetches := c.PollFetches(ctx)
		if ctx.Err() != nil {
			return nil
		}
		var handleErr error
		var done []*kgo.Record
		fetches.EachRecord(func(r *kgo.Record) {
			if handleErr != nil {
				return
			}
			m := outbox.Message{Topic: r.Topic, Key: string(r.Key), Value: r.Value, Headers: map[string]string{}}
			for _, hd := range r.Headers {
				m.Headers[hd.Key] = string(hd.Value)
			}
			m.EventID = m.Headers["event-id"]
			if err := h(ctx, m); err != nil {
				handleErr = err
				return
			}
			done = append(done, r)
		})
		if len(done) > 0 {
			if err := c.CommitRecords(ctx, done...); err != nil && ctx.Err() == nil {
				time.Sleep(time.Second) // not committed: these may be redelivered, consumers cope
			}
		}
		if handleErr != nil && ctx.Err() == nil {
			return handleErr // restart the client: uncommitted records are redelivered
		}
	}
}
