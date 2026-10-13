-- Fixture for Profile: tables with and without statistics, column grants, a
-- partitioned table whose parent was never analyzed, more columns than
-- Profile shows, and relations it cannot profile.

-- Never analyzed: autovacuum is off, so it stays that way.
CREATE TABLE fresh (
    id   int,
    note text,
    doc  json
) WITH (autovacuum_enabled = false);
INSERT INTO fresh (id, note, doc)
SELECT g, CASE WHEN g % 3 <> 0 THEN 'n' || g % 7 END, json_build_object('even', g % 2 = 0)
FROM generate_series(1, 300) AS g;

-- The partitions are analyzed, the parent is not: pg_stats has nothing on
-- it, and its rows are the sum of its partitions'.
CREATE TABLE events (id int, part int) PARTITION BY LIST (part);
CREATE TABLE events_1 PARTITION OF events FOR VALUES IN (1) WITH (autovacuum_enabled = false);
CREATE TABLE events_2 PARTITION OF events FOR VALUES IN (2) WITH (autovacuum_enabled = false);
INSERT INTO events (id, part) SELECT g, 1 + g % 2 FROM generate_series(1, 40000) AS g;
ANALYZE events_1, events_2;

-- ro_clean may SELECT only id and label.
CREATE TABLE secrets (id int, label text, token text);
INSERT INTO secrets (id, label, token) SELECT g, 'label ' || g % 3, md5(g::text) FROM generate_series(1, 30) AS g;
CREATE INDEX secrets_label_idx ON secrets (label);
ANALYZE secrets;

-- ro_clean may not read it at all.
CREATE TABLE hidden (id int);
INSERT INTO hidden (id) SELECT g FROM generate_series(1, 10) AS g;
ANALYZE hidden;

-- 60 columns, c01 to c60, analyzed.
DO $$
BEGIN
    EXECUTE 'CREATE TABLE wide (' ||
        (SELECT string_agg(format('c%s int', lpad(g::text, 2, '0')), ', ') FROM generate_series(1, 60) AS g) || ')';
END $$;
INSERT INTO wide (c01) SELECT g FROM generate_series(1, 10) AS g;
ANALYZE wide;

CREATE VIEW fresh_ids AS SELECT id FROM fresh;

GRANT SELECT ON fresh, events, wide, fresh_ids TO ro_clean;
GRANT SELECT (id, label) ON secrets TO ro_clean;
