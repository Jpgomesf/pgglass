package guard_test

import (
	"errors"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/guard"
)

// The API other packages build against. Changing it must fail here first.
var (
	_ func(string) error = guard.Check
	_ error              = (*guard.Violation)(nil)
)

func TestViolationIsAnError(t *testing.T) {
	var err error = &guard.Violation{Reason: "SET is not allowed"}
	var v *guard.Violation
	if !errors.As(err, &v) || v.Error() != "SET is not allowed" {
		t.Errorf("Violation error = %v", err)
	}
}
