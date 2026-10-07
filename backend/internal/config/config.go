// Package config is the only package that reads the environment.
package config

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
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
	DatabasePath     string
	GitHubAppID      int64
	GitHubPrivateKey Secret
	WebhookSecret    Secret
	LLM              *LLM
	Dashboard        *Dashboard
}

// sessionKeyBytes is the AES-256 key length the dashboard seals tokens with.
const sessionKeyBytes = 32

// Dashboard configures Sign in with GitHub. A nil *Dashboard on Config means
// the dashboard routes are off: none of its variables were set.
type Dashboard struct {
	ClientID     string
	ClientSecret Secret
	// PublicURL is the https URL the browser reaches pollux at.
	PublicURL *url.URL
	// SessionKey holds the 32 raw key bytes, not their base64 text.
	SessionKey Secret
}

// Origin is PublicURL's scheme and lowercased host without the scheme's default
// port, the value browsers send as Origin.
func (d Dashboard) Origin() string {
	host := strings.ToLower(d.PublicURL.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	defaultPort := map[string]string{"https": "443", "http": "80"}[d.PublicURL.Scheme]
	if port := d.PublicURL.Port(); port != "" && port != defaultPort {
		host += ":" + port
	}
	return d.PublicURL.Scheme + "://" + host
}

// LLMProvider selects which wire format to speak to the LLM. OpenAI covers any
// OpenAI-compatible chat completions endpoint: Gemini, GitHub Models,
// OpenRouter, Ollama.
type LLMProvider string

const (
	LLMProviderAnthropic LLMProvider = "anthropic"
	LLMProviderOpenAI    LLMProvider = "openai"
)

// LLM configures the analysis runner. A nil *LLM on Config means the runner
// is off: LLM_PROVIDER was unset.
type LLM struct {
	Provider    LLMProvider
	BaseURL     string
	APIKey      Secret
	Model       string
	TriageModel string
}

// Load reads the environment and returns one error listing every invalid variable.
func Load() (Config, error) {
	var errs []error

	cfg := Config{
		Addr:         envOr("ADDR", ":8080"),
		DatabasePath: envOr("DATABASE_PATH", "pollux.db"),
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

	env := loadLLMEnv()
	if env.provider == "" {
		for _, v := range []struct{ key, value string }{
			{"LLM_BASE_URL", env.baseURL},
			{"LLM_API_KEY", env.apiKey},
			{"LLM_MODEL", env.model},
			{"LLM_TRIAGE_MODEL", env.triageModel},
		} {
			if v.value != "" {
				errs = append(errs, fmt.Errorf("%s: set but LLM_PROVIDER is unset", v.key))
			}
		}
	} else {
		llm, llmErrs := env.validate()
		errs = append(errs, llmErrs...)
		cfg.LLM = llm
	}

	dashboard, dashErrs := loadDashboard()
	errs = append(errs, dashErrs...)
	cfg.Dashboard = dashboard

	return cfg, errors.Join(errs...)
}

// dashboardEnv holds the raw dashboard variables before validation.
type dashboardEnv struct {
	clientID, clientSecret, publicURL, sessionKey string
}

// loadDashboard reads the dashboard variables: all or none.
func loadDashboard() (*Dashboard, []error) {
	env := dashboardEnv{
		clientID:     os.Getenv("GITHUB_CLIENT_ID"),
		clientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		publicURL:    os.Getenv("PUBLIC_URL"),
		sessionKey:   os.Getenv("SESSION_KEY"),
	}
	var set, unset []string
	for _, v := range []struct{ key, value string }{
		{"GITHUB_CLIENT_ID", env.clientID},
		{"GITHUB_CLIENT_SECRET", env.clientSecret},
		{"PUBLIC_URL", env.publicURL},
		{"SESSION_KEY", env.sessionKey},
	} {
		if v.value == "" {
			unset = append(unset, v.key)
		} else {
			set = append(set, v.key)
		}
	}
	if len(set) == 0 {
		return nil, nil
	}
	if len(unset) > 0 {
		return nil, []error{fmt.Errorf("%s: required when %s is set", strings.Join(unset, ", "), strings.Join(set, ", "))}
	}

	var errs []error
	publicURL, err := url.Parse(env.publicURL)
	if err != nil || publicURL.Scheme != "https" || publicURL.Host == "" || publicURL.RawQuery != "" || publicURL.Fragment != "" ||
		(publicURL.Path != "" && publicURL.Path != "/") {
		errs = append(errs, errors.New("PUBLIC_URL: must be an absolute https URL without path, query, or fragment"))
	}
	key, err := base64.StdEncoding.DecodeString(env.sessionKey)
	if err != nil || len(key) != sessionKeyBytes {
		errs = append(errs, fmt.Errorf("SESSION_KEY: must be %d bytes, base64-encoded", sessionKeyBytes))
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return &Dashboard{
		ClientID:     env.clientID,
		ClientSecret: Secret{value: env.clientSecret},
		PublicURL:    publicURL,
		SessionKey:   Secret{value: string(key)},
	}, nil
}

// llmEnv holds the raw LLM_* variables before validation.
type llmEnv struct {
	provider, baseURL, apiKey, model, triageModel string
}

func loadLLMEnv() llmEnv {
	return llmEnv{
		provider:    os.Getenv("LLM_PROVIDER"),
		baseURL:     os.Getenv("LLM_BASE_URL"),
		apiKey:      os.Getenv("LLM_API_KEY"),
		model:       os.Getenv("LLM_MODEL"),
		triageModel: os.Getenv("LLM_TRIAGE_MODEL"),
	}
}

// EvalRunner selects which runner the eval harness scores.
type EvalRunner string

const (
	// EvalRunnerServer scores the server runner against the configured LLM.
	EvalRunnerServer EvalRunner = "server"
	// EvalRunnerActions scores the Actions runner by running the action's Claude
	// step locally on a Claude credential.
	EvalRunnerActions EvalRunner = "actions"
)

// Eval configures the live eval harness: the runner under test, its LLM (server
// runner) or Claude credential (Actions runner), the judge model, how many runs
// per case, how many run at once, and an optional case filter.
type Eval struct {
	Runner EvalRunner
	// LLM is set for the server runner only.
	LLM LLM
	// ClaudeOAuthToken and AnthropicAPIKey are set for the Actions runner; at
	// least one is non-empty.
	ClaudeOAuthToken Secret
	AnthropicAPIKey  Secret
	// JudgeModel is empty for the Actions runner to use Claude Code's default.
	JudgeModel string
	// Path is the PATH the runners' child processes get. Kept out of reports:
	// it holds the developer's local layout.
	Path     string `json:"-"`
	Runs     int
	Parallel int
	Cases    []string
}

// LoadEval reads the EVAL_* variables, plus the LLM_* variables for the server
// runner or the Claude credential for the Actions runner, and returns one error
// listing every invalid variable.
func LoadEval() (Eval, error) {
	var errs []error

	var eval Eval
	eval.Runner = EvalRunner(envOr("EVAL_RUNNER", string(EvalRunnerServer)))
	eval.Path = os.Getenv("PATH")
	switch eval.Runner {
	case EvalRunnerServer:
		env := loadLLMEnv()
		if env.provider == "" {
			errs = append(errs, errors.New("LLM_PROVIDER is required"))
		} else {
			llm, llmErrs := env.validate()
			errs = append(errs, llmErrs...)
			if llm != nil {
				eval.LLM = *llm
			}
		}
		eval.JudgeModel = envOr("EVAL_JUDGE_MODEL", eval.LLM.Model)
	case EvalRunnerActions:
		oauth, apiKey := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"), os.Getenv("ANTHROPIC_API_KEY")
		if oauth == "" && apiKey == "" {
			errs = append(errs, errors.New("CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY is required when EVAL_RUNNER=actions"))
		}
		eval.ClaudeOAuthToken = Secret{value: oauth}
		eval.AnthropicAPIKey = Secret{value: apiKey}
		eval.JudgeModel = os.Getenv("EVAL_JUDGE_MODEL")
	default:
		errs = append(errs, fmt.Errorf("EVAL_RUNNER: unknown runner %q", eval.Runner))
	}

	var err error
	eval.Runs, err = positiveIntOr("EVAL_RUNS", 3)
	if err != nil {
		errs = append(errs, err)
	}
	eval.Parallel, err = positiveIntOr("EVAL_PARALLEL", 2)
	if err != nil {
		errs = append(errs, err)
	}
	for id := range strings.SplitSeq(os.Getenv("EVAL_CASE"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			eval.Cases = append(eval.Cases, id)
		}
	}

	return eval, errors.Join(errs...)
}

func positiveIntOr(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := parsePositive(key, v)
	if err != nil {
		return fallback, err
	}
	return int(n), nil
}

func parsePositive(key, v string) (int64, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s: must be a positive integer", key)
	}
	return n, nil
}

// validate checks the variables once LLM_PROVIDER is set and returns the
// resulting LLM, or nil and the collected errors.
func (e llmEnv) validate() (*LLM, []error) {
	var errs []error
	provider, baseURL, apiKey, model, triageModel := LLMProvider(e.provider), e.baseURL, e.apiKey, e.model, e.triageModel

	switch provider {
	case LLMProviderAnthropic:
		if apiKey == "" {
			errs = append(errs, errors.New("LLM_API_KEY is required when LLM_PROVIDER=anthropic"))
		}
	case LLMProviderOpenAI:
		if baseURL == "" {
			errs = append(errs, errors.New("LLM_BASE_URL is required when LLM_PROVIDER=openai"))
		}
	default:
		errs = append(errs, fmt.Errorf("LLM_PROVIDER: unknown provider %q", provider))
	}

	if model == "" {
		errs = append(errs, errors.New("LLM_MODEL is required when LLM_PROVIDER is set"))
	}
	if triageModel == "" {
		triageModel = model
	}

	if baseURL != "" {
		if err := validateAbsoluteHTTPURL(baseURL); err != nil {
			errs = append(errs, fmt.Errorf("LLM_BASE_URL: %w", err))
		}
	}

	if len(errs) > 0 {
		return nil, errs
	}

	return &LLM{
		Provider:    provider,
		BaseURL:     baseURL,
		APIKey:      Secret{value: apiKey},
		Model:       model,
		TriageModel: triageModel,
	}, nil
}

// validateAbsoluteHTTPURL reports an error unless raw parses as an absolute
// http or https URL.
func validateAbsoluteHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %q: %w", raw, err)
	}
	if !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not an absolute http(s) URL", raw)
	}
	return nil
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
	return parsePositive(key, v)
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
