package actions_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/actions"
)

const actionDir = "../../../../action"

func readYAML(t *testing.T, name string, out any) string {
	t.Helper()

	root, err := os.OpenRoot(actionDir)
	if err != nil {
		t.Fatalf("open %s: %v", actionDir, err)
	}
	defer func() { _ = root.Close() }()
	raw, err := root.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := yaml.Unmarshal(raw, out); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return string(raw)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func TestWorkflowContract(t *testing.T) {
	t.Parallel()

	var action struct {
		Inputs map[string]any `yaml:"inputs"`
		Runs   struct {
			Steps []struct {
				Uses string            `yaml:"uses"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"runs"`
	}
	actionText := readYAML(t, "action.yml", &action)

	var workflow struct {
		On struct {
			Dispatch struct {
				Inputs map[string]any `yaml:"inputs"`
			} `yaml:"workflow_dispatch"`
		} `yaml:"on"`
	}
	readYAML(t, "pollux-agent.yml", &workflow)

	dispatchInputs := []string{actions.InputDocs, actions.InputHeadSHA, actions.InputNonce, actions.InputPRNumber}

	t.Run("workflow_dispatch inputs", func(t *testing.T) {
		t.Parallel()
		if got := sortedKeys(workflow.On.Dispatch.Inputs); !slices.Equal(got, dispatchInputs) {
			t.Errorf("pollux-agent.yml workflow_dispatch inputs = %v, want %v", got, dispatchInputs)
		}
	})

	t.Run("action inputs", func(t *testing.T) {
		t.Parallel()
		for _, name := range dispatchInputs {
			if _, ok := action.Inputs[name]; !ok {
				t.Errorf("action.yml declares no input %q", name)
			}
		}
	})

	t.Run("artifact", func(t *testing.T) {
		t.Parallel()
		for _, step := range action.Runs.Steps {
			if !strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
				continue
			}
			if got := step.With["name"]; got != actions.ArtifactName {
				t.Errorf("uploaded artifact name = %q, want %q", got, actions.ArtifactName)
			}
			if got := filepath.Base(step.With["path"]); got != actions.ResultFileName {
				t.Errorf("uploaded file = %q, want %q", got, actions.ResultFileName)
			}
			return
		}
		t.Error("action.yml has no upload-artifact step")
	})

	t.Run("envelope fields", func(t *testing.T) {
		t.Parallel()
		jq := regexp.MustCompile(`(?s)jq -n .*?--rawfile raw.*?'(\{.*?\})'\s*\\?\s*>\s*"\$out/` + regexp.QuoteMeta(actions.ResultFileName) + `"`).FindStringSubmatch(actionText)
		if jq == nil {
			t.Fatalf("action.yml has no jq filter writing %s", actions.ResultFileName)
		}
		var written []string
		for _, m := range regexp.MustCompile(`(\w+): (?:\$\w+|\()`).FindAllStringSubmatch(jq[1], -1) {
			written = append(written, m[1])
		}
		slices.Sort(written)

		var tags []string
		typ := reflect.TypeFor[actions.Artifact[struct{}]]()
		for i := range typ.NumField() {
			tags = append(tags, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
		}
		slices.Sort(tags)

		if !slices.Equal(written, tags) {
			t.Errorf("action.yml writes envelope fields %v, actions.Artifact has JSON tags %v", written, tags)
		}
	})
}

func TestScaffoldPromptNamesIndexHeading(t *testing.T) {
	t.Parallel()

	text, err := os.ReadFile(filepath.Join(actionDir, "scaffold.md"))
	if err != nil {
		t.Fatalf("read scaffold.md: %v", err)
	}
	if !strings.Contains(string(text), review.IndexHeading) {
		t.Errorf("action/scaffold.md does not contain %q, which docs.CheckScaffold requires of the index", review.IndexHeading)
	}
}
