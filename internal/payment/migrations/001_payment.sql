-- Payment intents: one per order, moving through
--   pending → authorized → captured → refund_pending → refunded
--   pending → failed (declined) · authorized → voided · refund_pending → captured (refund failed)
-- Transitions are conditional UPDATEs (WHERE status = <from>), so two racing callers can't
-- both apply the same transition.

CREATE TABLE payment.intents (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid        NOT NULL,
    order_id       text        NOT NULL,
    customer_id    text        NOT NULL,
    amount_minor   bigint      NOT NULL CHECK (amount_minor > 0),
    currency       char(3)     NOT NULL,
    payment_method text        NOT NULL,
    status         text        NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'authorized', 'captured', 'voided', 'refund_pending', 'refunded', 'failed')),
    psp_charge_id  text,
    psp_refund_id  text,
    fee_minor      bigint      NOT NULL DEFAULT 0,
    refund_attempts int        NOT NULL DEFAULT 0,      -- part of the refund idempotency key
    failure_reason text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, order_id)
);
CREATE INDEX intents_charge_idx ON payment.intents (psp_charge_id);

-- Every webhook event id is processed once per tenant; redeliveries hit the primary key.
CREATE TABLE payment.webhook_events (
    tenant_id   uuid        NOT NULL,
    event_id    text        NOT NULL,
    type        text        NOT NULL,
    payload     jsonb       NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, event_id)
);

-- Reconciliation: one run per tenant per day, with every difference found.
CREATE TABLE payment.recon_runs (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid        NOT NULL,
    day        date        NOT NULL,
    matched    int         NOT NULL,
    breaks     int         NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE payment.recon_breaks (
    run_id     uuid   NOT NULL REFERENCES payment.recon_runs (id),
    tenant_id  uuid   NOT NULL,
    kind       text   NOT NULL CHECK (kind IN ('missing_in_ledger', 'missing_at_psp', 'amount_mismatch')),
    charge_id  text   NOT NULL,
    line_type  text   NOT NULL,
    ledger_minor bigint,
    psp_minor    bigint
);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['intents', 'webhook_events', 'recon_runs', 'recon_breaks'] LOOP
        EXECUTE format('ALTER TABLE payment.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE payment.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON payment.%I
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;

-- Outbox / inbox (see pkg/outbox; no RLS: only the relay reads the outbox).
CREATE TABLE payment.outbox (
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
CREATE INDEX outbox_unpublished_idx ON payment.outbox (id) WHERE published_at IS NULL;

GRANT USAGE ON SCHEMA payment TO {{APP_ROLE}};
GRANT SELECT, INSERT, UPDATE ON payment.intents TO {{APP_ROLE}};
GRANT SELECT, INSERT ON payment.webhook_events, payment.recon_runs, payment.recon_breaks TO {{APP_ROLE}};
GRANT SELECT, INSERT, UPDATE (published_at) ON payment.outbox TO {{APP_ROLE}};
