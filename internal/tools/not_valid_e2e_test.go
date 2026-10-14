package tools_test

import (
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

const (
	notValidSQL = `CREATE TABLE notes (id int PRIMARY KEY, body text);
		INSERT INTO notes VALUES (1, NULL);
		DO $$
		BEGIN
			IF current_setting('server_version_num')::int >= 180000 THEN
				EXECUTE 'ALTER TABLE notes ADD CONSTRAINT notes_body_not_null NOT NULL body NOT VALID';
			END IF;
		END $$;`
	validatedSQL = `CREATE TABLE notes (id int PRIMARY KEY, body text NOT NULL);
		INSERT INTO notes VALUES (1, 'first');`
)

// pg_schema and pg_diff_schema show a column as NOT NULL only once Postgres has
// checked every row, so a NOT VALID one (PostgreSQL 18) is nullable.
func TestSchemaAndDiffShowANotValidNotNullColumnAsNullable(t *testing.T) {
	t.Parallel()
	testdb.CreateDatabase(t, "tools_not_valid", notValidSQL)
	testdb.CreateDatabase(t, "tools_validated", validatedSQL)
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"not_valid": direct(testdb.DSN(t, testdb.Superuser, "tools_not_valid")),
		"validated": direct(testdb.DSN(t, testdb.Superuser, "tools_validated")),
	}))

	got := sizes.ReplaceAllString(s.ok(t, "pg_schema", map[string]any{"connection": "not_valid", "schema": "public", "table": "notes"}), "SIZE")
	if want := "public.notes: table, SIZE\n  id integer NOT NULL\n  body text\nconstraints:\n  notes_pkey PRIMARY KEY (id)"; got != want {
		t.Errorf("pg_schema:\n%s\nwant\n%s", got, want)
	}
	got = s.ok(t, "pg_diff_schema", diffArgs("validated", "not_valid"))
	if want := "schema public, validated → not_valid: 1 difference (- only in validated, + only in not_valid)\n" +
		"~ column notes.body: text NOT NULL → text"; got != want {
		t.Errorf("pg_diff_schema:\n%s\nwant\n%s", got, want)
	}
}
