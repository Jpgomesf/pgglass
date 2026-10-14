package testdb

import (
	"errors"
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/engine"
)

// Querier reads as the role through the engine, in a READ ONLY transaction
// that refuses even the writes the role may make.
func TestQuerierReadsThroughTheEngine(t *testing.T) {
	q := Querier(t, RoleLeaky, DBApp)
	res, err := q.Query(t.Context(), engine.Query{SQL: "SELECT current_user, current_database(), current_setting('transaction_read_only')"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range res.Rows[0] {
		got = append(got, *v)
	}
	if strings.Join(got, " ") != "ro_leaky app on" {
		t.Errorf("current_user, current_database(), transaction_read_only = %q, want ro_leaky app on", got)
	}

	_, err = q.Query(t.Context(), engine.Query{SQL: "INSERT INTO app.audit_log (message) VALUES ('through the querier')"})
	var qe *engine.QueryError
	if !errors.As(err, &qe) || qe.SQLState != sqlStateReadOnlyTransaction {
		t.Errorf("INSERT that ro_leaky may make = %v, want SQLSTATE %s", err, sqlStateReadOnlyTransaction)
	}
}

// AppSQL is the fixture of DBApp: loaded into another database, it makes the
// same relations.
func TestAppSQLIsTheAppFixture(t *testing.T) {
	const name = "testdb_app_copy"
	CreateDatabase(t, name, AppSQL())
	const relations = `SELECT string_agg(relname || ':' || relkind::text, ' ' ORDER BY relname)
		FROM pg_class WHERE relnamespace = 'app'::regnamespace`
	want := Value[string](t, Conn(t, Superuser, DBApp), relations)
	if got := Value[string](t, Conn(t, Superuser, name), relations); got != want {
		t.Errorf("relations of the copy:\n%s\nwant those of %s:\n%s", got, DBApp, want)
	}
}

// The helpers fail the test, naming the step and the server's reason, rather
// than return an error every caller must check.
func TestHelpersFailTheTestWithTheReason(t *testing.T) {
	conn := Conn(t, RoleClean, DBApp)
	for _, tc := range []struct {
		name string
		call func(testing.TB)
		want string
	}{
		{"Conn as an unknown role", func(tb testing.TB) { Conn(tb, "testdb_nobody", DBApp) },
			"testdb: connect as testdb_nobody to app: "},
		{"Value of SQL that fails", func(tb testing.TB) { Value[int](tb, conn, "SELECT 'x'::int") },
			"testdb: SELECT 'x'::int: ERROR: invalid input syntax for type integer"},
		{"CreateDatabase of an existing name", func(tb testing.TB) { CreateDatabase(tb, DBApp, "") },
			`testdb: create database app: ERROR: database "app" already exists`},
		{"CreateDatabase of a fixture that fails", func(tb testing.TB) { CreateDatabase(tb, "testdb_bad_fixture", "SELECT nope") },
			`testdb: load fixture into testdb_bad_fixture: ERROR: column "nope" does not exist`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{TB: t}
			attempt(func() { tc.call(r) })
			wantFatal(t, r, tc.want)
		})
	}
	if Value[bool](t, Conn(t, Superuser, DBApp), "SELECT EXISTS (SELECT FROM pg_database WHERE datname = 'testdb_bad_fixture')") {
		t.Error("the database whose fixture failed is still there after its test")
	}
}

// A database dropped before its test ends fails the test when its cleanup
// cannot drop it.
func TestCreateDatabaseReportsADropThatFails(t *testing.T) {
	const name = "testdb_dropped_early"
	admin := Conn(t, Superuser, maintenanceDB)
	r := &recorder{}
	t.Run("create and drop", func(t *testing.T) {
		r.TB = t
		CreateDatabase(r, name, "")
		if _, err := admin.Exec(t.Context(), "DROP DATABASE "+name); err != nil {
			t.Fatal(err)
		}
	})
	if r.failed {
		t.Fatalf("CreateDatabase failed: %s", r.msg)
	}
	if len(r.errors) != 1 || !strings.HasPrefix(r.errors[0], "testdb: drop database "+name+`: ERROR: database "`+name+`" does not exist`) {
		t.Errorf("errors = %q, want the cleanup's failed drop", r.errors)
	}
}
