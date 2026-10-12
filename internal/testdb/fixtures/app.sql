-- Fixture database "app": the column types, indexes, constraints and object
-- kinds the server must handle. Loaded by the superuser; values are
-- deterministic (generate_series), so tests can assert on them.

-- dblink opens new sessions that ignore the caller's read-only transaction.
-- It lands in schema public; only ro_leaky may execute it.
CREATE EXTENSION dblink;
REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA public FROM PUBLIC;

CREATE SCHEMA app;
COMMENT ON SCHEMA app IS 'Fixture application schema';

CREATE TYPE app.order_status AS ENUM ('pending', 'paid', 'shipped', 'cancelled');

-- uuid, text, bool, date, timestamptz, text[], inet, json
CREATE TABLE app.customers (
    id          uuid PRIMARY KEY,
    email       text NOT NULL,
    name        text NOT NULL,
    active      boolean NOT NULL DEFAULT true,
    signup_date date NOT NULL,
    created_at  timestamptz NOT NULL,
    tags        text[] NOT NULL DEFAULT '{}',
    last_ip     inet,
    profile     json
);
COMMENT ON TABLE app.customers IS 'People who place orders';
COMMENT ON COLUMN app.customers.email IS 'Login email, unique per customer';
CREATE UNIQUE INDEX customers_email_key ON app.customers (email);
CREATE INDEX customers_lower_name_idx ON app.customers (lower(name));

-- bigint identity, foreign key, enum, numeric with a check, int[], jsonb, interval, bytea
CREATE TABLE app.orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id uuid NOT NULL REFERENCES app.customers (id),
    status      app.order_status NOT NULL DEFAULT 'pending',
    total       numeric(12, 2) NOT NULL CONSTRAINT orders_total_check CHECK (total >= 0),
    quantities  int[] NOT NULL,
    metadata    jsonb,
    fulfillment interval,
    receipt     bytea,
    placed_at   timestamptz NOT NULL
);
CREATE INDEX orders_customer_id_idx ON app.orders (customer_id);
CREATE INDEX orders_pending_placed_at_idx ON app.orders (placed_at) WHERE status = 'pending';

-- The large table: 20k rows for row and byte limits.
CREATE TABLE app.events (
    id          bigint PRIMARY KEY,
    kind        text NOT NULL,
    payload     jsonb NOT NULL,
    occurred_at timestamptz NOT NULL
);

-- Row-level security: non-owners see only tenant 'acme' (half the rows).
CREATE TABLE app.tenant_notes (
    id     int PRIMARY KEY,
    tenant text NOT NULL,
    note   text NOT NULL
);
ALTER TABLE app.tenant_notes ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_notes_acme ON app.tenant_notes FOR SELECT USING (tenant = 'acme');

-- The one table ro_leaky may INSERT into.
CREATE TABLE app.audit_log (
    id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message   text NOT NULL,
    logged_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO app.customers (id, email, name, active, signup_date, created_at, tags, last_ip, profile)
SELECT md5('customer-' || g)::uuid,
       'customer' || g || '@example.com',
       'Customer ' || g,
       g % 5 <> 0,
       date '2024-01-01' + g,
       timestamptz '2024-01-01 00:00:00+00' + g * interval '1 day',
       ARRAY['tier' || g % 3, 'region' || g % 4],
       CASE WHEN g % 7 <> 0 THEN ('10.0.0.' || g)::inet END,
       CASE WHEN g % 10 <> 0 THEN json_build_object('newsletter', g % 2 = 0, 'score', g) END
FROM generate_series(1, 50) AS g;

INSERT INTO app.orders (customer_id, status, total, quantities, metadata, fulfillment, receipt, placed_at)
SELECT md5('customer-' || (1 + g % 50))::uuid,
       (ARRAY['pending', 'paid', 'shipped', 'cancelled']::app.order_status[])[1 + g % 4],
       round(g * 12.34, 2),
       ARRAY[1 + g % 3, 1 + g % 5],
       CASE WHEN g % 6 <> 0 THEN jsonb_build_object('channel', CASE WHEN g % 2 = 0 THEN 'web' ELSE 'store' END, 'items', 1 + g % 4) END,
       CASE WHEN g % 4 = 2 THEN make_interval(days => g % 9, hours => g % 24) END,
       CASE WHEN g % 3 = 0 THEN decode(md5('receipt-' || g), 'hex') END,
       timestamptz '2024-03-01 09:00:00+00' + g * interval '6 hours'
FROM generate_series(1, 50) AS g;

INSERT INTO app.events (id, kind, payload, occurred_at)
SELECT g,
       (ARRAY['view', 'click', 'purchase', 'refund'])[1 + g % 4],
       jsonb_build_object('seq', g, 'customer', md5('customer-' || (1 + g % 50))::uuid),
       timestamptz '2024-01-01 00:00:00+00' + g * interval '1 minute'
FROM generate_series(1, 20000) AS g;

INSERT INTO app.tenant_notes (id, tenant, note)
SELECT g, CASE WHEN g % 2 = 0 THEN 'acme' ELSE 'globex' END, 'Note ' || g
FROM generate_series(1, 50) AS g;

INSERT INTO app.audit_log (message, logged_at)
SELECT 'Audit entry ' || g, timestamptz '2024-01-01 00:00:00+00' + g * interval '1 hour'
FROM generate_series(1, 50) AS g;

CREATE VIEW app.customer_order_totals AS
SELECT c.id AS customer_id, c.email, count(o.id) AS order_count, coalesce(sum(o.total), 0) AS total_spent
FROM app.customers AS c
LEFT JOIN app.orders AS o ON o.customer_id = c.id
GROUP BY c.id, c.email;

CREATE MATERIALIZED VIEW app.daily_event_counts AS
SELECT date_trunc('day', occurred_at) AS day, kind, count(*) AS events
FROM app.events
GROUP BY 1, 2;

GRANT USAGE ON SCHEMA app TO ro_clean, ro_leaky;
GRANT SELECT ON ALL TABLES IN SCHEMA app TO ro_clean, ro_leaky;
GRANT INSERT ON app.audit_log TO ro_leaky;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO ro_leaky;

ANALYZE;
