package guard_test

import (
	"errors"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/guard"
)

func TestCheckSearchPath(t *testing.T) {
	for _, schema := range []string{"net", "NET"} {
		var v *guard.Violation
		if err := guard.CheckSearchPath(schema); !errors.As(err, &v) {
			t.Errorf("CheckSearchPath(%q) = %v, want a *Violation", schema, err)
		}
	}
	// Function rules match names, not schemas: a schema named dblink is fine.
	for _, schema := range []string{"", "app", "public", "pg_catalog", "network", "dblink", "lo_"} {
		if err := guard.CheckSearchPath(schema); err != nil {
			t.Errorf("CheckSearchPath(%q) = %v, want nil", schema, err)
		}
	}
}
