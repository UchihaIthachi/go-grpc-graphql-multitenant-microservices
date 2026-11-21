// Package outbox implements the transactional outbox and the idempotent inbox.
//
// Outbox: a service writes the event row in the SAME transaction as the state change it
// describes, so "state changed but the event was lost" (or the reverse) can't happen. A relay
// then publishes unpublished rows and marks them published. Delivery is at-least-once: a crash
// between publish and mark republishes the batch, so consumers must be idempotent.
//
// Inbox: a consumer records (consumer, event_id) in the same transaction as its handling;
// a redelivered event finds the row and is skipped.
//
// Each service's migration creates its own <schema>.outbox and <schema>.inbox tables, e.g.:
//
//	CREATE TABLE payment.outbox (
//	    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
//	    event_id     uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
//	    tenant_id    uuid NOT NULL,
//	    topic        text NOT NULL,
//	    key          text NOT NULL,
//	    type         text NOT NULL,
//	    payload      jsonb NOT NULL,
//	    created_at   timestamptz NOT NULL DEFAULT now(),
//	    published_at timestamptz);
//	CREATE TABLE payment.inbox (consumer text, event_id uuid, processed_at timestamptz DEFAULT now(),
//	    PRIMARY KEY (consumer, event_id));
//
// The outbox has no row-level security on purpose: the relay publishes for all tenants, and no
// request path ever reads it. Rows carry tenant_id, which travels as a message header.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

// Event is something that happened, to be published after the transaction commits.
type Event struct {
	Topic   string // e.g. "payment.events"
	Key     string // partition key: events with the same key stay in order (e.g. the order id)
	Type    string // e.g. "payment.captured"
	Payload any    // JSON-encoded
}

// Enqueue writes ev into table inside tx. The tenant comes from ctx.
func Enqueue(ctx context.Context, tx pgx.Tx, table string, ev Event) error {
	tid, err := tenancy.From(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(ev.Payload)
	if err != nil {
		return fmt.Errorf("outbox: encode %s: %w", ev.Type, err)
	}
	_, err = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, topic, key, type, payload) VALUES ($1, $2, $3, $4, $5)`, table),
		string(tid), ev.Topic, ev.Key, ev.Type, body)
	return err
}

// Message is an outbox row ready to publish.
type Message struct {
	EventID string
	Topic   string
	Key     string
	Value   []byte
	Headers map[string]string // event-id, event-type, tenant-id
}

// Publisher delivers a batch; it returns an error unless every message was accepted.
type Publisher interface {
	Publish(ctx context.Context, msgs []Message) error
}

// Relay moves committed outbox rows to the Publisher.
type Relay struct {
	DB       *pgxpool.Pool
	Table    string
	Pub      Publisher
	Batch    int           // default 100
	Interval time.Duration // default 500 ms
	Log      *slog.Logger
}

// Run publishes until ctx is done.
func (r Relay) Run(ctx context.Context) {
	iv := r.Interval
	if iv == 0 {
		iv = 500 * time.Millisecond
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		for {
			n, err := r.Once(ctx)
			if err != nil && ctx.Err() == nil && r.Log != nil {
				r.Log.Warn("outbox relay", "table", r.Table, "err", err)
			}
			if err != nil || n == 0 {
				break // wait for the next tick; a full batch loops straight away
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once publishes up to one batch and returns how many rows it published.
// Rows are locked with SKIP LOCKED, so several relay replicas share the work without
// publishing the same row twice concurrently.
func (r Relay) Once(ctx context.Context) (int, error) {
	batch := r.Batch
	if batch == 0 {
		batch = 100
	}
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT id, event_id, tenant_id, topic, key, type, payload
		FROM %s WHERE published_at IS NULL ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, r.Table), batch)
	if err != nil {
		return 0, err
	}
	var ids []int64
	var msgs []Message
	for rows.Next() {
		var id int64
		var m Message
		var tenant, typ string
		if err := rows.Scan(&id, &m.EventID, &tenant, &m.Topic, &m.Key, &typ, &m.Value); err != nil {
			rows.Close()
			return 0, err
		}
		m.Headers = map[string]string{"event-id": m.EventID, "event-type": typ, "tenant-id": tenant}
		ids = append(ids, id)
		msgs = append(msgs, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(msgs) == 0 {
		return 0, nil
	}
	if err := r.Pub.Publish(ctx, msgs); err != nil {
		return 0, fmt.Errorf("outbox: publish %d: %w", len(msgs), err) // rollback: retried next tick
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET published_at = now() WHERE id = ANY($1)`, r.Table), ids); err != nil {
		return 0, err
	}
	return len(msgs), tx.Commit(ctx)
}

// FirstDelivery records that consumer handled eventID, inside the handler's transaction.
// It returns false if the event was already handled (a redelivery): skip it.
func FirstDelivery(ctx context.Context, tx pgx.Tx, table, consumer, eventID string) (bool, error) {
	tag, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (consumer, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, table), consumer, eventID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
