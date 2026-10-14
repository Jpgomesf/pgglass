package resolver

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jpgomesf/pgglass/internal/config"
)

func primeSecret(t *testing.T, name string, value map[string]string, age time.Duration) {
	t.Helper()
	awsSecrets.mu.Lock()
	awsSecrets.cache[secretKey{name: name}] = &secretEntry{value: value, fetchedAt: time.Now().Add(-age)}
	awsSecrets.mu.Unlock()
	t.Cleanup(func() { awsSecrets.Invalidate("", name) })
}

type fakeSecrets struct {
	secrets map[secretKey]map[string]string
	fetches int
}

func (f *fakeSecrets) fetch(_ context.Context, awsProfile, secretName string) (map[string]string, error) {
	f.fetches++
	secret, ok := f.secrets[secretKey{awsProfile, secretName}]
	if !ok {
		return nil, fmt.Errorf("get secret %q: not found", secretName)
	}
	return maps.Clone(secret), nil
}

func fakeAWS(t *testing.T, secrets map[secretKey]map[string]string) *fakeSecrets {
	t.Helper()
	f := &fakeSecrets{secrets: secrets}
	saved := awsSecrets.fetch
	awsSecrets.fetch = f.fetch
	t.Cleanup(func() {
		awsSecrets.fetch = saved
		for key := range secrets {
			awsSecrets.Invalidate(key.profile, key.name)
		}
	})
	return f
}

func dbSecret(password string) map[string]string {
	return map[string]string{"host": "db.example.internal", "db_name": "app", "user": "ro", "password": password}
}

func resolvedPassword(t *testing.T, conn config.Connection) string {
	t.Helper()
	dsn, err := AWSSecretsSource{}.Resolve(t.Context(), "c", conn)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("the DSN does not parse: %v", err)
	}
	return cfg.Password
}

func TestAWSSecretsCacheIsCached(t *testing.T) {
	if awsSecrets.IsCached("", "resolver_test_absent") {
		t.Error("IsCached of an absent secret = true")
	}
	primeSecret(t, "resolver_test_fresh", map[string]string{"host": "h"}, time.Minute)
	if !awsSecrets.IsCached("", "resolver_test_fresh") {
		t.Error("IsCached of a fresh secret = false")
	}
	primeSecret(t, "resolver_test_stale", map[string]string{"host": "h"}, secretCacheTTL+time.Hour)
	if awsSecrets.IsCached("", "resolver_test_stale") {
		t.Error("IsCached of an expired secret = true")
	}
}

// After a rotation, the cached secret still answers, with the old password,
// until the engine invalidates it when the server refuses that password.
func TestAWSSecretsSourceFetchesAgainOnceInvalidated(t *testing.T) {
	key := secretKey{name: "resolver_test_rotated"}
	store := fakeAWS(t, map[secretKey]map[string]string{key: dbSecret("first")})
	conn := config.Connection{Source: "aws_secrets_manager", SecretName: key.name}

	if got := resolvedPassword(t, conn); got != "first" {
		t.Fatalf("password = %q, want first", got)
	}
	store.secrets[key] = dbSecret("second")
	if got := resolvedPassword(t, conn); got != "first" {
		t.Errorf("before Invalidate: password = %q, want the cached first", got)
	}
	if !Invalidate(conn) {
		t.Error("Invalidate = false, want true: the source caches")
	}
	if got := resolvedPassword(t, conn); got != "second" {
		t.Errorf("after Invalidate: password = %q, want second", got)
	}
	if store.fetches != 2 {
		t.Errorf("%d fetches, want 2", store.fetches)
	}
}

// One secret name can hold different secrets under different AWS profiles:
// each is cached, reported and invalidated on its own.
func TestAWSSecretsCacheKeysByProfileAndName(t *testing.T) {
	const name = "resolver_test_profiles"
	profiles := []string{"", "staging", "prod"}
	secrets := map[secretKey]map[string]string{}
	for _, profile := range profiles {
		secrets[secretKey{profile, name}] = dbSecret("pw-" + profile)
	}
	store := fakeAWS(t, secrets)
	conn := func(profile string) config.Connection {
		return config.Connection{Source: "aws_secrets_manager", AWSProfile: profile, SecretName: name}
	}

	for round := range 2 {
		for _, profile := range profiles {
			if got := resolvedPassword(t, conn(profile)); got != "pw-"+profile {
				t.Errorf("round %d, profile %q: password = %q, want pw-%s", round, profile, got, profile)
			}
		}
	}
	if store.fetches != len(profiles) {
		t.Errorf("%d fetches, want %d", store.fetches, len(profiles))
	}
	if got := (AWSSecretsSource{}).Status(conn("other")); got != "not loaded" {
		t.Errorf("Status under another profile = %q, want not loaded", got)
	}

	Invalidate(conn("staging"))
	for profile, want := range map[string]bool{"": true, "staging": false, "prod": true} {
		if got := awsSecrets.IsCached(profile, name); got != want {
			t.Errorf("after invalidating staging: IsCached(%q) = %v, want %v", profile, got, want)
		}
	}
}

func TestAWSSecretsCacheGetReturnsCachedValue(t *testing.T) {
	want := map[string]string{"host": "h", "user": "u"}
	primeSecret(t, "resolver_test_get", want, time.Minute)

	got, err := awsSecrets.Get(t.Context(), "", "resolver_test_get")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got["host"] != "h" || got["user"] != "u" {
		t.Errorf("Get = %v, want %v", got, want)
	}
}

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
		"x dbname=other host=elsewhere",
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

	got := resolveSecret(t, "resolver_test_noport", map[string]string{"host": "::1", "db_name": "d", "user": "u", "password": "p"})
	if got.Host != "::1" || got.Port != 5432 {
		t.Errorf("without a port: host %s, port %d; want ::1 and 5432", got.Host, got.Port)
	}
}

func TestSecretValue(t *testing.T) {
	got, err := secretValue("s", &secretsmanager.GetSecretValueOutput{SecretString: aws.String(`{"host": "h", "user": "u"}`)})
	if err != nil || got["host"] != "h" || got["user"] != "u" {
		t.Errorf("secretValue of a JSON string = %v, %v", got, err)
	}

	_, err = secretValue("bin", &secretsmanager.GetSecretValueOutput{SecretBinary: []byte{1, 2, 3}})
	if err == nil || !strings.Contains(err.Error(), "binary secrets are not supported") {
		t.Errorf("secretValue of a binary secret: err = %v", err)
	}

	_, err = secretValue("bad", &secretsmanager.GetSecretValueOutput{SecretString: aws.String("not json")})
	if err == nil || !strings.Contains(err.Error(), `parse secret "bad"`) {
		t.Errorf("secretValue of a non-JSON string: err = %v", err)
	}
}

func TestAWSSecretsSourceResolveValidates(t *testing.T) {
	var s AWSSecretsSource
	if _, err := s.Resolve(t.Context(), "c", config.Connection{}); err == nil ||
		!strings.Contains(err.Error(), "missing required fields: secret_name") {
		t.Errorf("Resolve without secret_name: err = %v", err)
	}

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
