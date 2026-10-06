package main

import (
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/config"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

func TestServerRunnerTimeoutIsBelowAnalysisDeadline(t *testing.T) {
	t.Parallel()

	if llmrunner.AnalysisTimeout >= gate.AnalysisDeadline {
		t.Errorf("llmrunner.AnalysisTimeout = %s, want below gate.AnalysisDeadline = %s", llmrunner.AnalysisTimeout, gate.AnalysisDeadline)
	}
}

func TestBuildRunners_ServerRunnerOnlyWithLLMProviderActionsAlways(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		llm        *config.LLM
		wantServer bool
	}{
		{name: "LLM_PROVIDER unset", llm: nil},
		{name: "openai", llm: &config.LLM{Provider: config.LLMProviderOpenAI, BaseURL: "http://x", Model: "m"}, wantServer: true},
		{name: "anthropic", llm: &config.LLM{Provider: config.LLMProviderAnthropic, Model: "m"}, wantServer: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runners, err := buildRunners(config.Config{LLM: tc.llm}, nil)
			if err != nil {
				t.Fatalf("buildRunners() error = %v, want nil", err)
			}
			if got := runners.Server != nil; got != tc.wantServer {
				t.Errorf("Server runner set = %t, want %t", got, tc.wantServer)
			}
			if runners.Actions == nil {
				t.Error("Actions runner = nil, want it set regardless of LLM config")
			}
		})
	}
}
