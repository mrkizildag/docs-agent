package github

import (
	"context"
	"fmt"

	"github.com/google/go-github/v92/github"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

// ListComments returns owner/repo#number's review comments, then its issue
// comments.
func (c *Client) ListComments(ctx context.Context, installationID int64, owner, repo string, number int) ([]gate.Comment, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("list comments %s/%s#%d: %w", owner, repo, number, err)
	}

	bot, err := c.appBotLogin(ctx)
	if err != nil {
		return nil, fmt.Errorf("list comments %s/%s#%d: %w", owner, repo, number, err)
	}

	var comments []gate.Comment
	reviewOpts := &github.PullRequestListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for rc, err := range client.PullRequests.ListCommentsIter(ctx, owner, repo, number, reviewOpts) {
		if err != nil {
			return nil, fmt.Errorf("list review comments %s/%s#%d: %w", owner, repo, number, err)
		}
		comments = append(comments, gate.Comment{
			ID: rc.GetID(), Mine: rc.GetUser().GetLogin() == bot, Kind: gate.CommentKindReview, URL: rc.GetHTMLURL(), Body: rc.GetBody(),
			Path: rc.GetPath(), StartLine: rc.GetStartLine(), Line: rc.GetLine(),
		})
	}

	issueOpts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for ic, err := range client.Issues.ListCommentsIter(ctx, owner, repo, number, issueOpts) {
		if err != nil {
			return nil, fmt.Errorf("list issue comments %s/%s#%d: %w", owner, repo, number, err)
		}
		comments = append(comments, gate.Comment{ID: ic.GetID(), Mine: ic.GetUser().GetLogin() == bot, Kind: gate.CommentKindIssue, URL: ic.GetHTMLURL(), Body: ic.GetBody()})
	}
	return comments, nil
}

// CreateReviewComment creates a review comment on the right side of c.Path at
// c.CommitSHA.
func (c *Client) CreateReviewComment(ctx context.Context, installationID int64, owner, repo string, number int, rc gate.ReviewComment) (gate.Comment, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return gate.Comment{}, fmt.Errorf("create review comment %s/%s#%d: %w", owner, repo, number, err)
	}

	req := github.CreatePullRequestCommentRequest{
		Body:     rc.Body,
		CommitID: rc.CommitSHA,
		Path:     rc.Path,
		Line:     &rc.Line,
		Side:     new("RIGHT"),
	}
	if rc.StartLine != 0 {
		req.StartLine = &rc.StartLine
		req.StartSide = new("RIGHT")
	}

	created, _, err := client.PullRequests.CreateComment(ctx, owner, repo, number, req)
	if err != nil {
		return gate.Comment{}, fmt.Errorf("create review comment %s/%s#%d on %s:%d: %w", owner, repo, number, rc.Path, rc.Line, err)
	}
	return gate.Comment{ID: created.GetID(), Kind: gate.CommentKindReview, URL: created.GetHTMLURL(), Body: created.GetBody()}, nil
}

// EditReviewComment replaces the body of review comment id.
func (c *Client) EditReviewComment(ctx context.Context, installationID int64, owner, repo string, id int64, body string) error {
	client, err := c.installationClient(installationID)
	if err != nil {
		return fmt.Errorf("edit review comment %s/%s %d: %w", owner, repo, id, err)
	}

	if _, _, err := client.PullRequests.UpdateComment(ctx, owner, repo, id, github.UpdatePullRequestCommentRequest{Body: body}); err != nil {
		return fmt.Errorf("edit review comment %s/%s %d: %w", owner, repo, id, err)
	}
	return nil
}

// CreateIssueComment creates a comment on the conversation of owner/repo#number.
func (c *Client) CreateIssueComment(ctx context.Context, installationID int64, owner, repo string, number int, body string) (gate.Comment, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return gate.Comment{}, fmt.Errorf("create issue comment %s/%s#%d: %w", owner, repo, number, err)
	}

	created, _, err := client.Issues.CreateComment(ctx, owner, repo, number, github.IssueCommentRequest{Body: body})
	if err != nil {
		return gate.Comment{}, fmt.Errorf("create issue comment %s/%s#%d: %w", owner, repo, number, err)
	}
	return gate.Comment{ID: created.GetID(), Kind: gate.CommentKindIssue, URL: created.GetHTMLURL(), Body: created.GetBody()}, nil
}

// EditIssueComment replaces the body of issue comment id.
func (c *Client) EditIssueComment(ctx context.Context, installationID int64, owner, repo string, id int64, body string) error {
	client, err := c.installationClient(installationID)
	if err != nil {
		return fmt.Errorf("edit issue comment %s/%s %d: %w", owner, repo, id, err)
	}

	if _, _, err := client.Issues.UpdateComment(ctx, owner, repo, id, github.IssueCommentRequest{Body: body}); err != nil {
		return fmt.Errorf("edit issue comment %s/%s %d: %w", owner, repo, id, err)
	}
	return nil
}

// ReplyToReviewComment posts body as a reply in the thread of review comment inReplyTo.
func (c *Client) ReplyToReviewComment(ctx context.Context, installationID int64, owner, repo string, number int, inReplyTo int64, body string) (gate.Comment, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return gate.Comment{}, fmt.Errorf("reply to review comment %s/%s#%d %d: %w", owner, repo, number, inReplyTo, err)
	}

	created, _, err := client.PullRequests.CreateCommentInReplyTo(ctx, owner, repo, number, body, inReplyTo)
	if err != nil {
		return gate.Comment{}, fmt.Errorf("reply to review comment %s/%s#%d %d: %w", owner, repo, number, inReplyTo, err)
	}
	return gate.Comment{ID: created.GetID(), Kind: gate.CommentKindReview, URL: created.GetHTMLURL(), Body: created.GetBody()}, nil
}
