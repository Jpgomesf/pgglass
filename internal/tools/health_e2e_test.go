package tools_test

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

var healthSections = []string{"connections", "long_running", "blocked", "cache_hit", "dead_tuples", "unused_indexes", "xid_wraparound", "replication"}

// healthReport calls pg_health on conn and returns the notice its result
// starts with, its rows, and the details of each section.
func healthReport(t *testing.T, s *session, conn string) (notice string, rows int, sections map[string][]string) {
	t.Helper()
	text := s.ok(t, "pg_health", map[string]any{"connection": conn})
	notice, body, ok := strings.Cut(text, "section\tdetail\n")
	if !ok {
		t.Fatalf("pg_health %s: no header in\n%s", conn, text)
	}
	lines := strings.Split(body, "\n")
	lines, footer := lines[:len(lines)-1], lines[len(lines)-1]
	if want := fmt.Sprintf("(%d rows)", len(lines)); footer != want {
		t.Errorf("pg_health %s: footer %q, want %q", conn, footer, want)
	}
	var order []string
	sections = map[string][]string{}
	for _, line := range lines {
		section, detail, _ := strings.Cut(line, "\t")
		if !slices.Contains(order, section) {
			order = append(order, section)
		}
		sections[section] = append(sections[section], detail)
	}
	if !slices.Equal(order, healthSections) {
		t.Errorf("pg_health %s: sections %q, want %q", conn, order, healthSections)
	}
	return notice, len(lines), sections
}

func TestHealthOnTheAppFixture(t *testing.T) {
	t.Parallel()
	testdb.Conn(t, testdb.Superuser, testdb.DBApp) // a session whose state ro_clean may not see
	s := connect(t, newConfig(config.RoleCheckWarn, appConns(t)))

	notice, superRows, super := healthReport(t, s, "super")
	if want := "role check: role postgres: 1 finding(s)\n  critical superuser: bypasses every privilege check\n\n"; notice != want {
		t.Errorf("notice = %q, want %q", notice, want)
	}
	notice, cleanRows, clean := healthReport(t, s, "clean")
	if notice != "" {
		t.Errorf("ro_clean has no findings, yet: %q", notice)
	}

	connections := regexp.MustCompile(`^(up to )?\d+ of max_connections 100: `)
	for role, got := range map[string]map[string][]string{"postgres": super, "ro_clean": clean} {
		if len(got["connections"]) != 1 || !connections.MatchString(got["connections"][0]) {
			t.Errorf("%s: connections = %q", role, got["connections"])
		}
		if !slices.Contains(got["unused_indexes"], "app.customers_lower_name_idx on app.customers: 16 kB") {
			t.Errorf("%s: unused_indexes = %q", role, got["unused_indexes"])
		}
		if !slices.Equal(got["replication"], []string{"not a replica"}) {
			t.Errorf("%s: replication = %q", role, got["replication"])
		}
	}
	// Only the superuser sees every session in full. To ro_clean, the backends
	// whose type it may not see make the count of client backends an upper
	// bound.
	if slices.ContainsFunc(super["connections"], isHidden) || slices.ContainsFunc(super["long_running"], isHidden) {
		t.Errorf("the superuser sees every session, yet:\n%q\n%q", super["connections"], super["long_running"])
	}
	upperBound := regexp.MustCompile(`^up to \d+ of max_connections 100: .+; the type and state of \d+ are not visible to role ro_clean$`)
	if !upperBound.MatchString(strings.Join(clean["connections"], "\n")) ||
		!slices.Contains(clean["long_running"], "other roles' sessions are not visible to role ro_clean") {
		t.Errorf("ro_clean:\n%q\n%q", clean["connections"], clean["long_running"])
	}

	want := []auditLine{
		{Msg: "tool call", Tool: "pg_health", Connection: "super", Rows: superRows, Outcome: "ok"},
		{Msg: "tool call", Tool: "pg_health", Connection: "clean", Rows: cleanRows, Outcome: "ok"},
	}
	lines := s.auditLines(t)
	if len(lines) != len(want) {
		t.Fatalf("%d audit lines, want %d:\n%s", len(lines), len(want), s.audit)
	}
	for i, line := range lines {
		line.DurationMS = nil
		if line != want[i] {
			t.Errorf("audit line %d = %+v\nwant %+v", i, line, want[i])
		}
	}
}

func isHidden(detail string) bool { return strings.Contains(detail, "not visible") }

func TestHealthShowsBlockedSessions(t *testing.T) {
	t.Parallel()
	const name = "tools_health_blocked"
	testdb.CreateDatabase(t, name, "CREATE TABLE t (id int PRIMARY KEY, v int); INSERT INTO t VALUES (1, 0);")
	holder, waiter := testdb.Block(t, testdb.Conn(t, testdb.Superuser, name), testdb.Conn(t, testdb.Superuser, name),
		"UPDATE t SET v = 1 WHERE id = 1", "UPDATE t SET v = 2 WHERE id = 1")
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{
		"super": direct(testdb.DSN(t, testdb.Superuser, name)),
		"clean": direct(testdb.DSN(t, testdb.RoleClean, name)),
	}))

	_, _, super := healthReport(t, s, "super")
	blocked := regexp.MustCompile(fmt.Sprintf(`^pid %d, database %s, active for \d+s, waiting on Lock:transactionid, blocked by %d$`, waiter, name, holder))
	if !slices.ContainsFunc(super["blocked"], blocked.MatchString) {
		t.Errorf("blocked = %q, want a line matching %s", super["blocked"], blocked)
	}
	// ro_clean finds the session, but not what it is doing.
	_, _, clean := healthReport(t, s, "clean")
	if want := fmt.Sprintf("pid %d, database %s, state not visible to role ro_clean, blocked by %d", waiter, name, holder); !slices.Contains(clean["blocked"], want) {
		t.Errorf("blocked = %q, want it to contain %q", clean["blocked"], want)
	}
}

// dead_tuples ranks tables by their dead rows, so a tiny table with a high
// share of them comes after a large one with more.
func TestHealthRanksTablesByDeadRows(t *testing.T) {
	t.Parallel()
	const name = "tools_health_dead"
	testdb.CreateDatabase(t, name, `
		CREATE TABLE large (id int) WITH (autovacuum_enabled = off);
		CREATE TABLE tiny (id int) WITH (autovacuum_enabled = off);
		INSERT INTO large SELECT generate_series(1, 1000);
		DELETE FROM large WHERE id <= 10;
		INSERT INTO tiny VALUES (1), (2), (3), (4);
		DELETE FROM tiny WHERE id <= 3;`)
	// The statistics reach other sessions some time after the statements
	// that change them.
	conn := testdb.Conn(t, testdb.Superuser, name)
	for deadline := time.Now().Add(10 * time.Second); !testdb.Value[bool](t, conn, `SELECT
		(SELECT n_dead_tup = 10 AND n_live_tup = 990 FROM pg_stat_user_tables WHERE relname = 'large') AND
		(SELECT n_dead_tup = 3 AND n_live_tup = 1 FROM pg_stat_user_tables WHERE relname = 'tiny')`); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the statistics")
		}
	}
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{"clean": direct(testdb.DSN(t, testdb.RoleClean, name))}))

	_, _, got := healthReport(t, s, "clean")
	want := []string{
		"public.large: 1.0% dead (10 dead, 990 live), never autovacuumed",
		"public.tiny: 75.0% dead (3 dead, 1 live), never autovacuumed",
	}
	if !slices.Equal(got["dead_tuples"], want) {
		t.Errorf("dead_tuples = %q, want %q", got["dead_tuples"], want)
	}
}

// Every section reads a view or calls a function that is revoked here, so
// each one says it is not visible, and the call still succeeds.
func TestHealthDegradesWithoutPrivileges(t *testing.T) {
	t.Parallel()
	const name = "tools_health_hidden"
	testdb.CreateDatabase(t, name, `
		REVOKE SELECT ON pg_catalog.pg_stat_activity, pg_catalog.pg_stat_database,
			pg_catalog.pg_stat_user_tables, pg_catalog.pg_stat_user_indexes FROM PUBLIC;
		REVOKE EXECUTE ON FUNCTION pg_catalog.age(xid), pg_catalog.pg_is_in_recovery() FROM PUBLIC;`)
	s := connect(t, newConfig(config.RoleCheckWarn, map[string]config.Connection{"clean": direct(testdb.DSN(t, testdb.RoleClean, name))}))

	_, _, got := healthReport(t, s, "clean")
	for _, section := range healthSections {
		if !slices.Equal(got[section], []string{"not visible to role ro_clean"}) {
			t.Errorf("%s = %q, want it not visible to ro_clean", section, got[section])
		}
	}
	if lines := s.auditLines(t); len(lines) != 1 || lines[0].Outcome != "ok" || lines[0].Rows != len(healthSections) {
		t.Errorf("audit lines = %+v", lines)
	}
}

// pg_health goes through the role check and the argument checks of every
// tool, and the audit log records each call.
func TestHealthGoesThroughTheRoleCheck(t *testing.T) {
	t.Parallel()
	s := connect(t, newConfig(config.RoleCheckStrict, appConns(t)))
	got := s.fails(t, "pg_health", map[string]any{"connection": "super"})
	if !strings.HasPrefix(got, "connection \"super\" refused (role_check strict): its role can do more than read.\n") {
		t.Errorf("pg_health on the superuser under role_check strict:\n%s", got)
	}
	_, rows, _ := healthReport(t, s, "clean")
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"connection": "nope"}, `unknown connection "nope"`},
		{map[string]any{"connection": "clean", "verbose": true}, `invalid arguments: json: unknown field "verbose"`},
	} {
		if got := s.fails(t, "pg_health", tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("pg_health %v = %q, want it to contain %q", tc.args, got, tc.want)
		}
	}

	want := []auditLine{
		{Msg: "tool call", Tool: "pg_health", Connection: "super", Outcome: "preflight_block"},
		{Msg: "tool call", Tool: "pg_health", Connection: "clean", Rows: rows, Outcome: "ok"},
		{Msg: "tool call", Tool: "pg_health", Outcome: "error"},
		{Msg: "tool call", Tool: "pg_health", Outcome: "error"},
	}
	lines := s.auditLines(t)
	if len(lines) != len(want) {
		t.Fatalf("%d audit lines, want %d:\n%s", len(lines), len(want), s.audit)
	}
	for i, line := range lines {
		line.DurationMS = nil
		if line != want[i] {
			t.Errorf("audit line %d = %+v\nwant %+v", i, line, want[i])
		}
	}
}

// The definition is small, and says long_running lists open transactions
// too, not only running queries.
func TestHealthDefinition(t *testing.T) {
	tool, size := listedTool(t, "pg_health")
	if size > 700 {
		t.Errorf("pg_health definition takes %d bytes of JSON, want at most 700", size)
	}
	if !strings.Contains(tool.Description, "queries running or transactions open over a minute") {
		t.Errorf("description %q, want it to name open transactions", tool.Description)
	}
}
