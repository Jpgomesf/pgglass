package guard_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/guard"
)

// CheckRelation denies by name the relations whose references Check rejects.
func TestCheckRelationRejectsDeniedRelations(t *testing.T) {
	for _, name := range []string{"pg_authid", "pg_subscription", "pg_user_mapping", "PG_AUTHID"} {
		err := guard.CheckRelation(name)
		want := "relation " + name + " is not allowed: it discloses server configuration or credentials"
		var v *guard.Violation
		if !errors.As(err, &v) || v.Reason != want {
			t.Errorf("CheckRelation(%q) = %v, want a *Violation: %s", name, err, want)
		}
		// Check rejects a reference to it the same way.
		if err := guard.Check(`SELECT FROM pg_catalog."` + name + `"`); err == nil || err.Error() != want {
			t.Errorf("Check of a reference to %s = %v, want %s", name, err, want)
		}
	}
	// pgx drops NUL bytes when it quotes a name, leaving pg_authid.
	var v *guard.Violation
	if err := guard.CheckRelation("pg_au\x00thid"); !errors.As(err, &v) || !strings.Contains(v.Reason, "NUL byte") {
		t.Errorf("CheckRelation with a NUL byte = %v, want a *Violation", err)
	}
	for _, name := range []string{"orders", "pg_roles", "pg_user", "pg_stats", "pg_class", "authid"} {
		if err := guard.CheckRelation(name); err != nil {
			t.Errorf("CheckRelation(%q) = %v, want nil", name, err)
		}
	}
}
