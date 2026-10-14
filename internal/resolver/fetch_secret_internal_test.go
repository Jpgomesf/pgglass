package resolver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jpgomesf/pgglass/internal/config"
)

type secretsManager struct {
	mu    sync.Mutex
	asked []string
}

func (s *secretsManager) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked
}

func fakeSecretsManager(t *testing.T, secrets map[string]string) *secretsManager {
	t.Helper()
	sm := &secretsManager{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ SecretId string }
		if r.Header.Get("X-Amz-Target") != "secretsmanager.GetSecretValue" || json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "unexpected request", http.StatusNotImplemented)
			return
		}
		sm.mu.Lock()
		sm.asked = append(sm.asked, in.SecretId)
		sm.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		value, ok := secrets[in.SecretId]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"__type": "ResourceNotFoundException", "message": "Secrets Manager can't find the specified secret."}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"Name": in.SecretId, "SecretString": value})
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	profiles := filepath.Join(dir, "config")
	if err := os.WriteFile(profiles, []byte("[profile staging]\nregion = eu-west-1\n"+
		"aws_access_key_id = staging-key\naws_secret_access_key = staging-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"AWS_CONFIG_FILE":                     profiles,
		"AWS_SHARED_CREDENTIALS_FILE":         filepath.Join(dir, "credentials"),
		"AWS_PROFILE":                         "",
		"AWS_DEFAULT_PROFILE":                 "",
		"AWS_REGION":                          "us-east-1",
		"AWS_ACCESS_KEY_ID":                   "test-key",
		"AWS_SECRET_ACCESS_KEY":               "test-secret",
		"AWS_SESSION_TOKEN":                   "",
		"AWS_ENDPOINT_URL":                    "",
		"AWS_ENDPOINT_URL_SECRETS_MANAGER":    srv.URL,
		"AWS_IGNORE_CONFIGURED_ENDPOINT_URLS": "",
		"AWS_EC2_METADATA_DISABLED":           "true",
	} {
		t.Setenv(k, v)
	}
	return sm
}

// fetchSecret reads the secret from Secrets Manager, with the default
// credential chain or the named profile, and parses its JSON.
func TestFetchSecretReadsSecretsManager(t *testing.T) {
	sm := fakeSecretsManager(t, map[string]string{
		"prod/db":    `{"host": "db.example.internal", "user": "ro"}`,
		"staging/db": `{"host": "staging.example.internal"}`,
		"plain":      "not json",
	})
	for _, tc := range []struct{ profile, name, want string }{
		{"", "prod/db", "db.example.internal"},
		{"staging", "staging/db", "staging.example.internal"},
	} {
		secret, err := fetchSecret(t.Context(), tc.profile, tc.name)
		if err != nil || secret["host"] != tc.want {
			t.Errorf("fetchSecret(%q, %q) = %v, %v; want host %s", tc.profile, tc.name, secret, err, tc.want)
		}
	}

	for _, tc := range []struct{ profile, name, want string }{
		{"absent-profile", "prod/db", `load AWS config (profile "absent-profile"): `},
		{"", "absent/db", `get secret "absent/db": `},
		{"", "plain", `parse secret "plain": `},
	} {
		if secret, err := fetchSecret(t.Context(), tc.profile, tc.name); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("fetchSecret(%q, %q) = %v, %v; want an error starting %q", tc.profile, tc.name, secret, err, tc.want)
		}
	}
	if got := strings.Join(sm.requests(), " "); got != "prod/db staging/db absent/db plain" {
		t.Errorf("Secrets Manager was asked for %q, want every secret but the one of the absent profile", got)
	}
}

// Resolve fetches through the cache: the first call asks Secrets Manager, the
// next ones read the cache.
func TestResolveFetchesFromSecretsManagerOnce(t *testing.T) {
	const name = "resolver_test_fetched"
	sm := fakeSecretsManager(t, map[string]string{
		name: `{"host": "db.example.internal", "db_name": "app", "user": "ro", "password": "fetched"}`,
	})
	t.Cleanup(func() { awsSecrets.Invalidate("", name) })
	conn := config.Connection{Source: "aws_secrets_manager", SecretName: name}
	for range 2 {
		if got := resolvedPassword(t, conn); got != "fetched" {
			t.Errorf("password = %q, want fetched", got)
		}
	}
	if got := sm.requests(); len(got) != 1 {
		t.Errorf("Secrets Manager was asked %d times, want once", len(got))
	}
}

// A secret that cannot be fetched is not cached: the next Resolve asks again.
func TestFailedFetchIsNotCached(t *testing.T) {
	store := fakeAWS(t, map[secretKey]map[string]string{})
	conn := config.Connection{Source: "aws_secrets_manager", SecretName: "resolver_test_unfetchable"}
	for range 2 {
		dsn, err := AWSSecretsSource{}.Resolve(t.Context(), "c", conn)
		if dsn != "" || err == nil || err.Error() != `get secret "resolver_test_unfetchable": not found` {
			t.Errorf("Resolve = %q, %v; want the fetch error", dsn, err)
		}
	}
	if store.fetches != 2 || awsSecrets.IsCached("", conn.SecretName) {
		t.Errorf("%d fetches, cached %v; want 2 and nothing cached", store.fetches, awsSecrets.IsCached("", conn.SecretName))
	}
}
