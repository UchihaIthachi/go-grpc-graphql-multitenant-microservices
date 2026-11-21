-- Catalog: products with full-text search, stock levels and per-order reservations.

CREATE TABLE catalog.products (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL,
    sku          text        NOT NULL CHECK (sku <> ''),
    name         text        NOT NULL CHECK (name <> ''),
    description  text        NOT NULL DEFAULT '',
    price_minor  bigint      NOT NULL CHECK (price_minor > 0),
    currency     char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    created_at   timestamptz NOT NULL DEFAULT now(),
    search       tsvector    GENERATED ALWAYS AS (
                     setweight(to_tsvector('simple', name), 'A') || setweight(to_tsvector('simple', description), 'B')) STORED,
    UNIQUE (tenant_id, sku),
    UNIQUE (id, tenant_id)
);
CREATE INDEX products_search_idx ON catalog.products USING gin (search);

-- The invariants that make overselling impossible live here, not in Go:
-- reserved never exceeds on_hand, and neither goes below zero.
CREATE TABLE catalog.stock (
    product_id uuid    PRIMARY KEY,
    tenant_id  uuid    NOT NULL,
    on_hand    integer NOT NULL DEFAULT 0 CHECK (on_hand >= 0),
    reserved   integer NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    CHECK (reserved <= on_hand),
    FOREIGN KEY (product_id, tenant_id) REFERENCES catalog.products (id, tenant_id)
);

CREATE TABLE catalog.reservations (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL,
    order_id    text        NOT NULL,
    product_id  uuid        NOT NULL,
    qty         integer     NOT NULL CHECK (qty > 0),
    status      text        NOT NULL DEFAULT 'reserved' CHECK (status IN ('reserved', 'committed', 'released')),
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, order_id, product_id),
    FOREIGN KEY (product_id, tenant_id) REFERENCES catalog.products (id, tenant_id)
);
CREATE INDEX reservations_expiry_idx ON catalog.reservations (expires_at) WHERE status = 'reserved';

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['products', 'stock', 'reservations'] LOOP
        EXECUTE format('ALTER TABLE catalog.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE catalog.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON catalog.%I
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)$p$, t);
    END LOOP;
END $$;

-- Releasing expired reservations is system work across all tenants. Rather than giving the app
-- role a way around RLS, it may call only this one function, which runs as the owner
-- (the owner role is BYPASSRLS; see deploy/compose/initdb.sql) and does exactly one thing.
CREATE FUNCTION catalog.expire_reservations(max_rows integer) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = catalog, pg_temp AS $$
DECLARE n integer;
BEGIN
    WITH expired AS (
        UPDATE catalog.reservations r SET status = 'released'
         WHERE r.id IN (SELECT id FROM catalog.reservations
                         WHERE status = 'reserved' AND expires_at < now()
                         ORDER BY expires_at LIMIT max_rows FOR UPDATE SKIP LOCKED)
        RETURNING r.product_id, r.qty
    ), per_product AS (
        SELECT product_id, sum(qty) AS qty FROM expired GROUP BY product_id
    )
    UPDATE catalog.stock s SET reserved = s.reserved - p.qty FROM per_product p WHERE s.product_id = p.product_id;
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END $$;

GRANT USAGE ON SCHEMA catalog TO {{APP_ROLE}};
GRANT SELECT, INSERT ON catalog.products TO {{APP_ROLE}};
GRANT SELECT, INSERT, UPDATE (on_hand, reserved) ON catalog.stock TO {{APP_ROLE}};
GRANT SELECT, INSERT, UPDATE (status) ON catalog.reservations TO {{APP_ROLE}};
REVOKE ALL ON FUNCTION catalog.expire_reservations(integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION catalog.expire_reservations(integer) TO {{APP_ROLE}};
