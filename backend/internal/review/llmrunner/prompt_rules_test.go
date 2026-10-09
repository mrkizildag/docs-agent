package llmrunner_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review/instructions"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

func TestServerPromptsStateTheSharedRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prompt string
		rules  []string
	}{
		{"draft", llmrunner.DraftSystemPrompt(), instructions.ReviewRules()},
		{"scaffold", llmrunner.ScaffoldSystemPrompt(), instructions.ScaffoldRules()},
		{"triage", llmrunner.TriageSystemPrompt, []string{instructions.Threshold, instructions.NoDocNeeded}},
		{"new doc", llmrunner.NewDocSystemPrompt, []string{instructions.NewDocWhen, instructions.NoDocNeeded}},
		{"verify", llmrunner.VerifySystemPrompt, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			untrusted := strings.Index(tc.prompt, instructions.Untrusted)
			for _, rule := range append([]string{instructions.Untrusted, llmrunner.UntrustedMarkers}, tc.rules...) {
				i := strings.Index(tc.prompt, rule)
				if i < 0 {
					t.Errorf("%s prompt lacks rule %q", tc.name, rule)
				}
				if i < untrusted {
					t.Errorf("%s prompt states %q before the untrusted rule", tc.name, rule)
				}
			}
			for _, name := range []string{"JSON schema", "Read, Grep and Glob"} {
				if strings.Contains(tc.prompt, name) {
					t.Errorf("%s prompt names the action's %q", tc.name, name)
				}
			}
		})
	}
}

func TestServerAgentPromptsSayBadSubmissionsComeBack(t *testing.T) {
	t.Parallel()

	for name, prompt := range map[string]string{"draft": llmrunner.DraftSystemPrompt(), "scaffold": llmrunner.ScaffoldSystemPrompt()} {
		if !strings.Contains(prompt, "is returned with") {
			t.Errorf("%s prompt does not say a bad submission comes back with its problems", name)
		}
	}
}
