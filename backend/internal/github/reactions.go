package github

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/go-github/v92/github"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

// React adds reaction to comment id and returns the reaction's ID; GitHub
// returns the existing reaction when this app already left it.
func (c *Client) React(ctx context.Context, installationID int64, owner, repo string, kind gate.CommentKind, id int64, reaction gate.Reaction) (int64, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return 0, fmt.Errorf("react %s/%s %s comment %d: %w", owner, repo, kind, id, err)
	}

	switch kind {
	case gate.CommentKindReview:
		created, _, err := client.Reactions.CreatePullRequestCommentReaction(ctx, owner, repo, id, string(reaction))
		if err != nil {
			return 0, fmt.Errorf("react %s/%s review comment %d: %w", owner, repo, id, err)
		}
		return created.GetID(), nil
	case gate.CommentKindIssue:
		created, _, err := client.Reactions.CreateIssueCommentReaction(ctx, owner, repo, id, string(reaction))
		if err != nil {
			return 0, fmt.Errorf("react %s/%s issue comment %d: %w", owner, repo, id, err)
		}
		return created.GetID(), nil
	default:
		return 0, fmt.Errorf("react %s/%s comment %d: unknown comment kind %q", owner, repo, id, kind)
	}
}

// Unreact removes reaction reactionID from comment id; a reaction that is
// already gone is not an error.
func (c *Client) Unreact(ctx context.Context, installationID int64, owner, repo string, kind gate.CommentKind, id, reactionID int64) error {
	client, err := c.installationClient(installationID)
	if err != nil {
		return fmt.Errorf("unreact %s/%s %s comment %d: %w", owner, repo, kind, id, err)
	}

	switch kind {
	case gate.CommentKindReview:
		resp, err := client.Reactions.DeletePullRequestCommentReaction(ctx, owner, repo, id, reactionID)
		if err != nil && !isNotFound(resp) {
			return fmt.Errorf("unreact %s/%s review comment %d: %w", owner, repo, id, err)
		}
	case gate.CommentKindIssue:
		resp, err := client.Reactions.DeleteIssueCommentReaction(ctx, owner, repo, id, reactionID)
		if err != nil && !isNotFound(resp) {
			return fmt.Errorf("unreact %s/%s issue comment %d: %w", owner, repo, id, err)
		}
	default:
		return fmt.Errorf("unreact %s/%s comment %d: unknown comment kind %q", owner, repo, id, kind)
	}
	return nil
}

func isNotFound(resp *github.Response) bool {
	return resp != nil && resp.Response != nil && resp.StatusCode == http.StatusNotFound
}
