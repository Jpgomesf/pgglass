package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`{"connections": {"db": {"source": "direct", "dsn": "postgres://x"}}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Defaults{
		StatementTimeoutSeconds: 30,
		LockTimeoutSeconds:      5,
		MaxRows:                 1000,
		MaxBytes:                1 << 20,
		SpillTTLHours:           24,
		Format:                  "tsv",
		RoleCheck:               "warn",
	}
	if cfg.Defaults != want {
		t.Errorf("defaults = %+v, want %+v", cfg.Defaults, want)
	}
	if got := cfg.Connections["db"].DSN; got != "postgres://x" {
		t.Errorf("connection dsn = %q", got)
	}
}

func TestParseKeepsExplicitValues(t *testing.T) {
	cfg, err := Parse([]byte(`{"defaults": {
		"statement_timeout_seconds": 60,
		"lock_timeout_seconds": 2,
		"max_rows": 50,
		"max_bytes": 4096,
		"spill_ttl_hours": 1,
		"format": "markdown",
		"role_check": "strict"
	}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Defaults{
		StatementTimeoutSeconds: 60,
		LockTimeoutSeconds:      2,
		MaxRows:                 50,
		MaxBytes:                4096,
		SpillTTLHours:           1,
		Format:                  "markdown",
		RoleCheck:               "strict",
	}
	if cfg.Defaults != want {
		t.Errorf("defaults = %+v, want %+v", cfg.Defaults, want)
	}
}

func TestParseAcceptsEveryFormatAndRoleCheck(t *testing.T) {
	for _, format := range []string{"tsv", "json", "markdown"} {
		for _, mode := range []string{"strict", "warn", "off"} {
			doc := `{"defaults": {"format": "` + format + `", "role_check": "` + mode + `"}}`
			if _, err := Parse([]byte(doc)); err != nil {
				t.Errorf("format %q, role_check %q: %v", format, mode, err)
			}
		}
	}
}

func TestParseRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name     string
		defaults string
		wantErr  string
	}{
		{"negative statement timeout", `"statement_timeout_seconds": -1`, "defaults.statement_timeout_seconds must be positive, got -1"},
		{"negative lock timeout", `"lock_timeout_seconds": -5`, "defaults.lock_timeout_seconds must be positive, got -5"},
		{"negative max rows", `"max_rows": -1`, "defaults.max_rows must be positive, got -1"},
		{"negative max bytes", `"max_bytes": -1024`, "defaults.max_bytes must be positive, got -1024"},
		{"negative spill ttl", `"spill_ttl_hours": -24`, "defaults.spill_ttl_hours must be positive, got -24"},
		{"unknown format", `"format": "xml"`, `defaults.format must be one of tsv, json, markdown, got "xml"`},
		{"csv is not a config format", `"format": "csv"`, `defaults.format must be one of tsv, json, markdown, got "csv"`},
		{"format is case sensitive", `"format": "TSV"`, `defaults.format must be one of tsv, json, markdown, got "TSV"`},
		{"unknown role check", `"role_check": "maybe"`, `defaults.role_check must be one of strict, warn, off, got "maybe"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(`{"defaults": {` + tt.defaults + `}}`))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseReportsEveryInvalidSetting(t *testing.T) {
	_, err := Parse([]byte(`{"defaults": {"max_rows": -1, "format": "xml", "role_check": "maybe"}}`))
	if err == nil {
		t.Fatal("Parse succeeded, want error")
	}
	for _, key := range []string{"max_rows", "format", "role_check"} {
		if !strings.Contains(err.Error(), "defaults."+key) {
			t.Errorf("error %q does not mention %s", err, key)
		}
	}
}

func TestParseRejectsMalformedJSON(t *testing.T) {
	_, err := Parse([]byte(`{"defaults": `))
	if err == nil || !strings.Contains(err.Error(), "parse config.json") {
		t.Errorf("error = %v, want a parse error", err)
	}
}

func TestParseAcceptsAndReportsDeprecatedReadonly(t *testing.T) {
	for _, tc := range []struct {
		doc  string
		uses bool
	}{
		{`{"connections": {"db": {"source": "direct"}}}`, false},
		{`{"defaults": {"readonly": false}}`, true},
		{`{"defaults": {"readonly": true}}`, true},
		{`{"connections": {"a": {"source": "direct"}, "b": {"source": "direct", "readonly": false}}}`, true},
	} {
		cfg, err := Parse([]byte(tc.doc))
		if err != nil {
			t.Fatalf("Parse(%s): %v", tc.doc, err)
		}
		if got := cfg.UsesReadonly(); got != tc.uses {
			t.Errorf("UsesReadonly() for %s = %v, want %v", tc.doc, got, tc.uses)
		}
	}
}

func TestWithDefaultsKeepsSetValuesAndIsIdempotent(t *testing.T) {
	d := Defaults{MaxRows: 7, Format: "json"}.WithDefaults()
	if d.MaxRows != 7 || d.Format != "json" {
		t.Errorf("explicit values overwritten: %+v", d)
	}
	if d.MaxBytes != DefaultMaxBytes || d.RoleCheck != DefaultRoleCheck {
		t.Errorf("unset values not defaulted: %+v", d)
	}
	if again := d.WithDefaults(); again != d {
		t.Errorf("WithDefaults not idempotent: %+v != %+v", again, d)
	}
}

func TestValidateRejectsUnsetValues(t *testing.T) {
	if err := (Defaults{}).Validate(); err == nil {
		t.Error("Validate of zero Defaults succeeded, want error")
	}
	if err := (Defaults{}).WithDefaults().Validate(); err != nil {
		t.Errorf("Validate after WithDefaults: %v", err)
	}
}

func TestLoadReadsConfigFromWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	doc := `{"defaults": {"max_rows": 10}, "connections": {"db": {"source": "env", "dsn_env_var": "X"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Defaults.MaxRows != 10 || cfg.Defaults.MaxBytes != DefaultMaxBytes {
		t.Errorf("defaults = %+v", cfg.Defaults)
	}
	if cfg.Connections["db"].DSNEnvVar != "X" {
		t.Errorf("connections = %+v", cfg.Connections)
	}
}

func TestLoadValidates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"defaults": {"format": "xml"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "defaults.format") {
		t.Errorf("Load error = %v, want a format validation error", err)
	}
}

func TestLoadFailsWithoutConfigFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "read config.json") {
		t.Errorf("Load error = %v, want a read error", err)
	}
}

func TestSampleConfigParses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("sample config.json: %v", err)
	}
	if len(cfg.Connections) == 0 {
		t.Error("sample config.json has no connections")
	}
}
