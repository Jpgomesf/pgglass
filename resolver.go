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
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// ResolveCredentials returns a DSN string for the given connection based on its source type.
func ResolveCredentials(ctx context.Context, connName string, conn Connection) (string, error) {
	switch conn.Source {
	case "direct":
		return resolveDirect(connName, conn)
	case "env":
		return resolveEnv(connName, conn)
	case "dotenv":
		return resolveDotenv(connName, conn)
	case "aws_secrets_manager":
		return resolveAWS(ctx, connName, conn)
	default:
		return "", fmt.Errorf("connection %q: unknown source %q", connName, conn.Source)
	}
}

func resolveDirect(connName string, conn Connection) (string, error) {
	if conn.DSN == "" {
		return "", fmt.Errorf("connection %q (direct): 'dsn' is required", connName)
	}
	return conn.DSN, nil
}

func resolveEnv(connName string, conn Connection) (string, error) {
	envVar := conn.DSNEnvVar
	if envVar == "" {
		return "", fmt.Errorf("connection %q (env): 'dsn_env_var' is required", connName)
	}
	dsn := os.Getenv(envVar)
	if dsn == "" {
		return "", fmt.Errorf("connection %q (env): environment variable %q is not set", connName, envVar)
	}
	return dsn, nil
}

func resolveDotenv(connName string, conn Connection) (string, error) {
	dotenvPath := conn.DotenvPath
	if dotenvPath == "" {
		dotenvPath = ".env"
	}
	envVar := conn.DSNEnvVar
	if envVar == "" {
		return "", fmt.Errorf("connection %q (dotenv): 'dsn_env_var' is required", connName)
	}

	vars, err := parseDotenv(dotenvPath)
	if err != nil {
		return "", fmt.Errorf("connection %q (dotenv): %w", connName, err)
	}

	dsn, ok := vars[envVar]
	if !ok || dsn == "" {
		return "", fmt.Errorf("connection %q (dotenv): %q not found in %s", connName, envVar, dotenvPath)
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
		// Strip surrounding quotes
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

// --- AWS Secrets Manager resolver ---

// awsSecretsCache provides thread-safe caching for AWS Secrets Manager values.
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

// Get fetches a secret from AWS Secrets Manager, returning cached values
// on subsequent calls.
func (sc *awsSecretsCache) Get(ctx context.Context, awsProfile, secretName string) (map[string]string, error) {
	sc.mu.RLock()
	if cached, ok := sc.cache[secretName]; ok {
		sc.mu.RUnlock()
		return cached, nil
	}
	sc.mu.RUnlock()

	opts := []func(*config.LoadOptions) error{}
	if awsProfile != "" {
		opts = append(opts, config.WithSharedConfigProfile(awsProfile))
	}

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
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

func resolveAWS(ctx context.Context, connName string, conn Connection) (string, error) {
	if conn.SecretName == "" {
		return "", fmt.Errorf("connection %q (aws_secrets_manager): 'secret_name' is required", connName)
	}

	secret, err := awsSecrets.Get(ctx, conn.AWSProfile, conn.SecretName)
	if err != nil {
		return "", err
	}

	port := secret["port"]
	if port == "" {
		port = "5432"
	}

	dsn := fmt.Sprintf(
		"host=%s port=%s dbname=%s user=%s password=%s",
		secret["host"], port, secret["db_name"], secret["user"], secret["password"],
	)
	return dsn, nil
}
