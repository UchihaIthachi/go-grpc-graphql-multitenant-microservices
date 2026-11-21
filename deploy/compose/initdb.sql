-- Runs once, as the postgres superuser, when the volume is first created.
--
-- One database per service. Two roles:
--   owner  owns every table and runs the migrations. BYPASSRLS, because the few SECURITY
--          DEFINER functions (catalog.expire_reservations, orders.due_sagas, the ledger balance
--          trigger) do system work across tenants. Not a superuser.
--   app    what the services connect as at runtime. Owns nothing, NOBYPASSRLS: every query
--          it runs is filtered by the tenant_isolation policies.
CREATE ROLE owner LOGIN PASSWORD 'owner' NOSUPERUSER BYPASSRLS;
CREATE ROLE app   LOGIN PASSWORD 'app'   NOSUPERUSER NOBYPASSRLS;

CREATE DATABASE account OWNER owner;
CREATE DATABASE catalog OWNER owner;
CREATE DATABASE payment OWNER owner;
CREATE DATABASE orders  OWNER owner;

REVOKE ALL ON DATABASE account, catalog, payment, orders FROM PUBLIC;
GRANT CONNECT ON DATABASE account, catalog, payment, orders TO owner, app;
