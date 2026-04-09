package resolver

import (
	"context"

	"github.com/jpgomesf/mcp-postgres/internal/config"
)

func init() { RegisterSource(&DirectSource{}) }

// DirectSource resolves credentials from an inline DSN string.
type DirectSource struct{}

func (DirectSource) Type() string { return "direct" }

func (DirectSource) Resolve(_ context.Context, connName string, conn config.Connection) (string, error) {
	v := NewValidator(connName, "direct")
	v.Require("dsn", conn.DSN)
	if err := v.Err(); err != nil {
		return "", err
	}
	return conn.DSN, nil
}

func (DirectSource) Status(_ config.Connection) string { return "" }
