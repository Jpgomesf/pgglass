package introspect_test

import (
	"testing"

	"github.com/jpgomesf/pgglass/internal/introspect"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

const notValidSQL = `CREATE TABLE notes (id int PRIMARY KEY, title text NOT NULL, body text);
INSERT INTO notes VALUES (1, 'first', NULL);
DO $$
BEGIN
    IF current_setting('server_version_num')::int >= 180000 THEN
        EXECUTE 'ALTER TABLE notes ADD CONSTRAINT notes_body_not_null NOT NULL body NOT VALID';
    END IF;
END $$;`

// A column is NOT NULL only once Postgres checked every row: body, under a NOT
// VALID not-null constraint from PostgreSQL 18, is nullable on every version.
func TestANotValidNotNullColumnIsNullable(t *testing.T) {
	const name = "introspect_not_valid"
	testdb.CreateDatabase(t, name, notValidSQL)
	q := testdb.Querier(t, testdb.Superuser, name)

	d := describe(t, q, "public", "notes")
	equal(t, "columns", d.Columns, []introspect.Column{
		{Name: "id", Type: "integer", NotNull: true},
		{Name: "title", Type: "text", NotNull: true},
		{Name: "body", Type: "text"},
	})
	equal(t, "constraints", d.Constraints, []introspect.Constraint{
		{Name: "notes_pkey", Kind: "primary key", Definition: "PRIMARY KEY (id)"},
	})

	s, err := introspect.Structure(t.Context(), q, "public")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "structure columns", s["notes"].Columns, map[string]string{
		"id": "integer NOT NULL", "title": "text NOT NULL", "body": "text",
	})
}
