package tools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
	"github.com/jpgomesf/pgglass/internal/testdb"
)

// Postgres sends a UTF8 client the bytes of shared catalogs unchecked, and a
// session in a SQL_ASCII database can write any byte there, such as a
// database comment in Latin-1. JSON carries each byte that is not UTF-8 as
// U+FFFD, three bytes, so the server replaces them before it measures: rows
// and errors keep to the inline limit as the client decodes them.
func TestBytesThatAreNotUTF8StayWithinTheInlineLimit(t *testing.T) {
	t.Parallel()
	const legacy = "tools_not_utf8_legacy"
	admin := testdb.Conn(t, testdb.Superuser, "postgres")
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+legacy+" ENCODING 'SQL_ASCII' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+legacy+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", legacy, err)
		}
	})
	if _, err := testdb.Conn(t, testdb.Superuser, legacy).Exec(t.Context(),
		"COMMENT ON DATABASE "+legacy+" IS E'caf"+strings.Repeat(`\xE9`, 196)+"'"); err != nil {
		t.Fatal(err)
	}
	s := connect(t, newConfig(config.RoleCheckOff, map[string]config.Connection{"app": direct(testdb.DSN(t, testdb.Superuser, testdb.DBApp))}))

	const comment = "shobj_description(d.oid, 'pg_database')"
	for _, tc := range []struct{ sql, want string }{
		{"SELECT " + comment + " AS comment FROM pg_database d, generate_series(1, 49) WHERE d.datname = '" + legacy + "'",
			"comment\n" + strings.Repeat("caf\uFFFD\n", 49) + "(49 rows)"},
		{"SELECT repeat(" + comment + ", 45)::int FROM pg_database d WHERE d.datname = '" + legacy + "'",
			`ERROR: invalid input syntax for type integer: "` + strings.Repeat("caf\uFFFD", 45) + `" (SQLSTATE 22P02)`},
	} {
		if got, _ := s.call(t, "pg_query", args("app", tc.sql)); got != tc.want || len(got) > config.InlineMaxBytes {
			t.Errorf("%s:\n%d bytes at the client, limit %d:\n%.300q\nwant\n%.300q", tc.sql, len(got), config.InlineMaxBytes, got, tc.want)
		}
	}
}
