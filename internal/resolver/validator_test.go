package resolver_test

import (
	"strings"
	"testing"

	"github.com/jpgomesf/pgglass/internal/resolver"
)

func TestValidatorAllPresent(t *testing.T) {
	t.Parallel()
	v := resolver.NewValidator("conn", "direct")
	if got := v.Require("dsn", "postgres://localhost/db"); got != "postgres://localhost/db" {
		t.Errorf("Require returned %q, want the value back", got)
	}
	if got := v.RequireFromMap(map[string]string{"host": "h"}, "host"); got != "h" {
		t.Errorf("RequireFromMap returned %q, want h", got)
	}
	if err := v.Err(); err != nil {
		t.Errorf("Err with nothing missing = %v, want nil", err)
	}
}

func TestValidatorReportsEveryMissingField(t *testing.T) {
	t.Parallel()
	v := resolver.NewValidator("prod", "aws_secrets_manager")
	v.Require("secret_name", "")
	v.RequireFromMap(map[string]string{"host": "h"}, "password") // absent key
	v.RequireFromMap(map[string]string{"user": ""}, "user")      // present but empty

	err := v.Err()
	if err == nil {
		t.Fatal("Err with missing fields = nil")
	}
	for _, want := range []string{`connection "prod"`, "aws_secrets_manager", "secret_name", "password", "user"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}
