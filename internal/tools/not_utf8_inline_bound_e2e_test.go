package tools_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// Postgres sends a UTF8 client the bytes of shared catalogs unchecked, and a
// session in a SQL_ASCII database can write any byte there, such as a
// database comment in Latin-1, which a UTF8 database can then take into the
// names it creates, such as those of columns and types. JSON carries each
// byte that is not UTF-8 as U+FFFD, three bytes, so the server replaces them
// before it measures: rows, column names, the types a summary lists and
// errors keep to the inline limit as the client decodes them.
func TestBytesThatAreNotUTF8StayWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	const legacy, names = "tools_not_utf8_legacy", "tools_not_utf8_names"
	admin := testdb.Conn(t, testdb.Superuser, "postgres")
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+legacy+" ENCODING 'SQL_ASCII' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+legacy+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", legacy, err)
		}
	})
	testdb.CreateDatabase(t, names, "")
	if _, err := testdb.Conn(t, testdb.Superuser, legacy).Exec(t.Context(),
		"COMMENT ON DATABASE "+legacy+" IS E'caf"+strings.Repeat(`\xE9`, 196)+"';\n"+
			"COMMENT ON DATABASE "+names+" IS E'"+strings.Repeat(`\xE9`, 57)+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := testdb.Conn(t, testdb.Superuser, names).Exec(t.Context(), `DO $$
	DECLARE latin1 text := (SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = current_database());
	BEGIN
		EXECUTE format('CREATE TYPE %I AS ENUM (%L)', 't' || latin1, repeat('x', 63));
		EXECUTE 'CREATE TABLE named (' || (SELECT string_agg(format('%I int', 'c' || lpad(g::text, 3, '0') || latin1), ', ')
			FROM generate_series(1, 120) g) || ')';
		EXECUTE 'CREATE TABLE typed AS SELECT ' || (SELECT string_agg(format('%L::%I AS c%s', repeat('x', 63), 't' || latin1, lpad(g::text, 2, '0')), ', ')
			FROM generate_series(1, 20) g) || ' FROM generate_series(1, 10)';
	END $$`); err != nil {
		t.Fatal(err)
	}
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{
		"app":   direct(testdb.DSN(t, testdb.Superuser, testdb.DBApp)),
		"names": direct(testdb.DSN(t, testdb.Superuser, names)),
	}))

	const comment = "shobj_description(d.oid, 'pg_database')"
	header := make([]string, 120)
	for i := range header {
		header[i] = fmt.Sprintf("c%03d\uFFFD", i+1)
	}
	for _, tc := range []struct{ conn, sql, want string }{
		{"app", "SELECT " + comment + " AS comment FROM pg_database d, generate_series(1, 49) WHERE d.datname = '" + legacy + "'",
			"comment\n" + strings.Repeat("caf\uFFFD\n", 49) + "(49 rows)"},
		{"app", "SELECT repeat(" + comment + ", 45)::int FROM pg_database d WHERE d.datname = '" + legacy + "'",
			`ERROR: invalid input syntax for type integer: "` + strings.Repeat("caf\uFFFD", 45) + `" (SQLSTATE 22P02)`},
		{"names", "SELECT * FROM named", strings.Join(header, "\t") + "\n(0 rows)"},
	} {
		if got, _ := s.call(t, "pg_query", args(tc.conn, tc.sql)); got != tc.want || len(got) > config.InlineMaxBytes {
			t.Errorf("%s:\n%d bytes at the client, limit %d:\n%.300q\nwant\n%.300q", tc.sql, len(got), config.InlineMaxBytes, got, tc.want)
		}
	}
	got := s.ok(t, "pg_query", args("names", "SELECT * FROM typed"))
	if !strings.Contains(got, "\nc01\tt\uFFFD\n") || len(got) > config.InlineMaxBytes {
		t.Errorf("summary of columns of a type named in Latin-1: %d bytes at the client, limit %d:\n%.600q", len(got), config.InlineMaxBytes, got)
	}
}
