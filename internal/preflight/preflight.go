// Package preflight audits what the role behind a connection can do beyond
// reading, so a connection that is not truly read-only is refused or flagged
// (config role_check: strict, warn or off).
package preflight

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jpgomesf/mcp-postgres/internal/engine"
)

var errNotImplemented = errors.New("not implemented")

// Finding severities.
const (
	SeverityCritical = "critical" // the role can change data or escape the read-only transaction
	SeverityWarning  = "warning"  // risky, but not a write path on its own
)

// Finding is one capability the role should not have.
type Finding struct {
	Severity string // SeverityCritical or SeverityWarning
	Check    string // short identifier of the check, e.g. "table_write"
	Detail   string // what was found, e.g. "INSERT on app.audit_log"
}

// Report is the result of Audit.
type Report struct {
	Role     string // current_user of the audited connection
	Findings []Finding
}

// Blocking reports whether any finding is critical.
func (r Report) Blocking() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityCritical {
			return true
		}
	}
	return false
}

// String renders the report for tool output: one line naming the role,
// then one line per finding.
func (r Report) String() string {
	if len(r.Findings) == 0 {
		return fmt.Sprintf("role %s: no findings", r.Role)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "role %s: %d finding(s)", r.Role, len(r.Findings))
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "\n  %s %s: %s", f.Severity, f.Check, f.Detail)
	}
	return b.String()
}

// Audit inspects, through q, the privileges of the role q connects as.
func Audit(ctx context.Context, q engine.Querier) (Report, error) {
	return Report{}, errNotImplemented
}
