package actions

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// The names below are the contract with action/action.yml and
// action/pollux-agent.yml; TestWorkflowContract pins them to the YAML.
const (
	// ArtifactName is the artifact the workflow uploads.
	ArtifactName = "pollux-agent-result"
	// ResultFileName is the file inside the artifact holding the Artifact JSON.
	ResultFileName = "result.json"

	// InputHeadSHA, InputPRNumber, InputNonce and InputDocs are the
	// workflow_dispatch input names.
	InputHeadSHA  = "head_sha"
	InputPRNumber = "pr_number"
	InputNonce    = "nonce"
	InputDocs     = "docs"

	maxArtifactBytes = 10 << 20
)

// resultJSON returns the result file of the completed run's result artifact.
func resultJSON(ctx context.Context, api WorkflowAPI, c review.Completion) ([]byte, error) {
	body, err := api.RunArtifact(ctx, c.InstallationID, c.Owner, c.Repo, c.RunID, ArtifactName)
	if err != nil {
		return nil, fmt.Errorf("fetch %s artifact: %w", ArtifactName, err)
	}
	defer func() { _ = body.Close() }()

	archive, err := io.ReadAll(io.LimitReader(body, maxArtifactBytes+1))
	if err != nil {
		return nil, fmt.Errorf("download artifact: %w", err)
	}
	if len(archive) > maxArtifactBytes {
		return nil, fmt.Errorf("download artifact: larger than %d bytes", maxArtifactBytes)
	}

	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("open artifact zip: %w", err)
	}
	file, err := zr.Open(ResultFileName)
	if err != nil {
		return nil, fmt.Errorf("open %s in artifact: %w", ResultFileName, err)
	}
	defer func() { _ = file.Close() }()

	result, err := io.ReadAll(io.LimitReader(file, maxArtifactBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s in artifact: %w", ResultFileName, err)
	}
	if len(result) > maxArtifactBytes {
		return nil, fmt.Errorf("read %s in artifact: larger than %d bytes", ResultFileName, maxArtifactBytes)
	}
	return result, nil
}
