package preflight_test

import (
	_ "embed" // fixture SQL
	"errors"
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/preflight"
	"github.com/jpgomesf/mcp-postgres/internal/testdb"
)

//go:embed testdata/escalation.sql
var escalationSQL string

// TestAuditEscalation audits, through the engine as the server does, roles
// that reach past reading in ways testdata/fixture.sql does not cover. Each
// has a critical finding, so role_check strict refuses it.
func TestAuditEscalation(t *testing.T) {
	const database = "preflight_escalation"
	testdb.CreateDatabase(t, database, escalationSQL)
	tests := []struct {
		name, login, role string // role: the current user once logged in as login
		want              []preflight.Finding
	}{
		// The role's search_path cannot hide a finding behind a look-alike catalog.
		{"shadowed catalog", "pf_shadowed", "pf_shadowed", []preflight.Finding{
			critical("role_membership", "member of pg_execute_server_program"),
		}},
		{"extension escapes", "pf_escapes", "pf_escapes", []preflight.Finding{
			critical("function_execute", "EXECUTE on 3 functions: aws_lambda.invoke, aws_s3.query_export_to_s3, bg.pg_background_launch"),
			warning("function_execute", "EXECUTE on 2 functions: pg_catalog.pg_log_backend_memory_contexts, pg_catalog.pg_read_binary_file"),
		}},
		{"member of a superuser", "pf_climber", "pf_climber", []preflight.Finding{
			critical("superuser_membership", "member of pf_root"),
		}},
		{"superuser session user", "pf_switcher", "pf_quiet", []preflight.Finding{
			critical("superuser", "session user pf_switcher: bypasses every privilege check"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := preflight.Audit(t.Context(), engineQuerier(t, testdb.DSN(t, tt.login, database)))
			if err != nil {
				t.Fatal(err)
			}
			if report.Role != tt.role {
				t.Errorf("role = %q, want %q", report.Role, tt.role)
			}
			wantFindings(t, report, tt.want...)
			if !report.Blocking() {
				t.Error("Blocking() = false: role_check strict would admit this role")
			}
		})
	}
}

// sessionUser is the replies for a session user admin, other than the
// current user app, after the current user's: a member of the superuser root
// that can write one table.
func sessionUser() []reply {
	replies := append(clean(),
		row("f", "f", "f", "f"),      // attributes
		row(nil),                     // memberships
		row("1", "root"),             // superusers
		row("1", "INSERT on app.t"),  // table writes
		row("0", nil), row("0", nil), // function grants
	)
	replies[usersQuery] = row("app", "admin")
	replies[warningFunctionsQuery] = row("1", "pg_catalog.pg_read_file")
	return replies
}

func TestAuditAlsoAuditsADifferentSessionUser(t *testing.T) {
	s := &script{replies: sessionUser()}
	report, err := preflight.Audit(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if report.Role != "app" || s.queries != len(s.replies) {
		t.Errorf("role = %q after %d queries, want app after %d", report.Role, s.queries, len(s.replies))
	}
	wantFindings(t, report,
		critical("superuser_membership", "session user admin: member of root"),
		warning("function_execute", "EXECUTE on pg_catalog.pg_read_file"),
		warning("table_write", "session user admin: INSERT on app.t"),
	)
}

func TestAuditSuperuserSessionUserStopsAtItsAttributes(t *testing.T) {
	replies := append(clean(), row("t", "t", "t", "t"))
	replies[usersQuery] = row("app", "admin")
	s := &script{replies: replies}
	report, err := preflight.Audit(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if s.queries != len(replies) {
		t.Errorf("%d queries, want %d", s.queries, len(replies))
	}
	wantFindings(t, report, critical("superuser", "session user admin: bypasses every privilege check"))
}

// A failed query of the session user's audit fails the whole audit, as one
// of the current user's does.
func TestAuditFailsClosedOnTheSessionUser(t *testing.T) {
	boom := errors.New("connection reset")
	replies := sessionUser()
	replies[len(clean())+tableWritesQuery-attributesQuery] = reply{err: boom} // the session user's
	report, err := preflight.Audit(t.Context(), &script{replies: replies})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "preflight: table writes: ") {
		t.Errorf("error = %v, want one wrapping %v from the table writes", err, boom)
	}
	if report.Role != "" || report.Findings != nil {
		t.Errorf("a failed audit returned %+v, want an empty report", report)
	}
}
