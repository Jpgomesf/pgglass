package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"slices"
	"strings"
	"time"
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

	// MaxTimeoutSeconds bounds the timeouts: Postgres takes them as an int
	// of milliseconds.
	MaxTimeoutSeconds = math.MaxInt32 / 1000
	// MaxSpillTTLHours bounds spill_ttl_hours, which becomes a time.Duration.
	MaxSpillTTLHours = int(math.MaxInt64 / int64(time.Hour))

	// MaxNameBytes is the longest name Postgres keeps (NAMEDATALEN - 1): it
	// cuts a longer one. Connection names take at most as many, since the
	// tools repeat them in output that must stay within InlineMaxBytes.
	MaxNameBytes = 63
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
	// Formats are the result formats a config and a pg_query call may name.
	Formats = []string{FormatTSV, FormatJSON, FormatMarkdown}
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
		key        string
		value, max int
	}{
		{"statement_timeout_seconds", d.StatementTimeoutSeconds, MaxTimeoutSeconds},
		{"lock_timeout_seconds", d.LockTimeoutSeconds, MaxTimeoutSeconds},
		{"max_rows", d.MaxRows, math.MaxInt},
		{"max_bytes", d.MaxBytes, math.MaxInt},
		{"spill_ttl_hours", d.SpillTTLHours, MaxSpillTTLHours},
	} {
		switch {
		case s.value <= 0:
			errs = append(errs, fmt.Errorf("defaults.%s must be positive, got %d", s.key, s.value))
		case s.value > s.max:
			errs = append(errs, fmt.Errorf("defaults.%s must be at most %d, got %d", s.key, s.max, s.value))
		}
	}
	errs = append(errs, oneOf("format", d.Format, Formats), oneOf("role_check", d.RoleCheck, roleCheckModes))
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
// An unknown key is an error, so a misspelled setting such as "role_chek"
// fails loudly instead of leaving its default in place.
func Parse(data []byte) (Config, error) {
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config.json: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Config{}, errors.New("parse config.json: unexpected data after the top-level object")
	}
	cfg.Defaults = cfg.Defaults.WithDefaults()
	if err := errors.Join(cfg.Defaults.Validate(), checkNames(cfg.Connections)); err != nil {
		return Config{}, fmt.Errorf("invalid config.json: %w", err)
	}
	return cfg, nil
}

// checkNames reports every connection name longer than MaxNameBytes.
func checkNames(conns map[string]Connection) error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(conns)) {
		if len(name) > MaxNameBytes {
			errs = append(errs, fmt.Errorf("connection name %q is %d bytes: connection names take at most %d, as Postgres names do",
				name, len(name), MaxNameBytes))
		}
	}
	return errors.Join(errs...)
}

// Load reads the config file at path and parses it with Parse.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}
