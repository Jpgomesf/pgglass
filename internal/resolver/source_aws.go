package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/jpgomesf/mcp-postgres/internal/config"
)

func init() { RegisterSource(&AWSSecretsSource{}) }

// AWSSecretsSource resolves credentials from AWS Secrets Manager.
// Secrets are cached in-memory for the lifetime of the process.
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

	dsn := fmt.Sprintf(
		"host=%s port=%s dbname=%s user=%s password=%s",
		host, port, dbName, user, password,
	)
	return dsn, nil
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

// --- AWS secrets cache ---

const secretCacheTTL = 24 * time.Hour

type secretEntry struct {
	value     map[string]string
	fetchedAt time.Time
}

type awsSecretsCache struct {
	mu    sync.RWMutex
	cache map[string]*secretEntry
}

var awsSecrets = &awsSecretsCache{
	cache: make(map[string]*secretEntry),
}

// IsCached returns true if the secret is cached and not expired.
func (sc *awsSecretsCache) IsCached(secretName string) bool {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	entry, ok := sc.cache[secretName]
	return ok && time.Since(entry.fetchedAt) < secretCacheTTL
}

// Get fetches a secret from AWS Secrets Manager, returning cached values
// if available and not older than 24 hours.
func (sc *awsSecretsCache) Get(ctx context.Context, awsProfile, secretName string) (map[string]string, error) {
	sc.mu.RLock()
	if entry, ok := sc.cache[secretName]; ok && time.Since(entry.fetchedAt) < secretCacheTTL {
		sc.mu.RUnlock()
		return entry.value, nil
	}
	sc.mu.RUnlock()

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

	var secret map[string]string
	if err := json.Unmarshal([]byte(*result.SecretString), &secret); err != nil {
		return nil, fmt.Errorf("parse secret %q: %w", secretName, err)
	}

	sc.mu.Lock()
	sc.cache[secretName] = &secretEntry{value: secret, fetchedAt: time.Now()}
	sc.mu.Unlock()

	return secret, nil
}
