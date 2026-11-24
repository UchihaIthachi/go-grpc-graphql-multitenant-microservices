-- Each order remembers how its saga runs, chosen when it is placed:
--   orchestration  the order service drives every step (recovery loop retries)
--   choreography   services react to each other's events; the order service only follows along
ALTER TABLE orders.orders ADD COLUMN saga_mode text NOT NULL DEFAULT 'orchestration'
    CHECK (saga_mode IN ('orchestration', 'choreography'));

-- The recovery loop only drives orchestrated sagas.
CREATE OR REPLACE FUNCTION orders.due_sagas(max_rows integer) RETURNS TABLE (tenant_id uuid, order_id uuid)
LANGUAGE sql SECURITY DEFINER SET search_path = orders, pg_temp AS $$
    SELECT o.tenant_id, o.id FROM orders.orders o
     WHERE o.saga_mode = 'orchestration' AND o.next_attempt_at <= now()
       AND (o.lease_until IS NULL OR o.lease_until < now())
     ORDER BY o.next_attempt_at LIMIT max_rows
$$;
