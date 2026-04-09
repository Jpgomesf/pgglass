package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

func init() { RegisterSource(&AWSSecretsSource{}) }

// AWSSecretsSource resolves credentials from AWS Secrets Manager.
// Secrets are cached in-memory for the lifetime of the process.
type AWSSecretsSource struct{}

func (AWSSecretsSource) Type() string { return "aws_secrets_manager" }

func (AWSSecretsSource) Resolve(ctx context.Context, connName string, conn Connection) (string, error) {
	v := newValidator(connName, "aws_secrets_manager")
	v.require("secret_name", conn.SecretName)
	if err := v.err(); err != nil {
		return "", err
	}

	secret, err := awsSecrets.Get(ctx, conn.AWSProfile, conn.SecretName)
	if err != nil {
		return "", err
	}

	sv := newValidator(connName, fmt.Sprintf("aws_secrets_manager secret %q", conn.SecretName))
	host := sv.requireFromMap(secret, "host")
	dbName := sv.requireFromMap(secret, "db_name")
	user := sv.requireFromMap(secret, "user")
	password := sv.requireFromMap(secret, "password")
	if err := sv.err(); err != nil {
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

// --- AWS secrets cache ---

type awsSecretsCache struct {
	mu    sync.RWMutex
	cache map[string]map[string]string
}

var awsSecrets = &awsSecretsCache{
	cache: make(map[string]map[string]string),
}

// IsCached returns true if the secret has already been fetched.
func (sc *awsSecretsCache) IsCached(secretName string) bool {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	_, ok := sc.cache[secretName]
	return ok
}

// Get fetches a secret from AWS Secrets Manager, returning cached values on subsequent calls.
func (sc *awsSecretsCache) Get(ctx context.Context, awsProfile, secretName string) (map[string]string, error) {
	sc.mu.RLock()
	if cached, ok := sc.cache[secretName]; ok {
		sc.mu.RUnlock()
		return cached, nil
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
	sc.cache[secretName] = secret
	sc.mu.Unlock()

	return secret, nil
}
