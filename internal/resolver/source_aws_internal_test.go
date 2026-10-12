package resolver

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jpgomesf/mcp-postgres/internal/config"
)

// primeSecret puts a secret in the shared cache with the given age, and
// removes it when the test ends. Each test uses a unique name so the shared
// cache never crosses tests.
func primeSecret(t *testing.T, name string, value map[string]string, age time.Duration) {
	t.Helper()
	awsSecrets.mu.Lock()
	awsSecrets.cache[name] = &secretEntry{value: value, fetchedAt: time.Now().Add(-age)}
	awsSecrets.mu.Unlock()
	t.Cleanup(func() {
		awsSecrets.mu.Lock()
		delete(awsSecrets.cache, name)
		awsSecrets.mu.Unlock()
	})
}

func TestAWSSecretsCacheIsCached(t *testing.T) {
	if awsSecrets.IsCached("resolver_test_absent") {
		t.Error("IsCached of an absent secret = true")
	}
	primeSecret(t, "resolver_test_fresh", map[string]string{"host": "h"}, time.Minute)
	if !awsSecrets.IsCached("resolver_test_fresh") {
		t.Error("IsCached of a fresh secret = false")
	}
	primeSecret(t, "resolver_test_stale", map[string]string{"host": "h"}, secretCacheTTL+time.Hour)
	if awsSecrets.IsCached("resolver_test_stale") {
		t.Error("IsCached of an expired secret = true")
	}
}

func TestAWSSecretsCacheGetReturnsCachedValue(t *testing.T) {
	want := map[string]string{"host": "h", "user": "u"}
	primeSecret(t, "resolver_test_get", want, time.Minute)

	// A fresh entry is returned without contacting AWS (no profile needed).
	got, err := awsSecrets.Get(t.Context(), "", "resolver_test_get")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got["host"] != "h" || got["user"] != "u" {
		t.Errorf("Get = %v, want %v", got, want)
	}
}

// resolveSecret resolves a connection whose cached secret is secret, and
// parses the DSN as pgx does.
func resolveSecret(t *testing.T, name string, secret map[string]string) *pgconn.Config {
	t.Helper()
	primeSecret(t, name, secret, time.Minute)
	dsn, err := AWSSecretsSource{}.Resolve(t.Context(), "c", config.Connection{SecretName: name})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("the DSN does not parse: %v", err)
	}
	return cfg
}

func TestAWSSecretsSourceResolveFromCache(t *testing.T) {
	for i, password := range []string{
		"secret",
		"has space",
		`it's`,
		`back\slash`,
		"x dbname=other host=elsewhere", // must not set other parameters
		"p@ss:w/rd?#%",
	} {
		got := resolveSecret(t, fmt.Sprintf("resolver_test_full_%d", i), map[string]string{
			"host": "db.example.internal", "port": "6543", "db_name": "app db", "user": "ro", "password": password,
		})
		if got.Host != "db.example.internal" || got.Port != 6543 || got.Database != "app db" || got.User != "ro" || got.Password != password {
			t.Errorf("password %q: got host %s, port %d, database %q, user %s, password %q",
				password, got.Host, got.Port, got.Database, got.User, got.Password)
		}
	}

	// Port defaults to 5432 when the secret omits it; an IPv6 host keeps its colons.
	got := resolveSecret(t, "resolver_test_noport", map[string]string{"host": "::1", "db_name": "d", "user": "u", "password": "p"})
	if got.Host != "::1" || got.Port != 5432 {
		t.Errorf("without a port: host %s, port %d; want ::1 and 5432", got.Host, got.Port)
	}
}

func TestAWSSecretsSourceResolveValidates(t *testing.T) {
	var s AWSSecretsSource
	// No secret_name: rejected before any AWS call.
	if _, err := s.Resolve(t.Context(), "c", config.Connection{}); err == nil ||
		!strings.Contains(err.Error(), "missing required fields: secret_name") {
		t.Errorf("Resolve without secret_name: err = %v", err)
	}

	// A cached secret missing required fields is reported, not used.
	primeSecret(t, "resolver_test_partial", map[string]string{"host": "h"}, time.Minute)
	_, err := s.Resolve(t.Context(), "c", config.Connection{SecretName: "resolver_test_partial"})
	if err == nil || !strings.Contains(err.Error(), "missing required fields") {
		t.Fatalf("Resolve of a partial secret: err = %v", err)
	}
	for _, field := range []string{"db_name", "user", "password"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error %q does not name the missing field %q", err, field)
		}
	}
}

func TestAWSSecretsSourceStatus(t *testing.T) {
	var s AWSSecretsSource
	if got := s.Status(config.Connection{}); got != "" {
		t.Errorf("Status without a secret = %q, want empty", got)
	}
	if got := s.Status(config.Connection{SecretName: "resolver_test_status_absent"}); got != "not loaded" {
		t.Errorf("Status of an uncached secret = %q, want not loaded", got)
	}
	primeSecret(t, "resolver_test_status_loaded", map[string]string{"host": "h"}, time.Minute)
	if got := s.Status(config.Connection{SecretName: "resolver_test_status_loaded"}); got != "loaded" {
		t.Errorf("Status of a cached secret = %q, want loaded", got)
	}
}
