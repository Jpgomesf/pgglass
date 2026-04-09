package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// CredentialSource resolves a DSN from a connection configuration.
// Each implementation handles one source type (direct, env, dotenv, aws_secrets_manager).
type CredentialSource interface {
	// Type returns the source identifier used in config.json (e.g. "direct", "env").
	Type() string

	// Resolve returns a libpq-compatible DSN string for the given connection.
	Resolve(ctx context.Context, connName string, conn Connection) (string, error)
}

// sources maps source type strings to their implementations.
var sources = map[string]CredentialSource{}

// RegisterSource registers a CredentialSource implementation.
// Call this in init() functions to add custom sources.
func RegisterSource(s CredentialSource) {
	sources[s.Type()] = s
}

// RegisteredSourceTypes returns the list of registered source type names.
func RegisteredSourceTypes() []string {
	types := make([]string, 0, len(sources))
	for t := range sources {
		types = append(types, t)
	}
	return types
}

func init() {
	RegisterSource(&DirectSource{})
	RegisterSource(&EnvSource{})
	RegisterSource(&DotenvSource{})
	RegisterSource(&AWSSecretsSource{})
}

// ResolveCredentials dispatches to the appropriate CredentialSource based on conn.Source.
func ResolveCredentials(ctx context.Context, connName string, conn Connection) (string, error) {
	src, ok := sources[conn.Source]
	if !ok {
		return "", fmt.Errorf("connection %q: unknown source %q (registered: %s)",
			connName, conn.Source, strings.Join(RegisteredSourceTypes(), ", "))
	}
	return src.Resolve(ctx, connName, conn)
}

// --- DirectSource ---

// DirectSource resolves credentials from an inline DSN string.
type DirectSource struct{}

func (DirectSource) Type() string { return "direct" }

func (DirectSource) Resolve(_ context.Context, connName string, conn Connection) (string, error) {
	if conn.DSN == "" {
		return "", fmt.Errorf("connection %q (direct): 'dsn' is required", connName)
	}
	return conn.DSN, nil
}

// --- EnvSource ---

// EnvSource resolves credentials from an environment variable.
type EnvSource struct{}

func (EnvSource) Type() string { return "env" }

func (EnvSource) Resolve(_ context.Context, connName string, conn Connection) (string, error) {
	if conn.DSNEnvVar == "" {
		return "", fmt.Errorf("connection %q (env): 'dsn_env_var' is required", connName)
	}
	dsn := os.Getenv(conn.DSNEnvVar)
	if dsn == "" {
		return "", fmt.Errorf("connection %q (env): environment variable %q is not set", connName, conn.DSNEnvVar)
	}
	return dsn, nil
}

// --- DotenvSource ---

// DotenvSource resolves credentials from a .env file.
type DotenvSource struct{}

func (DotenvSource) Type() string { return "dotenv" }

func (DotenvSource) Resolve(_ context.Context, connName string, conn Connection) (string, error) {
	dotenvPath := conn.DotenvPath
	if dotenvPath == "" {
		dotenvPath = ".env"
	}
	if conn.DSNEnvVar == "" {
		return "", fmt.Errorf("connection %q (dotenv): 'dsn_env_var' is required", connName)
	}

	vars, err := parseDotenv(dotenvPath)
	if err != nil {
		return "", fmt.Errorf("connection %q (dotenv): %w", connName, err)
	}

	dsn, ok := vars[conn.DSNEnvVar]
	if !ok || dsn == "" {
		return "", fmt.Errorf("connection %q (dotenv): %q not found in %s", connName, conn.DSNEnvVar, dotenvPath)
	}
	return dsn, nil
}

// parseDotenv reads a .env file into a map. Supports KEY=VALUE, quoted values,
// comments (#), and blank lines. No variable expansion.
func parseDotenv(path string) (map[string]string, error) {
	f, err := os.Open(path) //nolint:gosec // user-configured path
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vars := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		vars[key] = value
	}
	return vars, scanner.Err()
}

// --- AWSSecretsSource ---

// AWSSecretsSource resolves credentials from AWS Secrets Manager.
// Secrets are cached in-memory for the lifetime of the process.
type AWSSecretsSource struct{}

func (AWSSecretsSource) Type() string { return "aws_secrets_manager" }

func (AWSSecretsSource) Resolve(ctx context.Context, connName string, conn Connection) (string, error) {
	if conn.SecretName == "" {
		return "", fmt.Errorf("connection %q (aws_secrets_manager): 'secret_name' is required", connName)
	}

	secret, err := awsSecrets.Get(ctx, conn.AWSProfile, conn.SecretName)
	if err != nil {
		return "", err
	}

	host := secret["host"]
	if host == "" {
		return "", fmt.Errorf("connection %q (aws_secrets_manager): secret %q missing required field 'host'", connName, conn.SecretName)
	}
	dbName := secret["db_name"]
	if dbName == "" {
		return "", fmt.Errorf("connection %q (aws_secrets_manager): secret %q missing required field 'db_name'", connName, conn.SecretName)
	}
	user := secret["user"]
	if user == "" {
		return "", fmt.Errorf("connection %q (aws_secrets_manager): secret %q missing required field 'user'", connName, conn.SecretName)
	}
	password := secret["password"]
	if password == "" {
		return "", fmt.Errorf("connection %q (aws_secrets_manager): secret %q missing required field 'password'", connName, conn.SecretName)
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

// --- AWS secrets cache (shared by AWSSecretsSource) ---

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
