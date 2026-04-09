package resolver

import (
	"context"
	"fmt"
	"os"

	"github.com/jpgomesf/mcp-postgres/internal/config"
)

func init() { RegisterSource(&EnvSource{}) }

// EnvSource resolves credentials from an environment variable.
type EnvSource struct{}

func (EnvSource) Type() string { return "env" }

func (EnvSource) Resolve(_ context.Context, connName string, conn config.Connection) (string, error) {
	v := NewValidator(connName, "env")
	envVar := v.Require("dsn_env_var", conn.DSNEnvVar)
	if err := v.Err(); err != nil {
		return "", err
	}
	dsn := os.Getenv(envVar)
	if dsn == "" {
		return "", fmt.Errorf("connection %q (env): environment variable %q is not set", connName, envVar)
	}
	return dsn, nil
}

func (EnvSource) Status(_ config.Connection) string { return "" }
