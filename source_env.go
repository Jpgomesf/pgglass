package main

import (
	"context"
	"fmt"
	"os"
)

func init() { RegisterSource(&EnvSource{}) }

// EnvSource resolves credentials from an environment variable.
type EnvSource struct{}

func (EnvSource) Type() string { return "env" }

func (EnvSource) Resolve(_ context.Context, connName string, conn Connection) (string, error) {
	v := newValidator(connName, "env")
	envVar := v.require("dsn_env_var", conn.DSNEnvVar)
	if err := v.err(); err != nil {
		return "", err
	}
	dsn := os.Getenv(envVar)
	if dsn == "" {
		return "", fmt.Errorf("connection %q (env): environment variable %q is not set", connName, envVar)
	}
	return dsn, nil
}
