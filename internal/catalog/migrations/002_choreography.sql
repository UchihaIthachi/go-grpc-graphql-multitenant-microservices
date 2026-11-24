-- Choreographed saga support: catalog reacts to events and publishes its own.

-- Reservations made from an order.placed event. Catalog only reacts to payment events for
-- these; orchestrated orders get their commit/release calls from the order service instead.
ALTER TABLE catalog.reservations ADD COLUMN via_event boolean NOT NULL DEFAULT false;

CREATE TABLE catalog.outbox (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id     uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    tenant_id    uuid NOT NULL,
    topic        text NOT NULL,
    key          text NOT NULL,
    type         text NOT NULL,
    payload      jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX outbox_unpublished_idx ON catalog.outbox (id) WHERE published_at IS NULL;

-- Inbox: which events a consumer already handled, written in the same transaction as the effect.
CREATE TABLE catalog.inbox (consumer text, event_id uuid, processed_at timestamptz DEFAULT now(), PRIMARY KEY (consumer, event_id));

GRANT UPDATE (via_event) ON catalog.reservations TO {{APP_ROLE}};
GRANT SELECT, INSERT, UPDATE (published_at) ON catalog.outbox TO {{APP_ROLE}};
GRANT SELECT, INSERT ON catalog.inbox TO {{APP_ROLE}};
