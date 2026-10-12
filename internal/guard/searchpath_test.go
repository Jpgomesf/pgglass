package guard_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/guard"
)

// With a denied schema on the search path, its functions could be called
// unqualified, past Check's schema rule.
func TestCheckSearchPathRejectsDeniedSchemas(t *testing.T) {
	for _, c := range []struct{ schema, want string }{
		{"net", "schema net is not allowed as the search_path"},
		{"aws_s3", "schema aws_s3 is not allowed"},
		{"aws_lambda", "schema aws_lambda is not allowed"},
		{"NET", "schema NET is not allowed"},
		// pgx drops NUL bytes when it quotes the schema, leaving "net".
		{"n\x00et", "NUL byte"},
	} {
		err := guard.CheckSearchPath(c.schema)
		var v *guard.Violation
		if !errors.As(err, &v) || !strings.Contains(v.Reason, c.want) {
			t.Errorf("CheckSearchPath(%q) = %v, want a *Violation containing %q", c.schema, err, c.want)
		}
	}
	// Function rules match names, not schemas: a schema named dblink is fine.
	for _, schema := range []string{"", "app", "public", "pg_catalog", "extensions", "network", "net2", "dblink", "lo_"} {
		if err := guard.CheckSearchPath(schema); err != nil {
			t.Errorf("CheckSearchPath(%q) = %v, want nil", schema, err)
		}
	}
}
