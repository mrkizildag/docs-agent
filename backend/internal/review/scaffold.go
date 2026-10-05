package review

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// Scaffolder writes the starting docs/ folder for a repo that has none.
type Scaffolder interface {
	// StartScaffold returns a Scaffold when the scaffolder finishes synchronously,
	// or Pending when the Scaffold arrives later through a webhook.
	StartScaffold(ctx context.Context, req ScaffoldRequest) (ScaffoldStarted, error)
}

// AsyncScaffolder is a Scaffolder whose StartScaffold may return Pending;
// CollectScaffold produces the Scaffold once the external run completes.
type AsyncScaffolder interface {
	Scaffolder
	// CollectScaffold returns *InvalidResultError when the run finished but its
	// output is unusable; any other error is transient and may be retried.
	CollectScaffold(ctx context.Context, c Completion) (Scaffold, error)
}

// ScaffoldRequest is the repo commit a scaffold is written from.
type ScaffoldRequest struct {
	InstallationID int64
	Owner          string
	Repo           string
	BaseSHA        string
}

// ScaffoldStarted is Pending or Scaffold.
type ScaffoldStarted interface{ isScaffoldStarted() }

// Scaffold is the three docs of the default structure. Fixed fields make any
// other path inexpressible.
type Scaffold struct {
	Runner       string
	Model        string
	Index        string // docs/README.md
	Architecture string // docs/architecture.md
	Setup        string // docs/guides/setup.md
}

func (Pending) isScaffoldStarted()  {}
func (Scaffold) isScaffoldStarted() {}

// ScaffoldDocs is what the Actions runner's Claude Code run returns as
// structured_output when it writes the starting docs.
type ScaffoldDocs struct {
	Index        string `json:"index" jsonschema:"Full markdown of docs/README.md, frontmatter included."`
	Architecture string `json:"architecture" jsonschema:"Full markdown of docs/architecture.md, frontmatter included."`
	Setup        string `json:"setup" jsonschema:"Full markdown of docs/guides/setup.md, frontmatter included."`
}

// ScaffoldSchema returns the JSON Schema for ScaffoldDocs.
func ScaffoldSchema() ([]byte, error) {
	schema, err := jsonschema.For[ScaffoldDocs](nil)
	if err != nil {
		return nil, fmt.Errorf("infer schema for review.ScaffoldDocs: %w", err)
	}
	return marshalSchema(schema, "review.ScaffoldDocs")
}
