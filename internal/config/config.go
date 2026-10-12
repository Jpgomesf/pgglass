package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	DefaultStatementTimeoutS = 30
	// InlineMaxBytes caps a query result returned inline; a larger one is
	// written to a spill file and summarized.
	InlineMaxBytes = 10_000

	DefaultLockTimeoutSeconds = 5
	DefaultMaxRows            = 1000
	DefaultMaxBytes           = 1 << 20 // 1 MiB
	DefaultSpillTTLHours      = 24
	DefaultFormat             = FormatTSV
	DefaultRoleCheck          = RoleCheckWarn
)

// Values of Defaults.RoleCheck: what to do when the connected role can do
// more than read (see internal/preflight).
const (
	RoleCheckStrict = "strict" // refuse to query through the connection
	RoleCheckWarn   = "warn"   // query, but warn in tool output
	RoleCheckOff    = "off"    // skip the audit
)

// Values of Defaults.Format: how query results are rendered.
const (
	FormatTSV      = "tsv"
	FormatJSON     = "json"
	FormatMarkdown = "markdown"
)

var (
	roleCheckModes = []string{RoleCheckStrict, RoleCheckWarn, RoleCheckOff}
	formats        = []string{FormatTSV, FormatJSON, FormatMarkdown}
)

// Config is the top-level configuration structure.
type Config struct {
	Defaults    Defaults              `json:"defaults"`
	Connections map[string]Connection `json:"connections"`
}

// Defaults holds settings shared by all connections. A zero value means
// "use the default": WithDefaults fills them in, and Load and Parse do so
// before validating.
type Defaults struct {
	// Deprecated: ignored. The server is always read-only.
	Readonly *bool `json:"readonly,omitempty"`

	StatementTimeoutSeconds int    `json:"statement_timeout_seconds"`
	LockTimeoutSeconds      int    `json:"lock_timeout_seconds"`
	MaxRows                 int    `json:"max_rows"`
	MaxBytes                int    `json:"max_bytes"`
	SpillTTLHours           int    `json:"spill_ttl_hours"`
	Format                  string `json:"format"`
	RoleCheck               string `json:"role_check"`
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

	// Deprecated: ignored. The server is always read-only.
	Readonly *bool `json:"readonly,omitempty"`

	// Per-connection settings
	RequiresVPN   bool   `json:"requires_vpn,omitempty"`
	DefaultSchema string `json:"default_schema,omitempty"`
}

// WithDefaults returns d with every unset (zero) setting replaced by its default.
func (d Defaults) WithDefaults() Defaults {
	setInt(&d.StatementTimeoutSeconds, DefaultStatementTimeoutS)
	setInt(&d.LockTimeoutSeconds, DefaultLockTimeoutSeconds)
	setInt(&d.MaxRows, DefaultMaxRows)
	setInt(&d.MaxBytes, DefaultMaxBytes)
	setInt(&d.SpillTTLHours, DefaultSpillTTLHours)
	if d.Format == "" {
		d.Format = DefaultFormat
	}
	if d.RoleCheck == "" {
		d.RoleCheck = DefaultRoleCheck
	}
	return d
}

func setInt(v *int, def int) {
	if *v == 0 {
		*v = def
	}
}

// Validate reports every invalid setting in d. Call it on the result of
// WithDefaults: unset values are invalid.
func (d Defaults) Validate() error {
	var errs []error
	for _, s := range []struct {
		key   string
		value int
	}{
		{"statement_timeout_seconds", d.StatementTimeoutSeconds},
		{"lock_timeout_seconds", d.LockTimeoutSeconds},
		{"max_rows", d.MaxRows},
		{"max_bytes", d.MaxBytes},
		{"spill_ttl_hours", d.SpillTTLHours},
	} {
		if s.value <= 0 {
			errs = append(errs, fmt.Errorf("defaults.%s must be positive, got %d", s.key, s.value))
		}
	}
	errs = append(errs, oneOf("format", d.Format, formats), oneOf("role_check", d.RoleCheck, roleCheckModes))
	return errors.Join(errs...)
}

func oneOf(key, value string, allowed []string) error {
	if slices.Contains(allowed, value) {
		return nil
	}
	return fmt.Errorf("defaults.%s must be one of %s, got %q", key, strings.Join(allowed, ", "), value)
}

// UsesReadonly reports whether the deprecated readonly key is set, in
// defaults or in any connection.
func (cfg Config) UsesReadonly() bool {
	if cfg.Defaults.Readonly != nil {
		return true
	}
	for _, conn := range cfg.Connections {
		if conn.Readonly != nil {
			return true
		}
	}
	return false
}

// Parse decodes a config.json document, fills unset defaults and validates them.
func Parse(data []byte) (Config, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config.json: %w", err)
	}
	cfg.Defaults = cfg.Defaults.WithDefaults()
	if err := cfg.Defaults.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config.json: %w", err)
	}
	return cfg, nil
}

// Load reads config.json from the same directory as the executable,
// falling back to the current working directory, and parses it with Parse.
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

	return Parse(data)
}
