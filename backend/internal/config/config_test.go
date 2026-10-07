package config_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/mrkizildag/pollux-agent/backend/internal/config"
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
		"GITHUB_WEBHOOK_SECRET", "GITHUB_CLIENT_ID", "GITHUB_CLIENT_SECRET", "PUBLIC_URL", "SESSION_KEY",
		"LLM_PROVIDER", "LLM_BASE_URL", "LLM_API_KEY", "LLM_MODEL", "LLM_TRIAGE_MODEL",
		"EVAL_RUNNER", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "EVAL_JUDGE_MODEL", "EVAL_RUNS", "EVAL_PARALLEL", "EVAL_CASE",
	}
}

func TestLoad(t *testing.T) {
	keyPath, wantKeyPEM := writeTestKey(t)
	// The external test package cannot construct a Secret, so secrets are checked via Reveal.
	ignoreSecrets := cmpopts.IgnoreFields(config.Config{}, "GitHubPrivateKey", "WebhookSecret")
	ignoreLLMSecrets := cmpopts.IgnoreFields(config.LLM{}, "APIKey")

	tests := []struct {
		name          string
		env           map[string]string
		want          config.Config
		wantLLMAPIKey string
		wantErr       bool
	}{
		{
			name: "defaults with required vars set",
			env:  requiredEnv(keyPath),
			want: config.Config{Addr: ":8080", LogLevel: slog.LevelInfo, DatabasePath: "pollux.db", GitHubAppID: 123},
		},
		{
			name: "overrides",
			env:  mergeEnv(requiredEnv(keyPath), map[string]string{"ADDR": ":9000", "LOG_LEVEL": "debug", "DATABASE_PATH": "/data/pollux.db"}),
			want: config.Config{Addr: ":9000", LogLevel: slog.LevelDebug, DatabasePath: "/data/pollux.db", GitHubAppID: 123},
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
			name:    "LLM_MODEL set without LLM_PROVIDER",
			env:     mergeEnv(requiredEnv(keyPath), map[string]string{"LLM_MODEL": "claude-opus"}),
			wantErr: true,
		},
		{
			name:    "LLM_BASE_URL set without LLM_PROVIDER",
			env:     mergeEnv(requiredEnv(keyPath), map[string]string{"LLM_BASE_URL": "https://example.com"}),
			wantErr: true,
		},
		{
			name:    "LLM_API_KEY set without LLM_PROVIDER",
			env:     mergeEnv(requiredEnv(keyPath), map[string]string{"LLM_API_KEY": "key"}),
			wantErr: true,
		},
		{
			name:    "LLM_TRIAGE_MODEL set without LLM_PROVIDER",
			env:     mergeEnv(requiredEnv(keyPath), map[string]string{"LLM_TRIAGE_MODEL": "claude-haiku"}),
			wantErr: true,
		},
		{
			name: "unknown LLM provider",
			env: mergeEnv(requiredEnv(keyPath), map[string]string{
				"LLM_PROVIDER": "bedrock", "LLM_MODEL": "claude-opus", "LLM_API_KEY": "key",
			}),
			wantErr: true,
		},
		{
			name: "LLM_MODEL required when provider set",
			env: mergeEnv(requiredEnv(keyPath), map[string]string{
				"LLM_PROVIDER": "anthropic", "LLM_API_KEY": "key",
			}),
			wantErr: true,
		},
		{
			name: "anthropic requires LLM_API_KEY",
			env: mergeEnv(requiredEnv(keyPath), map[string]string{
				"LLM_PROVIDER": "anthropic", "LLM_MODEL": "claude-opus",
			}),
			wantErr: true,
		},
		{
			name: "openai requires LLM_BASE_URL",
			env: mergeEnv(requiredEnv(keyPath), map[string]string{
				"LLM_PROVIDER": "openai", "LLM_MODEL": "gpt-4o",
			}),
			wantErr: true,
		},
		{
			name: "LLM_BASE_URL must be an absolute http(s) URL",
			env: mergeEnv(requiredEnv(keyPath), map[string]string{
				"LLM_PROVIDER": "openai", "LLM_MODEL": "gpt-4o", "LLM_BASE_URL": "not-a-url",
			}),
			wantErr: true,
		},
		{
			name: "valid anthropic provider",
			env: mergeEnv(requiredEnv(keyPath), map[string]string{
				"LLM_PROVIDER": "anthropic", "LLM_MODEL": "claude-opus", "LLM_API_KEY": "anthropic-secret",
			}),
			want: config.Config{
				Addr: ":8080", LogLevel: slog.LevelInfo, DatabasePath: "pollux.db", GitHubAppID: 123,
				LLM: &config.LLM{
					Provider: config.LLMProviderAnthropic, Model: "claude-opus", TriageModel: "claude-opus",
				},
			},
			wantLLMAPIKey: "anthropic-secret",
		},
		{
			name: "valid openai-compatible provider without an API key (e.g. Ollama)",
			env: mergeEnv(requiredEnv(keyPath), map[string]string{
				"LLM_PROVIDER": "openai", "LLM_MODEL": "llama3", "LLM_BASE_URL": "http://localhost:11434/v1",
			}),
			want: config.Config{
				Addr: ":8080", LogLevel: slog.LevelInfo, DatabasePath: "pollux.db", GitHubAppID: 123,
				LLM: &config.LLM{
					Provider: config.LLMProviderOpenAI, Model: "llama3", TriageModel: "llama3",
					BaseURL: "http://localhost:11434/v1",
				},
			},
		},
		{
			name: "LLM_TRIAGE_MODEL overrides the default",
			env: mergeEnv(requiredEnv(keyPath), map[string]string{
				"LLM_PROVIDER": "anthropic", "LLM_MODEL": "claude-opus", "LLM_API_KEY": "anthropic-secret",
				"LLM_TRIAGE_MODEL": "claude-haiku",
			}),
			want: config.Config{
				Addr: ":8080", LogLevel: slog.LevelInfo, DatabasePath: "pollux.db", GitHubAppID: 123,
				LLM: &config.LLM{
					Provider: config.LLMProviderAnthropic, Model: "claude-opus", TriageModel: "claude-haiku",
				},
			},
			wantLLMAPIKey: "anthropic-secret",
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

			if diff := cmp.Diff(tc.want, got, ignoreSecrets, ignoreLLMSecrets); diff != "" {
				t.Errorf("Load() (-want +got):\n%s", diff)
			}
			if got.WebhookSecret.Reveal() != "whsecret" {
				t.Errorf("WebhookSecret.Reveal() = %q, want %q", got.WebhookSecret.Reveal(), "whsecret")
			}
			if got.GitHubPrivateKey.Reveal() != string(wantKeyPEM) {
				t.Errorf("GitHubPrivateKey.Reveal() = %q, want %q", got.GitHubPrivateKey.Reveal(), wantKeyPEM)
			}
			if tc.want.LLM != nil {
				if got.LLM.APIKey.Reveal() != tc.wantLLMAPIKey {
					t.Errorf("LLM.APIKey.Reveal() = %q, want %q", got.LLM.APIKey.Reveal(), tc.wantLLMAPIKey)
				}
			}
		})
	}
}

func TestLoadDashboard(t *testing.T) {
	keyPath, _ := writeTestKey(t)
	sessionKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	full := map[string]string{
		"GITHUB_CLIENT_ID":     "Iv1.abc",
		"GITHUB_CLIENT_SECRET": "client-secret",
		"PUBLIC_URL":           "https://pollux.example.com",
		"SESSION_KEY":          sessionKey,
	}

	tests := []struct {
		name    string
		env     map[string]string
		wantOn  bool
		wantErr string
	}{
		{name: "none set", env: nil},
		{name: "all set", env: full, wantOn: true},
		{name: "client secret missing", env: mergeEnv(full, map[string]string{"GITHUB_CLIENT_SECRET": ""}), wantErr: "GITHUB_CLIENT_SECRET"},
		{name: "only public url", env: map[string]string{"PUBLIC_URL": "https://pollux.example.com"}, wantErr: "GITHUB_CLIENT_ID"},
		{name: "http public url", env: mergeEnv(full, map[string]string{"PUBLIC_URL": "http://pollux.example.com"}), wantErr: "PUBLIC_URL"},
		{name: "public url with path", env: mergeEnv(full, map[string]string{"PUBLIC_URL": "https://pollux.example.com/app"}), wantErr: "PUBLIC_URL"},
		{name: "session key not base64", env: mergeEnv(full, map[string]string{"SESSION_KEY": "!!!"}), wantErr: "SESSION_KEY"},
		{name: "session key too short", env: mergeEnv(full, map[string]string{"SESSION_KEY": base64.StdEncoding.EncodeToString(make([]byte, 16))}), wantErr: "SESSION_KEY"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range envVars() {
				t.Setenv(k, "")
			}
			for k, v := range mergeEnv(requiredEnv(keyPath), tc.env) {
				t.Setenv(k, v)
			}

			got, err := config.Load()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load() error = %v, want it to mention %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if (got.Dashboard != nil) != tc.wantOn {
				t.Fatalf("Load().Dashboard = %v, want set: %v", got.Dashboard, tc.wantOn)
			}
			if !tc.wantOn {
				return
			}
			if got.Dashboard.ClientID != "Iv1.abc" || got.Dashboard.ClientSecret.Reveal() != "client-secret" ||
				got.Dashboard.PublicURL.String() != "https://pollux.example.com" || got.Dashboard.SessionKey.Reveal() != string(make([]byte, 32)) {
				t.Errorf("Load().Dashboard = %+v, want the configured values", got.Dashboard)
			}
		})
	}
}

func TestLoadEval(t *testing.T) {
	llmEnv := map[string]string{"LLM_PROVIDER": "anthropic", "LLM_API_KEY": "k", "LLM_MODEL": "big", "LLM_TRIAGE_MODEL": "small"}
	llmOpt := cmpopts.IgnoreFields(config.LLM{}, "APIKey")
	secretOpt := cmpopts.IgnoreFields(config.Eval{}, "ClaudeOAuthToken", "AnthropicAPIKey", "Path")

	tests := []struct {
		name    string
		env     map[string]string
		want    config.Eval
		wantErr bool
	}{
		{
			name: "defaults",
			env:  llmEnv,
			want: config.Eval{
				Runner:     config.EvalRunnerServer,
				LLM:        config.LLM{Provider: config.LLMProviderAnthropic, Model: "big", TriageModel: "small"},
				JudgeModel: "big", Runs: 3, Parallel: 2,
			},
		},
		{
			name: "overrides",
			env:  mergeEnv(llmEnv, map[string]string{"EVAL_JUDGE_MODEL": "judge", "EVAL_RUNS": "5", "EVAL_PARALLEL": "1", "EVAL_CASE": "a, b,,"}),
			want: config.Eval{
				Runner:     config.EvalRunnerServer,
				LLM:        config.LLM{Provider: config.LLMProviderAnthropic, Model: "big", TriageModel: "small"},
				JudgeModel: "judge", Runs: 5, Parallel: 1, Cases: []string{"a", "b"},
			},
		},
		{name: "provider required", env: map[string]string{}, wantErr: true},
		{
			name: "actions runner with oauth token",
			env:  map[string]string{"EVAL_RUNNER": "actions", "CLAUDE_CODE_OAUTH_TOKEN": "t"},
			want: config.Eval{Runner: config.EvalRunnerActions, Runs: 3, Parallel: 2},
		},
		{
			name: "actions runner with api key and judge model",
			env:  map[string]string{"EVAL_RUNNER": "actions", "ANTHROPIC_API_KEY": "k", "EVAL_JUDGE_MODEL": "judge"},
			want: config.Eval{Runner: config.EvalRunnerActions, JudgeModel: "judge", Runs: 3, Parallel: 2},
		},
		{name: "actions runner needs a credential", env: map[string]string{"EVAL_RUNNER": "actions"}, wantErr: true},
		{name: "unknown runner", env: mergeEnv(llmEnv, map[string]string{"EVAL_RUNNER": "cloud"}), wantErr: true},
		{name: "runs not positive", env: mergeEnv(llmEnv, map[string]string{"EVAL_RUNS": "0"}), wantErr: true},
		{name: "parallel not a number", env: mergeEnv(llmEnv, map[string]string{"EVAL_PARALLEL": "many"}), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range envVars() {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			t.Setenv("PATH", "/eval/bin")

			got, err := config.LoadEval()
			if tc.wantErr {
				if err == nil {
					t.Fatal("LoadEval() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadEval() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got, llmOpt, secretOpt); diff != "" {
				t.Errorf("LoadEval() mismatch (-want +got):\n%s", diff)
			}
			if got.Path != "/eval/bin" {
				t.Errorf("LoadEval() Path = %q, want %q", got.Path, "/eval/bin")
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

func TestEvalReportLeavesOutPath(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(config.Eval{Path: "/home/someone/bin"})
	if err != nil {
		t.Fatalf("json.Marshal(Eval) error = %v", err)
	}
	if strings.Contains(string(data), "someone") {
		t.Errorf("json.Marshal(Eval) = %s, want no PATH", data)
	}
}

func TestDashboardOrigin(t *testing.T) {
	t.Parallel()
	tests := []struct{ publicURL, want string }{
		{"https://Pollux.Example:443", "https://pollux.example"},
		{"https://pollux.example", "https://pollux.example"},
		{"https://pollux.example:8443", "https://pollux.example:8443"},
		{"https://[2001:DB8::1]:443", "https://[2001:db8::1]"},
	}
	for _, tc := range tests {
		u, err := url.Parse(tc.publicURL)
		if err != nil {
			t.Fatalf("url.Parse(%q) = %v", tc.publicURL, err)
		}
		if got := (config.Dashboard{PublicURL: u}).Origin(); got != tc.want {
			t.Errorf("Origin() of %q = %q, want %q", tc.publicURL, got, tc.want)
		}
	}
}
