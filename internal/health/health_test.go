package health

import (
	"fmt"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jpgomesf/pgglass/internal/testdb"
)

// The tests share one server and list its sessions, so none runs in
// parallel: a session one test blocks would show in another's report.
func TestMain(m *testing.M) { testdb.Main(m) }

var sectionNames = []string{"connections", "long_running", "blocked", "cache_hit", "dead_tuples", "unused_indexes", "xid_wraparound", "replication"}

func report(t *testing.T, lines []Line) map[string][]string {
	t.Helper()
	var order []string
	sections := map[string][]string{}
	for _, l := range lines {
		if !slices.Contains(order, l.Section) {
			order = append(order, l.Section)
		}
		sections[l.Section] = append(sections[l.Section], l.Detail)
	}
	equal(t, order, sectionNames)
	return sections
}

func checkAs(t *testing.T, role, database string, minDuration time.Duration) map[string][]string {
	t.Helper()
	lines, err := check(t.Context(), testdb.Querier(t, role, database), sections(minDuration))
	if err != nil {
		t.Fatalf("check as %s on %s: %v", role, database, err)
	}
	return report(t, lines)
}

func match(t *testing.T, section string, got []string, patterns ...string) {
	t.Helper()
	if len(got) != len(patterns) {
		t.Errorf("%s = %q, want %d line(s) matching %q", section, got, len(patterns), patterns)
		return
	}
	for i, p := range patterns {
		if !regexp.MustCompile("^" + p + "$").MatchString(got[i]) {
			t.Errorf("%s line %d = %q, want it to match %q", section, i+1, got[i], p)
		}
	}
}

func waitFor(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !testdb.Value[bool](t, conn, sql, args...) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", sql)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestCheckOnTheAppFixture(t *testing.T) {
	testdb.Conn(t, testdb.Superuser, testdb.DBApp)
	unused := []string{
		"app.customers_lower_name_idx on app.customers: 16 kB",
		"app.orders_customer_id_idx on app.orders: 16 kB",
		"app.orders_pending_placed_at_idx on app.orders: 16 kB",
	}
	for _, tc := range []struct {
		role        string
		connections string
		longRunning []string
	}{
		{testdb.Superuser, `\d+ of max_connections 100: [a-z ,0-9]+`, []string{"none"}},
		{testdb.RoleClean, `up to \d+ of max_connections 100: active 1; the type and state of \d+ are not visible to role ro_clean`,
			[]string{"none", "other roles' sessions are not visible to role ro_clean"}},
	} {
		got := checkAs(t, tc.role, testdb.DBApp, longRunning)
		match(t, "connections", got["connections"], tc.connections)
		equal(t, got["long_running"], tc.longRunning)
		equal(t, got["blocked"], []string{"none"})
		match(t, "cache_hit", got["cache_hit"], `\d{1,3}\.\d\d% in database app`)
		equal(t, got["dead_tuples"], []string{"none"})
		equal(t, got["unused_indexes"], unused)
		match(t, "xid_wraparound", got["xid_wraparound"],
			`oldest age\(datfrozenxid\) \d+ \(database [a-z_0-9]+\): 0\.0% of autovacuum_freeze_max_age 200000000`)
		equal(t, got["replication"], []string{"not a replica"})
	}
}

func TestCheckFindsDeadTuplesAndUnusedIndexes(t *testing.T) {
	const name = "health_tables"
	testdb.CreateDatabase(t, name, `
		CREATE TABLE churn (id int PRIMARY KEY, v int) WITH (autovacuum_enabled = off);
		CREATE INDEX churn_v_idx ON churn (v);
		CREATE UNIQUE INDEX churn_v_id_key ON churn (v, id);
		CREATE TABLE scanned (id int, v int) WITH (autovacuum_enabled = off);
		CREATE INDEX scanned_v_idx ON scanned (v);
		CREATE TABLE tiny (id int) WITH (autovacuum_enabled = off);
		CREATE TABLE bookings (during int4range, EXCLUDE USING gist (during WITH &&));
		INSERT INTO churn SELECT g, g FROM generate_series(1, 100) g;
		DELETE FROM churn WHERE id <= 25;
		INSERT INTO scanned SELECT g, g FROM generate_series(1, 1000) g;
		UPDATE scanned SET v = v WHERE id <= 10;
		INSERT INTO tiny VALUES (1), (2), (3), (4);
		DELETE FROM tiny WHERE id <= 3;
		SET enable_seqscan = off;
		SELECT count(*) FROM scanned WHERE v = 5;`)
	waitFor(t, testdb.Conn(t, testdb.Superuser, name), `SELECT
		(SELECT n_dead_tup = 25 AND n_live_tup = 75 FROM pg_stat_user_tables WHERE relname = 'churn') AND
		(SELECT n_dead_tup = 10 AND n_live_tup = 1000 FROM pg_stat_user_tables WHERE relname = 'scanned') AND
		(SELECT n_dead_tup = 3 AND n_live_tup = 1 FROM pg_stat_user_tables WHERE relname = 'tiny') AND
		(SELECT idx_scan > 0 FROM pg_stat_user_indexes WHERE indexrelname = 'scanned_v_idx')`)

	got := checkAs(t, testdb.RoleClean, name, longRunning)
	equal(t, got["dead_tuples"], []string{
		"public.churn: 25.0% dead (25 dead, 75 live), never autovacuumed",
		"public.scanned: 1.0% dead (10 dead, 1000 live), never autovacuumed",
		"public.tiny: 75.0% dead (3 dead, 1 live), never autovacuumed",
	})
	equal(t, got["unused_indexes"], []string{"public.churn_v_idx on public.churn: 16 kB"})
}

func TestCheckListsLongRunningAndBlockedSessions(t *testing.T) {
	const name = "health_locks"
	testdb.CreateDatabase(t, name, "CREATE TABLE t (id int PRIMARY KEY, v int); INSERT INTO t VALUES (1, 0);")
	holderConn, waiterConn := testdb.Conn(t, testdb.Superuser, name), testdb.Conn(t, testdb.Superuser, name)
	if _, err := waiterConn.Exec(t.Context(), "SET application_name = 'health_waiter'"); err != nil {
		t.Fatal(err)
	}
	holder, waiter := testdb.Block(t, holderConn, waiterConn, "UPDATE t SET v = 1 WHERE id = 1", "UPDATE t SET v = 2 WHERE id = 1")
	time.Sleep(1100 * time.Millisecond)

	got := checkAs(t, testdb.Superuser, name, time.Second)
	waiting := fmt.Sprintf(`pid %d, database health_locks, active for \d+s, waiting on Lock:transactionid, application "health_waiter"`, waiter)
	idle := fmt.Sprintf(`pid %d, database health_locks, idle in transaction, transaction open for \d+s, waiting on Client:ClientRead`, holder)
	if long := got["long_running"]; len(long) == 2 && regexp.MustCompile("^"+waiting+"$").MatchString(long[0]) {
		match(t, "long_running", long, waiting, idle)
	} else {
		match(t, "long_running", long, idle, waiting)
	}
	match(t, "blocked", got["blocked"], fmt.Sprintf("%s, blocked by %d", waiting, holder))

	got = checkAs(t, testdb.RoleClean, name, time.Second)
	equal(t, got["long_running"], []string{"none", "other roles' sessions are not visible to role ro_clean"})
	equal(t, got["blocked"], []string{fmt.Sprintf(
		`pid %d, database health_locks, state not visible to role ro_clean, application "health_waiter", blocked by %d`, waiter, holder)})

	equal(t, checkAs(t, testdb.Superuser, name, longRunning)["long_running"], []string{"none"})
}

func start(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = conn.Exec(t.Context(), sql)
	}()
	t.Cleanup(func() { <-done })
}

// A session is as old as its open transaction, and its line says so; after a
// failure, as old as the failed query; in a committing procedure, as its CALL.
func TestCheckListsSessionsInOldTransactions(t *testing.T) {
	const name = "health_idle"
	testdb.CreateDatabase(t, name, `CREATE PROCEDURE nap_then_commit() LANGUAGE plpgsql
		AS 'BEGIN PERFORM pg_sleep(1.1); COMMIT; PERFORM pg_sleep(60); END'`)
	conn := func() (*pgx.Conn, int) {
		c := testdb.Conn(t, testdb.Superuser, name)
		return c, testdb.Value[int](t, c, "SELECT pg_backend_pid()")
	}
	idle, idlePID := conn()
	aborted, abortedPID := conn()
	active, activePID := conn()
	call, callPID := conn()
	observer := testdb.Conn(t, testdb.Superuser, name)
	for _, c := range []*pgx.Conn{idle, active} {
		if _, err := c.Exec(t.Context(), "BEGIN"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := aborted.Exec(t.Context(), "BEGIN; SELECT 1/0"); err == nil {
		t.Fatal("SELECT 1/0 succeeded")
	}
	start(t, call, "CALL nap_then_commit()")
	time.Sleep(1100 * time.Millisecond)

	if _, err := idle.Exec(t.Context(), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	start(t, active, "SELECT pg_sleep(60)")
	waitFor(t, observer, `SELECT count(*) = 2 FROM pg_stat_activity
		WHERE (pid = $1 AND wait_event = 'PgSleep') OR (pid = $2 AND xact_start > query_start)`, activePID, callPID)

	long := checkAs(t, testdb.Superuser, name, time.Second)["long_running"]
	for _, want := range []string{
		fmt.Sprintf(`pid %d, database health_idle, idle in transaction, transaction open for [1-9]\d*s, waiting on Client:ClientRead`, idlePID),
		fmt.Sprintf(`pid %d, database health_idle, idle in transaction \(aborted\) for [1-9]\d*s, waiting on Client:ClientRead`, abortedPID),
		fmt.Sprintf(`pid %d, database health_idle, active, transaction open for [1-9]\d*s, waiting on Timeout:PgSleep`, activePID),
		fmt.Sprintf(`pid %d, database health_idle, active for [1-9]\d*s, waiting on Timeout:PgSleep`, callPID),
	} {
		if !slices.ContainsFunc(long, regexp.MustCompile("^"+want+"$").MatchString) {
			t.Errorf("long_running = %q, want a line matching %q", long, want)
		}
	}
	if len(long) != 4 {
		t.Errorf("long_running = %q, want 4 lines", long)
	}
}

// max_connections limits client backends: autovacuum and parallel workers
// have slots of their own, so connections leaves them out.
func TestCheckCountsClientBackendsOnly(t *testing.T) {
	const name = "health_workers"
	testdb.CreateDatabase(t, name, `
		CREATE TABLE t WITH (autovacuum_enabled = off) AS SELECT g FROM generate_series(1, 100000) g;
		ANALYZE t;
		CREATE FUNCTION nap() RETURNS bool PARALLEL SAFE LANGUAGE plpgsql AS 'BEGIN PERFORM pg_sleep(0.001); RETURN true; END';`)
	leader, observer := testdb.Conn(t, testdb.Superuser, name), testdb.Conn(t, testdb.Superuser, name)
	if _, err := leader.Exec(t.Context(), `SET max_parallel_workers_per_gather = 2; SET parallel_setup_cost = 0;
		SET parallel_tuple_cost = 0; SET min_parallel_table_scan_size = 0`); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = leader.Exec(t.Context(), "SELECT count(*) FROM t WHERE nap()")
	}()
	t.Cleanup(func() { <-done })
	waitFor(t, observer, "SELECT count(*) > 0 FROM pg_stat_activity WHERE backend_type = 'parallel worker'")

	got := checkAs(t, testdb.Superuser, name, longRunning)
	clients := testdb.Value[int](t, observer, "SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'")
	match(t, "connections", got["connections"], fmt.Sprintf(`%d of max_connections 100: [a-z ,0-9]+`, clients))
}

// A section whose query the role may not run says so, and the others still
// run. The privileges are revoked in this database only.
func TestCheckSaysWhatTheRoleMayNotSee(t *testing.T) {
	const name = "health_hidden"
	testdb.CreateDatabase(t, name, `
		REVOKE EXECUTE ON FUNCTION pg_catalog.pg_blocking_pids(integer) FROM PUBLIC;
		REVOKE SELECT ON pg_catalog.pg_stat_user_tables FROM PUBLIC;
		REVOKE EXECUTE ON FUNCTION pg_catalog.age(xid) FROM PUBLIC;
		REVOKE EXECUTE ON FUNCTION pg_catalog.pg_is_in_recovery() FROM PUBLIC;`)
	hidden := []string{"not visible to role ro_clean"}

	got := checkAs(t, testdb.RoleClean, name, longRunning)
	match(t, "connections", got["connections"], `(up to )?\d+ of max_connections 100: .+`)
	equal(t, got["long_running"], []string{"none", "other roles' sessions are not visible to role ro_clean"})
	equal(t, got["blocked"], hidden)
	match(t, "cache_hit", got["cache_hit"], `\d{1,3}\.\d\d% in database health_hidden`)
	equal(t, got["dead_tuples"], hidden)
	equal(t, got["unused_indexes"], []string{"none"})
	equal(t, got["xid_wraparound"], hidden)
	equal(t, got["replication"], hidden)

	got = checkAs(t, testdb.Superuser, name, longRunning)
	equal(t, got["blocked"], []string{"none"})
	equal(t, got["dead_tuples"], []string{"none"})
	match(t, "xid_wraparound", got["xid_wraparound"], `oldest age\(datfrozenxid\) .+`)
	equal(t, got["replication"], []string{"not a replica"})
}
