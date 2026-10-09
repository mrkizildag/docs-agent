package instructions_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review/instructions"
)

func TestActionTextStatesEveryRuleAndOnlyItsOwnTools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		rules []string
		text  string
	}{
		{"review", instructions.ReviewRules(), instructions.ActionReview()},
		{"scaffold", instructions.ScaffoldRules(), instructions.ActionScaffold()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, rule := range append([]string{instructions.Untrusted}, tc.rules...) {
				if !strings.Contains(tc.text, rule) {
					t.Errorf("action text lacks rule %q", rule)
				}
			}
			for _, limit := range []string{"using only the Read, Grep and Glob tools", "refuse any other path", "You cannot modify anything"} {
				if !strings.Contains(tc.text, limit) {
					t.Errorf("action text lacks the limit %q", limit)
				}
			}
			for _, name := range []string{"read_file", "submit_proposals", "list_dir", "submit_docs", "UNTRUSTED"} {
				if strings.Contains(tc.text, name) {
					t.Errorf("action text names the server's %q", name)
				}
			}
		})
	}
}
