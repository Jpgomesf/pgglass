package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	DefaultStatementTimeoutS = 30
	DefaultConnectionTTL     = 5 * time.Minute
	InlineMaxChars           = 10_000
)

// BlockedSQL contains SQL keywords that are never allowed.
var BlockedSQL = map[string]bool{
	"INSERT":   true,
	"UPDATE":   true,
	"DELETE":   true,
	"DROP":     true,
	"ALTER":    true,
	"TRUNCATE": true,
	"CREATE":   true,
}

// SystemSchemas are excluded from schema discovery.
var SystemSchemas = map[string]bool{
	"pg_catalog":         true,
	"information_schema": true,
	"pg_toast":           true,
}

// Config is the top-level configuration structure.
type Config struct {
	Defaults    Defaults              `json:"defaults"`
	Connections map[string]Connection `json:"connections"`
}

// Defaults holds shared settings applied to all connections unless overridden.
type Defaults struct {
	Readonly                bool `json:"readonly"`
	StatementTimeoutSeconds int  `json:"statement_timeout_seconds"`
}

// Connection represents a named database connection with its credential source.
type Connection struct {
	// Source type: "direct", "env", "dotenv", "aws_secrets_manager"
	Source string `json:"source"`

	// Direct source
	DSN string `json:"dsn,omitempty"`

	// Env / Dotenv source
	DSNEnvVar  string `json:"dsn_env_var,omitempty"`
	DotenvPath string `json:"dotenv_path,omitempty"`

	// AWS Secrets Manager source
	AWSProfile string `json:"aws_profile,omitempty"`
	SecretName string `json:"secret_name,omitempty"`

	// Per-connection settings
	Readonly      *bool  `json:"readonly,omitempty"`
	RequiresVPN   bool   `json:"requires_vpn,omitempty"`
	DefaultSchema string `json:"default_schema,omitempty"`
}

// IsReadonly returns the effective readonly setting for this connection.
func (c Connection) IsReadonly(defaults Defaults) bool {
	if c.Readonly != nil {
		return *c.Readonly
	}
	return defaults.Readonly
}

// StatementTimeoutMS returns the statement timeout in milliseconds.
func (cfg Config) StatementTimeoutMS() int {
	if cfg.Defaults.StatementTimeoutSeconds > 0 {
		return cfg.Defaults.StatementTimeoutSeconds * 1000
	}
	return DefaultStatementTimeoutS * 1000
}

// ConnNames returns the list of configured connection names.
func (cfg Config) ConnNames() []string {
	names := make([]string, 0, len(cfg.Connections))
	for k := range cfg.Connections {
		names = append(names, k)
	}
	return names
}

// Load reads config.json from the same directory as the executable,
// falling back to the current working directory.
func Load() (Config, error) {
	exe, err := os.Executable()
	if err != nil {
		return Config{}, fmt.Errorf("resolve executable path: %w", err)
	}
	configPath := filepath.Join(filepath.Dir(exe), "config.json")

	data, err := os.ReadFile(configPath) //nolint:gosec // path derived from executable location
	if err != nil {
		data, err = os.ReadFile("config.json")
		if err != nil {
			return Config{}, fmt.Errorf("read config.json: %w", err)
		}
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config.json: %w", err)
	}

	return cfg, nil
}
