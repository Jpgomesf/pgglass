package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/jpgomesf/mcp-postgres/internal/config"
)

func init() { RegisterSource(&AWSSecretsSource{}) }

// AWSSecretsSource resolves credentials from AWS Secrets Manager. Secrets
// are cached in memory for secretCacheTTL, unless Invalidate drops them
// first.
type AWSSecretsSource struct{}

func (AWSSecretsSource) Type() string { return "aws_secrets_manager" }

func (AWSSecretsSource) Resolve(ctx context.Context, connName string, conn config.Connection) (string, error) {
	v := NewValidator(connName, "aws_secrets_manager")
	v.Require("secret_name", conn.SecretName)
	if err := v.Err(); err != nil {
		return "", err
	}

	secret, err := awsSecrets.Get(ctx, conn.AWSProfile, conn.SecretName)
	if err != nil {
		return "", err
	}

	sv := NewValidator(connName, fmt.Sprintf("aws_secrets_manager secret %q", conn.SecretName))
	host := sv.RequireFromMap(secret, "host")
	dbName := sv.RequireFromMap(secret, "db_name")
	user := sv.RequireFromMap(secret, "user")
	password := sv.RequireFromMap(secret, "password")
	if err := sv.Err(); err != nil {
		return "", err
	}

	port := secret["port"]
	if port == "" {
		port = "5432"
	}

	// A URL escapes every value, so a password holding a space or a quote
	// can neither break the DSN nor set another of its parameters.
	dsn := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, password),
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + dbName,
	}
	return dsn.String(), nil
}

// Status returns whether the AWS secret has been cached.
func (AWSSecretsSource) Status(conn config.Connection) string {
	if conn.SecretName == "" {
		return ""
	}
	if awsSecrets.IsCached(conn.SecretName) {
		return "loaded"
	}
	return "not loaded"
}

// Invalidate drops the cached secret of conn, so that the next Resolve
// fetches it again: after a rotation, the server refuses the cached password.
func (AWSSecretsSource) Invalidate(conn config.Connection) {
	awsSecrets.Invalidate(conn.SecretName)
}

// --- AWS secrets cache ---

const secretCacheTTL = 24 * time.Hour

type secretEntry struct {
	value     map[string]string
	fetchedAt time.Time
}

type awsSecretsCache struct {
	// fetch gets a secret from AWS Secrets Manager; tests replace it.
	fetch func(ctx context.Context, awsProfile, secretName string) (map[string]string, error)

	mu    sync.RWMutex
	cache map[string]*secretEntry
}

var awsSecrets = &awsSecretsCache{
	fetch: fetchSecret,
	cache: make(map[string]*secretEntry),
}

// IsCached returns true if the secret is cached and not expired.
func (sc *awsSecretsCache) IsCached(secretName string) bool {
	_, ok := sc.cached(secretName)
	return ok
}

// Get returns the secret from the cache when it was fetched less than
// secretCacheTTL ago, and from AWS Secrets Manager otherwise.
func (sc *awsSecretsCache) Get(ctx context.Context, awsProfile, secretName string) (map[string]string, error) {
	if secret, ok := sc.cached(secretName); ok {
		return secret, nil
	}
	secret, err := sc.fetch(ctx, awsProfile, secretName)
	if err != nil {
		return nil, err
	}
	sc.mu.Lock()
	sc.cache[secretName] = &secretEntry{value: secret, fetchedAt: time.Now()}
	sc.mu.Unlock()
	return secret, nil
}

// Invalidate drops the secret from the cache, if it is there.
func (sc *awsSecretsCache) Invalidate(secretName string) {
	sc.mu.Lock()
	delete(sc.cache, secretName)
	sc.mu.Unlock()
}

// cached returns the cached secret, unless it is missing or expired.
func (sc *awsSecretsCache) cached(secretName string) (map[string]string, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	entry, ok := sc.cache[secretName]
	if !ok || time.Since(entry.fetchedAt) >= secretCacheTTL {
		return nil, false
	}
	return entry.value, true
}

// fetchSecret gets a secret from AWS Secrets Manager, with the credentials of
// awsProfile or, when it is empty, of the default credential chain.
func fetchSecret(ctx context.Context, awsProfile, secretName string) (map[string]string, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if awsProfile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(awsProfile))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config (profile %q): %w", awsProfile, err)
	}

	client := secretsmanager.NewFromConfig(cfg)
	result, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretName),
	})
	if err != nil {
		return nil, fmt.Errorf("get secret %q: %w", secretName, err)
	}
	return secretValue(secretName, result)
}

// secretValue decodes the JSON object a secret holds as its string value. A
// binary secret has none.
func secretValue(secretName string, out *secretsmanager.GetSecretValueOutput) (map[string]string, error) {
	if out.SecretString == nil {
		return nil, fmt.Errorf("secret %q has no string value: binary secrets are not supported", secretName)
	}
	var secret map[string]string
	if err := json.Unmarshal([]byte(*out.SecretString), &secret); err != nil {
		return nil, fmt.Errorf("parse secret %q: %w", secretName, err)
	}
	return secret, nil
}
