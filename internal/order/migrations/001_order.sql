-- Orders and their saga state. The order row IS the saga: status is the current step, and
-- attempts / next_attempt_at / lease_until drive retries and recovery.

CREATE TABLE orders.orders (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL,
    request_id      text        NOT NULL,             -- client idempotency key for placeOrder
    customer_id     text        NOT NULL,
    payment_method  text        NOT NULL,
    status          text        NOT NULL DEFAULT 'PENDING' CHECK (status IN (
                        'PENDING', 'STOCK_RESERVED', 'PAYMENT_AUTHORIZED', 'PAYMENT_CAPTURED', 'CONFIRMED',
                        'VOIDING_PAYMENT', 'REFUNDING_PAYMENT', 'RELEASING_STOCK', 'REJECTED')),
    reason          text        NOT NULL DEFAULT '',
    total_minor     bigint      NOT NULL CHECK (total_minor > 0),
    currency        char(3)     NOT NULL,
    attempts        integer     NOT NULL DEFAULT 0,  -- failed tries of the current step
    last_error      text        NOT NULL DEFAULT '',
    next_attempt_at timestamptz,                     -- NULL once terminal
    lease_until     timestamptz,                     -- a runner is working on it until then
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, request_id),
    UNIQUE (id, tenant_id)
);
CREATE INDEX orders_due_idx ON orders.orders (next_attempt_at) WHERE next_attempt_at IS NOT NULL;
CREATE INDEX orders_customer_idx ON orders.orders (tenant_id, customer_id, created_at DESC);

CREATE TABLE orders.lines (
    order_id         uuid    NOT NULL,
    tenant_id        uuid    NOT NULL,
    product_id       uuid    NOT NULL,
    sku              text    NOT NULL,
    name             text    NOT NULL,
    qty              integer NOT NULL CHECK (qty > 0),
    unit_price_minor bigint  NOT NULL CHECK (unit_price_minor > 0),
    PRIMARY KEY (order_id, product_id),
    FOREIGN KEY (order_id, tenant_id) REFERENCES orders.orders (id, tenant_id)
);

-- Every transition, for support and debugging ("why was my order rejected?").
CREATE TABLE orders.history (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id   uuid        NOT NULL,
    tenant_id  uuid        NOT NULL,
    from_status text       NOT NULL,
    to_status  text        NOT NULL,
    note       text        NOT NULL DEFAULT '',
    at         timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (order_id, tenant_id) REFERENCES orders.orders (id, tenant_id)
);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['orders', 'lines', 'history'] LOOP
        EXECUTE format('ALTER TABLE orders.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE orders.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON orders.%I
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;

CREATE TABLE orders.outbox (
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
CREATE INDEX outbox_unpublished_idx ON orders.outbox (id) WHERE published_at IS NULL;
CREATE TABLE orders.inbox (consumer text, event_id uuid, processed_at timestamptz DEFAULT now(), PRIMARY KEY (consumer, event_id));

-- The recovery loop finds sagas due for another try in ALL tenants. Like the catalog's expiry
-- sweeper, it goes through one SECURITY DEFINER function instead of bypassing RLS itself; the
-- runner then works on each order inside that order's own tenant transaction.
CREATE FUNCTION orders.due_sagas(max_rows integer) RETURNS TABLE (tenant_id uuid, order_id uuid)
LANGUAGE sql SECURITY DEFINER SET search_path = orders, pg_temp AS $$
    SELECT o.tenant_id, o.id FROM orders.orders o
     WHERE o.next_attempt_at <= now() AND (o.lease_until IS NULL OR o.lease_until < now())
     ORDER BY o.next_attempt_at LIMIT max_rows
$$;

GRANT USAGE ON SCHEMA orders TO {{APP_ROLE}};
GRANT SELECT, INSERT, UPDATE ON orders.orders TO {{APP_ROLE}};
GRANT SELECT, INSERT ON orders.lines, orders.history, orders.inbox TO {{APP_ROLE}};
GRANT SELECT, INSERT, UPDATE (published_at) ON orders.outbox TO {{APP_ROLE}};
REVOKE ALL ON FUNCTION orders.due_sagas(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION orders.due_sagas(integer) TO {{APP_ROLE}};
