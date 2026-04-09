package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
)

func init() { RegisterSource(&DotenvSource{}) }

// DotenvSource resolves credentials from a .env file.
type DotenvSource struct{}

func (DotenvSource) Type() string { return "dotenv" }

func (DotenvSource) Resolve(_ context.Context, connName string, conn Connection) (string, error) {
	v := newValidator(connName, "dotenv")
	v.require("dsn_env_var", conn.DSNEnvVar)
	if err := v.err(); err != nil {
		return "", err
	}

	dotenvPath := conn.DotenvPath
	if dotenvPath == "" {
		dotenvPath = ".env"
	}

	vars, err := parseDotenv(dotenvPath)
	if err != nil {
		return "", fmt.Errorf("connection %q (dotenv): %w", connName, err)
	}

	dsn, ok := vars[conn.DSNEnvVar]
	if !ok || dsn == "" {
		return "", fmt.Errorf("connection %q (dotenv): %q not found in %s", connName, conn.DSNEnvVar, dotenvPath)
	}
	return dsn, nil
}

// parseDotenv reads a .env file into a map. Supports KEY=VALUE, quoted values,
// comments (#), and blank lines. No variable expansion.
func parseDotenv(path string) (map[string]string, error) {
	f, err := os.Open(path) //nolint:gosec // user-configured path
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vars := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		vars[key] = value
	}
	return vars, scanner.Err()
}
