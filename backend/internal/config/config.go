// Package config is the only package that reads the environment.
package config

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
)

// Secret redacts its value in fmt, slog, and JSON/text encodings so it never
// ends up in logs, error messages, or marshaled output.
type Secret struct {
	value string
}

func (s Secret) Reveal() string { return s.value }

func (Secret) String() string { return "[redacted]" }

func (Secret) GoString() string { return "[redacted]" }

func (Secret) LogValue() slog.Value { return slog.StringValue("[redacted]") }

func (Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type Config struct {
	Addr             string
	LogLevel         slog.Level
	GitHubAppID      int64
	GitHubPrivateKey Secret
	WebhookSecret    Secret
	AnthropicAPIKey  Secret
}

// Load reads the environment and returns one error listing every invalid variable.
func Load() (Config, error) {
	var errs []error

	cfg := Config{
		Addr: envOr("ADDR", ":8080"),
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(envOr("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}

	appID, err := requireInt64("GITHUB_APP_ID")
	if err != nil {
		errs = append(errs, err)
	}
	cfg.GitHubAppID = appID

	keyPath, err := requireString("GITHUB_APP_PRIVATE_KEY_FILE")
	if err != nil {
		errs = append(errs, err)
	} else {
		key, err := loadPrivateKey(keyPath)
		if err != nil {
			errs = append(errs, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_FILE: %w", err))
		} else {
			cfg.GitHubPrivateKey = key
		}
	}

	webhookSecret, err := requireString("GITHUB_WEBHOOK_SECRET")
	if err != nil {
		errs = append(errs, err)
	}
	cfg.WebhookSecret = Secret{value: webhookSecret}

	anthropicKey, err := requireString("ANTHROPIC_API_KEY")
	if err != nil {
		errs = append(errs, err)
	}
	cfg.AnthropicAPIKey = Secret{value: anthropicKey}

	return cfg, errors.Join(errs...)
}

func requireString(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return v, nil
}

func requireInt64(key string) (int64, error) {
	v, err := requireString(key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s: must be a positive integer", key)
	}
	return n, nil
}

// loadPrivateKey reads the PEM file GitHub generates for the app (PKCS#1, "RSA
// PRIVATE KEY") and also accepts PKCS#8, validating it decodes before storing it.
func loadPrivateKey(path string) (Secret, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is operator config, not request input
	if err != nil {
		return Secret{}, fmt.Errorf("read %s: %w", path, err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return Secret{}, errors.New("not a valid PEM-encoded private key")
	}

	switch block.Type {
	case "RSA PRIVATE KEY":
		if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
			return Secret{}, fmt.Errorf("parse RSA private key: %w", err)
		}
	case "PRIVATE KEY":
		if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
			return Secret{}, fmt.Errorf("parse PKCS#8 private key: %w", err)
		}
	default:
		return Secret{}, fmt.Errorf("unexpected PEM block type %q", block.Type)
	}

	return Secret{value: string(data)}, nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
