-- Changes to the app fixture, loaded after it into a copy: what Diff must
-- find between the app database and that copy.

-- A removed table and an added one, which ro_clean may read.
DROP TABLE app.audit_log;
CREATE TABLE app.coupons (
    code    text PRIMARY KEY,
    percent int NOT NULL
);
GRANT SELECT ON app.coupons TO ro_clean;

-- A changed column type, nullability and default, and an added column.
ALTER TABLE app.tenant_notes ALTER COLUMN id TYPE bigint;
ALTER TABLE app.customers ALTER COLUMN name DROP NOT NULL;
ALTER TABLE app.customers ALTER COLUMN active SET DEFAULT false;
ALTER TABLE app.orders ADD COLUMN note text;

-- An added index and a removed one.
CREATE INDEX events_kind_idx ON app.events (kind);
DROP INDEX app.customers_lower_name_idx;

-- A changed constraint and a changed policy.
ALTER TABLE app.orders DROP CONSTRAINT orders_total_check,
    ADD CONSTRAINT orders_total_check CHECK (total > 0);
ALTER POLICY tenant_notes_acme ON app.tenant_notes USING (tenant = 'globex');
