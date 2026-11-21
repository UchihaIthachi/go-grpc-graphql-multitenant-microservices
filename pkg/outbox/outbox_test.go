package outbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/testpg"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/outbox"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

var migrations = fstest.MapFS{"m/001.sql": {Data: []byte(`
CREATE TABLE t.outbox (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    tenant_id uuid NOT NULL, topic text NOT NULL, key text NOT NULL, type text NOT NULL,
    payload jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), published_at timestamptz);
CREATE TABLE t.inbox (consumer text, event_id uuid, processed_at timestamptz DEFAULT now(), PRIMARY KEY (consumer, event_id));
CREATE TABLE t.things (id int PRIMARY KEY);
GRANT USAGE ON SCHEMA t TO {{APP_ROLE}};
GRANT SELECT, INSERT, UPDATE (published_at) ON t.outbox TO {{APP_ROLE}};
GRANT SELECT, INSERT ON t.inbox, t.things TO {{APP_ROLE}};
`)}}

const tenant = tenancy.ID("11111111-1111-4111-8111-111111111111")

type fakePub struct {
	mu   sync.Mutex
	got  []outbox.Message
	fail bool
}

func (p *fakePub) Publish(_ context.Context, ms []outbox.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return errors.New("broker down")
	}
	p.got = append(p.got, ms...)
	return nil
}

func TestEventCommitsWithStateOrNotAtAll(t *testing.T) {
	pg := testpg.New(t, migrations, "m", "t")
	ctx := tenancy.With(context.Background(), tenant)

	// committed: state + event
	_ = db.InTenantTx(ctx, pg.App, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO t.things VALUES (1)`); err != nil {
			return err
		}
		return outbox.Enqueue(ctx, tx, "t.outbox", outbox.Event{Topic: "x", Key: "1", Type: "thing.created", Payload: map[string]int{"id": 1}})
	})
	// rolled back: neither
	_ = db.InTenantTx(ctx, pg.App, func(tx pgx.Tx) error {
		_ = outbox.Enqueue(ctx, tx, "t.outbox", outbox.Event{Topic: "x", Key: "2", Type: "thing.created", Payload: 2})
		return errors.New("business rule failed")
	})

	pub := &fakePub{}
	r := outbox.Relay{DB: pg.App, Table: "t.outbox", Pub: pub}
	if n, err := r.Once(ctx); err != nil || n != 1 {
		t.Fatalf("relay: n=%d err=%v (want exactly the committed event)", n, err)
	}
	m := pub.got[0]
	if m.Headers["event-type"] != "thing.created" || m.Headers["tenant-id"] != string(tenant) || string(m.Value) != `{"id": 1}` {
		t.Fatalf("message: %+v %s", m.Headers, m.Value)
	}
	if n, _ := r.Once(ctx); n != 0 {
		t.Fatal("published twice")
	}
}

func TestBrokerDownKeepsEventsForLater(t *testing.T) {
	pg := testpg.New(t, migrations, "m", "t")
	ctx := tenancy.With(context.Background(), tenant)
	for i := 0; i < 3; i++ {
		_ = db.InTenantTx(ctx, pg.App, func(tx pgx.Tx) error {
			return outbox.Enqueue(ctx, tx, "t.outbox", outbox.Event{Topic: "x", Key: "k", Type: "e", Payload: i})
		})
	}
	pub := &fakePub{fail: true}
	r := outbox.Relay{DB: pg.App, Table: "t.outbox", Pub: pub}
	if _, err := r.Once(ctx); err == nil {
		t.Fatal("expected publish error")
	}
	pub.fail = false
	if n, err := r.Once(ctx); err != nil || n != 3 {
		t.Fatalf("after recovery: n=%d err=%v", n, err)
	}
}

func TestParallelRelaysNeverDoublePublish(t *testing.T) {
	pg := testpg.New(t, migrations, "m", "t")
	ctx := tenancy.With(context.Background(), tenant)
	for i := 0; i < 200; i++ {
		_ = db.InTenantTx(ctx, pg.App, func(tx pgx.Tx) error {
			return outbox.Enqueue(ctx, tx, "t.outbox", outbox.Event{Topic: "x", Key: "k", Type: "e", Payload: i})
		})
	}
	pub := &fakePub{}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := outbox.Relay{DB: pg.App, Table: "t.outbox", Pub: pub, Batch: 7}
			for {
				n, err := r.Once(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	seen := map[string]int{}
	for _, m := range pub.got {
		seen[m.EventID]++
	}
	if len(pub.got) != 200 || len(seen) != 200 {
		t.Fatalf("4 relays published %d messages, %d distinct (want 200 / 200)", len(pub.got), len(seen))
	}
}

func TestInboxSkipsRedelivery(t *testing.T) {
	pg := testpg.New(t, migrations, "m", "t")
	ctx := tenancy.With(context.Background(), tenant)
	id := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	handled := 0
	for i := 0; i < 3; i++ {
		_ = db.InTenantTx(ctx, pg.App, func(tx pgx.Tx) error {
			first, err := outbox.FirstDelivery(ctx, tx, "t.inbox", "mailer", id)
			if err != nil || !first {
				return err
			}
			handled++
			return nil
		})
	}
	if handled != 1 {
		t.Fatalf("handled %d times", handled)
	}
}
