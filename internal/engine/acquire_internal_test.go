package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestAuthFailedIsTheServerRefusingCredentials(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&pgconn.PgError{Code: "28P01"}, true}, // invalid_password
		{&pgconn.PgError{Code: "28000"}, true}, // invalid_authorization_specification
		{fmt.Errorf("failed to connect: %w", &pgconn.PgError{Code: "28P01"}), true},
		{&pgconn.PgError{Code: "3D000"}, false}, // invalid_catalog_name: no such database
		{&pgconn.PgError{Code: "53300"}, false}, // too_many_connections
		{errors.New("dial tcp: connection refused"), false},
		{nil, false},
	} {
		if got := authFailed(tc.err); got != tc.want {
			t.Errorf("authFailed(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
