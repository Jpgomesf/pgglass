package resolver_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jpgomesf/mcp-postgres/internal/config"
	"github.com/jpgomesf/mcp-postgres/internal/resolver"
)

// writeDotenv writes contents to a temp .env file and returns its path.
func writeDotenv(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseDotenv(t *testing.T) {
	t.Parallel()
	path := writeDotenv(t, `# a comment
DSN=postgres://localhost/db

  QUOTED="has spaces"
SINGLE='single quoted'
TRIMMED =  value_with_outer_spaces
NO_EQUALS_LINE
EMPTY=
`)
	vars, err := resolver.ParseDotenv(path)
	if err != nil {
		t.Fatalf("ParseDotenv: %v", err)
	}
	want := map[string]string{
		"DSN":     "postgres://localhost/db",
		"QUOTED":  "has spaces",
		"SINGLE":  "single quoted",
		"TRIMMED": "value_with_outer_spaces",
		"EMPTY":   "",
	}
	for k, w := range want {
		if got, ok := vars[k]; !ok || got != w {
			t.Errorf("vars[%q] = %q (present %v), want %q", k, got, ok, w)
		}
	}
	if _, ok := vars["NO_EQUALS_LINE"]; ok {
		t.Errorf("a line without '=' became a variable: %v", vars)
	}
	if _, ok := vars["# a comment"]; ok {
		t.Errorf("a comment line became a variable: %v", vars)
	}
}

func TestParseDotenvMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := resolver.ParseDotenv(filepath.Join(t.TempDir(), "absent.env")); err == nil {
		t.Fatal("ParseDotenv of a missing file returned no error")
	}
}

func TestDirectSource(t *testing.T) {
	t.Parallel()
	var s resolver.DirectSource
	dsn, err := s.Resolve(t.Context(), "c", config.Connection{DSN: "postgres://localhost/db"})
	if err != nil || dsn != "postgres://localhost/db" {
		t.Errorf("Resolve = %q, %v", dsn, err)
	}
	if _, err := s.Resolve(t.Context(), "c", config.Connection{}); err == nil ||
		!strings.Contains(err.Error(), "missing required fields: dsn") {
		t.Errorf("Resolve without a DSN: err = %v", err)
	}
}

func TestEnvSource(t *testing.T) {
	var s resolver.EnvSource
	t.Setenv("RESOLVER_TEST_DSN", "postgres://localhost/envdb")
	dsn, err := s.Resolve(t.Context(), "c", config.Connection{DSNEnvVar: "RESOLVER_TEST_DSN"})
	if err != nil || dsn != "postgres://localhost/envdb" {
		t.Errorf("Resolve = %q, %v", dsn, err)
	}

	if _, err := s.Resolve(t.Context(), "c", config.Connection{}); err == nil ||
		!strings.Contains(err.Error(), "missing required fields: dsn_env_var") {
		t.Errorf("Resolve without dsn_env_var: err = %v", err)
	}
	if _, err := s.Resolve(t.Context(), "c", config.Connection{DSNEnvVar: "RESOLVER_TEST_UNSET"}); err == nil ||
		!strings.Contains(err.Error(), "is not set") {
		t.Errorf("Resolve of an unset variable: err = %v", err)
	}
}

func TestDotenvSource(t *testing.T) {
	t.Parallel()
	var s resolver.DotenvSource
	path := writeDotenv(t, "APP_DSN=postgres://localhost/dotenvdb\n")
	dsn, err := s.Resolve(t.Context(), "c", config.Connection{DSNEnvVar: "APP_DSN", DotenvPath: path})
	if err != nil || dsn != "postgres://localhost/dotenvdb" {
		t.Errorf("Resolve = %q, %v", dsn, err)
	}

	if _, err := s.Resolve(t.Context(), "c", config.Connection{DotenvPath: path}); err == nil ||
		!strings.Contains(err.Error(), "missing required fields: dsn_env_var") {
		t.Errorf("Resolve without dsn_env_var: err = %v", err)
	}
	if _, err := s.Resolve(t.Context(), "c", config.Connection{DSNEnvVar: "ABSENT", DotenvPath: path}); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("Resolve of an absent key: err = %v", err)
	}
	if _, err := s.Resolve(t.Context(), "c", config.Connection{DSNEnvVar: "X", DotenvPath: filepath.Join(t.TempDir(), "absent.env")}); err == nil {
		t.Errorf("Resolve with a missing dotenv file returned no error")
	}
}

// TestDotenvSourceDefaultPath covers the dotenv_path default of ".env" in the
// working directory. It changes directory, so it is not parallel.
func TestDotenvSourceDefaultPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("DEFAULT_DSN=postgres://localhost/defaultdb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	dsn, err := resolver.DotenvSource{}.Resolve(t.Context(), "c", config.Connection{DSNEnvVar: "DEFAULT_DSN"})
	if err != nil || dsn != "postgres://localhost/defaultdb" {
		t.Errorf("Resolve with the default path = %q, %v", dsn, err)
	}
}

func TestResolveDispatch(t *testing.T) {
	t.Parallel()
	dsn, err := resolver.Resolve(t.Context(), "c", config.Connection{Source: "direct", DSN: "postgres://localhost/db"})
	if err != nil || dsn != "postgres://localhost/db" {
		t.Errorf("Resolve(direct) = %q, %v", dsn, err)
	}

	_, err = resolver.Resolve(t.Context(), "c", config.Connection{Source: "nope"})
	if err == nil || !strings.Contains(err.Error(), `unknown source "nope"`) {
		t.Fatalf("Resolve of an unknown source: err = %v", err)
	}
	// The error lists the registered sources so the user can correct it.
	for _, want := range []string{"direct", "env", "dotenv", "aws_secrets_manager"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("unknown-source error %q does not list %q", err, want)
		}
	}
}

// TestRegisterSource writes the source registry, which RegisterSource does not
// lock because sources register in init. It is not parallel, so it ends before
// the parallel tests that read the registry resume.
func TestRegisterSource(t *testing.T) {
	resolver.RegisterSource(fakeSource{})
	if !resolver.IsRegistered("resolver_test_fake") {
		t.Fatal("RegisterSource did not register the source")
	}
	dsn, err := resolver.Resolve(t.Context(), "c", config.Connection{Source: "resolver_test_fake"})
	if err != nil || dsn != "fake-dsn" {
		t.Errorf("Resolve(fake) = %q, %v", dsn, err)
	}
	if got := resolver.StatusFor(config.Connection{Source: "resolver_test_fake"}); got != "fake-status" {
		t.Errorf("StatusFor(fake) = %q, want fake-status", got)
	}
}

// Only aws_secrets_manager has a status: pg_list_connections shows none for
// the other sources, nor for an unknown one.
func TestStatusForUnknownAndStatusless(t *testing.T) {
	t.Parallel()
	for _, conn := range []config.Connection{
		{Source: "nope"},
		{Source: "direct", DSN: "x"},
		{Source: "env", DSNEnvVar: "RESOLVER_TEST_UNSET"},
		{Source: "dotenv", DSNEnvVar: "APP_DSN"},
	} {
		if got := resolver.StatusFor(conn); got != "" {
			t.Errorf("StatusFor(%s) = %q, want empty", conn.Source, got)
		}
	}
}

// Only a source that caches credentials can return others once invalidated.
func TestInvalidateReportsWhetherTheSourceCaches(t *testing.T) {
	t.Parallel()
	for source, want := range map[string]bool{"aws_secrets_manager": true, "direct": false, "env": false, "dotenv": false, "nope": false} {
		conn := config.Connection{Source: source, SecretName: "resolver_test_invalidate"}
		if got := resolver.Invalidate(conn); got != want {
			t.Errorf("Invalidate(%s) = %v, want %v", source, got, want)
		}
	}
}

type fakeSource struct{}

func (fakeSource) Type() string { return "resolver_test_fake" }
func (fakeSource) Resolve(context.Context, string, config.Connection) (string, error) {
	return "fake-dsn", nil
}
func (fakeSource) Status(config.Connection) string { return "fake-status" }
