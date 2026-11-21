-- Double-entry ledger. Every money movement is an entry with at least two postings whose amounts
-- sum to zero per currency (positive = debit, negative = credit). The rules live in the database,
-- so no code path (or a hand-written SQL session) can create money or edit history.

CREATE TABLE ledger.accounts (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid        NOT NULL,
    code           text        NOT NULL,              -- e.g. customer:<id>, psp_clearing, merchant_payable
    currency       char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    kind           text        NOT NULL CHECK (kind IN ('asset', 'liability', 'revenue', 'expense', 'equity')),
    allow_negative boolean     NOT NULL DEFAULT false,
    balance_minor  bigint      NOT NULL DEFAULT 0,    -- cache of SUM(postings), kept by trigger
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, code, currency),
    UNIQUE (id, currency, tenant_id)                  -- target of the postings FK below
);

CREATE TABLE ledger.entries (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL,
    idempotency_key text        NOT NULL,
    request_hash    bytea       NOT NULL,             -- same key + different body = conflict
    kind            text        NOT NULL,             -- authorize, capture, refund, fee, ...
    memo            text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, idempotency_key),
    UNIQUE (id, tenant_id)
);

CREATE TABLE ledger.postings (
    id           bigint  GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id    uuid    NOT NULL,
    entry_id     uuid    NOT NULL,
    account_id   uuid    NOT NULL,
    amount_minor bigint  NOT NULL CHECK (amount_minor <> 0),
    currency     char(3) NOT NULL,
    -- the posting's currency and tenant must be the account's (FK checks ignore RLS, so the
    -- tenant has to be part of the key: another tenant's account id alone is not enough)
    FOREIGN KEY (account_id, currency, tenant_id) REFERENCES ledger.accounts (id, currency, tenant_id),
    FOREIGN KEY (entry_id, tenant_id) REFERENCES ledger.entries (id, tenant_id)
);
CREATE INDEX postings_account_idx ON ledger.postings (account_id);
CREATE INDEX postings_entry_idx ON ledger.postings (entry_id);

-- Invariant 1: each entry balances per currency and has >= 2 postings. Checked at COMMIT
-- (deferred), so all postings of an entry are inserted before the check runs.
CREATE FUNCTION ledger.check_entry_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    unbalanced text;
    n int;
BEGIN
    SELECT count(*) INTO n FROM ledger.postings WHERE entry_id = NEW.entry_id;
    IF n < 2 THEN
        RAISE EXCEPTION 'ledger_unbalanced: entry % has % posting(s)', NEW.entry_id, n;
    END IF;
    SELECT string_agg(currency || '=' || s, ', ') INTO unbalanced
      FROM (SELECT currency, sum(amount_minor) s FROM ledger.postings
             WHERE entry_id = NEW.entry_id GROUP BY currency HAVING sum(amount_minor) <> 0) t;
    IF unbalanced IS NOT NULL THEN
        RAISE EXCEPTION 'ledger_unbalanced: entry % sums to %', NEW.entry_id, unbalanced;
    END IF;
    RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER entry_balanced
    AFTER INSERT ON ledger.postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger.check_entry_balanced();

-- Invariant 2: balances follow the postings, and only allow_negative accounts may go below 0.
-- SECURITY DEFINER: the app role cannot UPDATE accounts itself, only through this trigger.
CREATE FUNCTION ledger.apply_posting() RETURNS trigger LANGUAGE plpgsql
SECURITY DEFINER SET search_path = ledger, pg_temp AS $$
DECLARE
    acc ledger.accounts%ROWTYPE;
BEGIN
    UPDATE ledger.accounts SET balance_minor = balance_minor + NEW.amount_minor
     WHERE id = NEW.account_id RETURNING * INTO acc;
    IF acc.balance_minor < 0 AND NOT acc.allow_negative THEN
        RAISE EXCEPTION 'ledger_insufficient_funds: account % would be %', acc.code, acc.balance_minor;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER posting_applies_balance
    AFTER INSERT ON ledger.postings
    FOR EACH ROW EXECUTE FUNCTION ledger.apply_posting();

-- Invariant 3: history is append-only, for everyone including the owner.
CREATE FUNCTION ledger.forbid_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger_immutable: % on % is not allowed; post a reversing entry', TG_OP, TG_TABLE_NAME;
END $$;
CREATE TRIGGER entries_immutable BEFORE UPDATE OR DELETE ON ledger.entries
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();
CREATE TRIGGER postings_immutable BEFORE UPDATE OR DELETE ON ledger.postings
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();

-- Tenancy: row-level security on every table, forced even for the owner.
ALTER TABLE ledger.accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.accounts FORCE ROW LEVEL SECURITY;
ALTER TABLE ledger.entries  ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.entries  FORCE ROW LEVEL SECURITY;
ALTER TABLE ledger.postings ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.postings FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON ledger.accounts
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON ledger.entries
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON ledger.postings
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- The application role: read everything of its tenant, insert accounts/entries/postings,
-- never UPDATE or DELETE anything (balances change only through the trigger above).
GRANT USAGE ON SCHEMA ledger TO {{APP_ROLE}};
GRANT SELECT, INSERT ON ledger.accounts, ledger.entries, ledger.postings TO {{APP_ROLE}};
