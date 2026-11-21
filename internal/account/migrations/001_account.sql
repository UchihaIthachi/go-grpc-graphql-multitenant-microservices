-- Tenants and their users.
--
-- tenants is a directory (slug -> id) that login needs BEFORE it knows the tenant, so it has no
-- RLS; it holds nothing secret. users is tenant data like everything else: FORCE RLS.

CREATE TABLE account.tenants (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       text        NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$'),
    name       text        NOT NULL CHECK (name <> ''),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE account.users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES account.tenants (id),
    email         text        NOT NULL CHECK (email = lower(email) AND email LIKE '%_@_%'),
    password_hash text        NOT NULL,
    role          text        NOT NULL CHECK (role IN ('admin', 'customer')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, email)
);

ALTER TABLE account.users ENABLE ROW LEVEL SECURITY;
ALTER TABLE account.users FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON account.users
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT USAGE ON SCHEMA account TO {{APP_ROLE}};
GRANT SELECT, INSERT ON account.tenants, account.users TO {{APP_ROLE}};
