package preflight_test

import (
	_ "embed" // fixture SQL
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
		name, role string
		want       []preflight.Finding
	}{
		// The role's search_path cannot hide a finding behind a look-alike catalog.
		{"shadowed catalog", "pf_shadowed", []preflight.Finding{
			critical("role_membership", "member of pg_execute_server_program"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := preflight.Audit(t.Context(), engineQuerier(t, testdb.DSN(t, tt.role, database)))
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
