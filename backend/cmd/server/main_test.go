package main

import (
	"testing"

	"github.com/mrkizildag/docs-agent/backend/internal/config"
)

func TestBuildRunners_ServerRunnerOnlyWithLLMProviderActionsAlways(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		llm        *config.LLM
		wantServer bool
		wantErr    bool
	}{
		{name: "LLM_PROVIDER unset", llm: nil},
		{name: "openai", llm: &config.LLM{Provider: config.LLMProviderOpenAI, BaseURL: "http://x", Model: "m"}, wantServer: true},
		{name: "anthropic", llm: &config.LLM{Provider: config.LLMProviderAnthropic, Model: "m"}, wantServer: true},
		{name: "unknown provider", llm: &config.LLM{Provider: "nope", Model: "m"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runners, err := buildRunners(config.Config{LLM: tc.llm}, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("buildRunners() error = %v, wantErr %t", err, tc.wantErr)
			}
			if got := runners.Server != nil; got != tc.wantServer {
				t.Errorf("Server runner set = %t, want %t", got, tc.wantServer)
			}
			if !tc.wantErr && runners.Actions == nil {
				t.Error("Actions runner = nil, want it set regardless of LLM config")
			}
		})
	}
}
