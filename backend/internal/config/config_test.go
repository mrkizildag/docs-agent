package config_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/mrkizildag/docs-agent/backend/internal/config"
)

func writeTestKey(t *testing.T) (path string, keyPEM []byte) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	keyPEM = pem.EncodeToMemory(block)
	path = filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path, keyPEM
}

func writeInvalidKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(path, []byte("not a pem file"), 0o600); err != nil {
		t.Fatalf("write invalid key: %v", err)
	}
	return path
}

func requiredEnv(keyPath string) map[string]string {
	return map[string]string{
		"GITHUB_APP_ID":               "123",
		"GITHUB_APP_PRIVATE_KEY_FILE": keyPath,
		"GITHUB_WEBHOOK_SECRET":       "whsecret",
		"ANTHROPIC_API_KEY":           "anthropic-secret",
	}
}

func mergeEnv(base, overrides map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(overrides))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	return merged
}

func envVars() []string {
	return []string{
		"ADDR", "LOG_LEVEL", "DATABASE_PATH", "GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY_FILE",
		"GITHUB_WEBHOOK_SECRET", "ANTHROPIC_API_KEY",
	}
}

func TestLoad(t *testing.T) {
	keyPath, wantKeyPEM := writeTestKey(t)
	// The external test package cannot construct a Secret, so secrets are checked via Reveal.
	ignoreSecrets := cmpopts.IgnoreFields(config.Config{}, "GitHubPrivateKey", "WebhookSecret", "AnthropicAPIKey")

	tests := []struct {
		name    string
		env     map[string]string
		want    config.Config
		wantErr bool
	}{
		{
			name: "defaults with required vars set",
			env:  requiredEnv(keyPath),
			want: config.Config{Addr: ":8080", LogLevel: slog.LevelInfo, DatabasePath: "docs-agent.db", GitHubAppID: 123},
		},
		{
			name: "overrides",
			env:  mergeEnv(requiredEnv(keyPath), map[string]string{"ADDR": ":9000", "LOG_LEVEL": "debug", "DATABASE_PATH": "/data/docs-agent.db"}),
			want: config.Config{Addr: ":9000", LogLevel: slog.LevelDebug, DatabasePath: "/data/docs-agent.db", GitHubAppID: 123},
		},
		{
			name:    "invalid log level",
			env:     mergeEnv(requiredEnv(keyPath), map[string]string{"LOG_LEVEL": "loud"}),
			wantErr: true,
		},
		{
			name:    "missing app id",
			env:     mergeEnv(requiredEnv(keyPath), map[string]string{"GITHUB_APP_ID": ""}),
			wantErr: true,
		},
		{
			name:    "bad app id",
			env:     mergeEnv(requiredEnv(keyPath), map[string]string{"GITHUB_APP_ID": "not-a-number"}),
			wantErr: true,
		},
		{
			name:    "missing private key file",
			env:     mergeEnv(requiredEnv(keyPath), map[string]string{"GITHUB_APP_PRIVATE_KEY_FILE": ""}),
			wantErr: true,
		},
		{
			name:    "unreadable private key file",
			env:     mergeEnv(requiredEnv(keyPath), map[string]string{"GITHUB_APP_PRIVATE_KEY_FILE": "/nonexistent/key.pem"}),
			wantErr: true,
		},
		{
			name:    "private key file not a valid key",
			env:     mergeEnv(requiredEnv(keyPath), map[string]string{"GITHUB_APP_PRIVATE_KEY_FILE": writeInvalidKey(t)}),
			wantErr: true,
		},
		{
			name:    "missing webhook secret",
			env:     mergeEnv(requiredEnv(keyPath), map[string]string{"GITHUB_WEBHOOK_SECRET": ""}),
			wantErr: true,
		},
		{
			name:    "missing anthropic key",
			env:     mergeEnv(requiredEnv(keyPath), map[string]string{"ANTHROPIC_API_KEY": ""}),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range envVars() {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			got, err := config.Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}

			if diff := cmp.Diff(tc.want, got, ignoreSecrets); diff != "" {
				t.Errorf("Load() (-want +got):\n%s", diff)
			}
			if got.WebhookSecret.Reveal() != "whsecret" {
				t.Errorf("WebhookSecret.Reveal() = %q, want %q", got.WebhookSecret.Reveal(), "whsecret")
			}
			if got.AnthropicAPIKey.Reveal() != "anthropic-secret" {
				t.Errorf("AnthropicAPIKey.Reveal() = %q, want %q", got.AnthropicAPIKey.Reveal(), "anthropic-secret")
			}
			if got.GitHubPrivateKey.Reveal() != string(wantKeyPEM) {
				t.Errorf("GitHubPrivateKey.Reveal() = %q, want %q", got.GitHubPrivateKey.Reveal(), wantKeyPEM)
			}
		})
	}
}

func TestSecretRedacted(t *testing.T) {
	keyPath, _ := writeTestKey(t)
	for _, k := range envVars() {
		t.Setenv(k, "")
	}
	for k, v := range requiredEnv(keyPath) {
		t.Setenv(k, v)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	const value = "whsecret"

	if s := fmt.Sprint(cfg.WebhookSecret); strings.Contains(s, value) {
		t.Errorf("fmt.Sprint leaked secret: %q", s)
	}
	if s := fmt.Sprintf("%#v", cfg.WebhookSecret); strings.Contains(s, value) {
		t.Errorf("%%#v leaked secret: %q", s)
	}

	logBuf := &strings.Builder{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	logger.Info("test", "secret", cfg.WebhookSecret)
	if strings.Contains(logBuf.String(), value) {
		t.Errorf("slog output leaked secret: %q", logBuf.String())
	}

	b, err := json.Marshal(cfg.WebhookSecret)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(b), value) {
		t.Errorf("json.Marshal leaked secret: %q", string(b))
	}
}
