package preflight_test

import (
	"testing"

	"github.com/jpgomesf/pgglass/internal/preflight"
)

func TestBlocking(t *testing.T) {
	warning := preflight.Finding{Severity: preflight.SeverityWarning, Check: "temp", Detail: "TEMP on app"}
	critical := preflight.Finding{Severity: preflight.SeverityCritical, Check: "table_write", Detail: "INSERT on app.audit_log"}
	tests := []struct {
		name     string
		findings []preflight.Finding
		want     bool
	}{
		{"no findings", nil, false},
		{"warnings only", []preflight.Finding{warning, warning}, false},
		{"one critical", []preflight.Finding{warning, critical}, true},
	}
	for _, tt := range tests {
		if got := (preflight.Report{Role: "r", Findings: tt.findings}).Blocking(); got != tt.want {
			t.Errorf("%s: Blocking() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestString(t *testing.T) {
	if got, want := (preflight.Report{Role: "ro_clean"}).String(), "role ro_clean: no findings"; got != want {
		t.Errorf("clean report = %q, want %q", got, want)
	}
	report := preflight.Report{Role: "ro_leaky", Findings: []preflight.Finding{
		{Severity: preflight.SeverityCritical, Check: "table_write", Detail: "INSERT on app.audit_log"},
		{Severity: preflight.SeverityWarning, Check: "file_read", Detail: "member of pg_read_server_files"},
	}}
	want := "role ro_leaky: 2 finding(s)\n" +
		"  critical table_write: INSERT on app.audit_log\n" +
		"  warning file_read: member of pg_read_server_files"
	if got := report.String(); got != want {
		t.Errorf("String() =\n%s\nwant\n%s", got, want)
	}
}
