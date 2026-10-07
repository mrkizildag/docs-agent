package github

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

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
	for {
		page, resp, err := client.PullRequests.ListComments(ctx, owner, repo, number, reviewOpts)
		if err != nil {
			return nil, fmt.Errorf("list review comments %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, rc := range page {
			comments = append(comments, gate.Comment{
				ID: rc.GetID(), Mine: rc.GetUser().GetLogin() == bot, Kind: gate.CommentKindReview, URL: rc.GetHTMLURL(), Body: rc.GetBody(),
				Path: rc.GetPath(), StartLine: rc.GetStartLine(), Line: rc.GetLine(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		reviewOpts.Page = resp.NextPage
	}

	issueOpts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		page, resp, err := client.Issues.ListComments(ctx, owner, repo, number, issueOpts)
		if err != nil {
			return nil, fmt.Errorf("list issue comments %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, ic := range page {
			comments = append(comments, gate.Comment{ID: ic.GetID(), Mine: ic.GetUser().GetLogin() == bot, Kind: gate.CommentKindIssue, URL: ic.GetHTMLURL(), Body: ic.GetBody()})
		}
		if resp.NextPage == 0 {
			return comments, nil
		}
		issueOpts.Page = resp.NextPage
	}
}

// CreateReviewComment creates a review comment on the right side of rc.Path at
// rc.CommitSHA, or a file-level comment when rc.File is set.
func (c *Client) CreateReviewComment(ctx context.Context, installationID int64, owner, repo string, number int, rc gate.ReviewComment) (gate.Comment, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return gate.Comment{}, fmt.Errorf("create review comment %s/%s#%d: %w", owner, repo, number, err)
	}

	req := github.CreatePullRequestCommentRequest{
		Body:     rc.Body,
		CommitID: rc.CommitSHA,
		Path:     rc.Path,
	}
	if rc.File {
		req.SubjectType = new("file")
	} else {
		req.Line = &rc.Line
		req.Side = new("RIGHT")
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

const reviewThreadsQuery = `query($owner:String!,$repo:String!,$number:Int!,$cursor:String){repository(owner:$owner,name:$repo){pullRequest(number:$number){reviewThreads(first:100,after:$cursor){pageInfo{hasNextPage endCursor} nodes{id isResolved comments(first:100){nodes{fullDatabaseId}}}}}}}`

const resolveThreadMutation = `mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{id}}}`

// ResolveReviewThread resolves the review thread that contains review
// comment commentID, wherever GitHub lists it among the thread's comments.
// Threads are matched on their first 100 comments; a proposal thread never
// grows near that. A thread that is already resolved, or gone with its
// comment, is not an error.
func (c *Client) ResolveReviewThread(ctx context.Context, installationID int64, owner, repo string, number int, commentID int64) error {
	client, err := c.installationClient(installationID)
	if err != nil {
		return fmt.Errorf("resolve review thread %s/%s#%d comment %d: %w", owner, repo, number, commentID, err)
	}

	type threadComment struct {
		FullDatabaseID json.Number `json:"fullDatabaseId"`
	}
	wantID := strconv.FormatInt(commentID, 10)
	isWanted := func(c threadComment) bool { return c.FullDatabaseID.String() == wantID }
	var cursor *string
	for {
		var data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Comments   struct {
								Nodes []threadComment `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "repo": repo, "number": number, "cursor": cursor}
		if err := graphQL(ctx, client, reviewThreadsQuery, vars, &data); err != nil {
			return fmt.Errorf("list review threads %s/%s#%d: %w", owner, repo, number, err)
		}

		threads := data.Repository.PullRequest.ReviewThreads
		for _, th := range threads.Nodes {
			if !slices.ContainsFunc(th.Comments.Nodes, isWanted) {
				continue
			}
			if th.IsResolved {
				return nil
			}
			var resolved struct{}
			if err := graphQL(ctx, client, resolveThreadMutation, map[string]any{"id": th.ID}, &resolved); err != nil {
				return fmt.Errorf("resolve review thread %s/%s#%d comment %d: %w", owner, repo, number, commentID, err)
			}
			return nil
		}
		if !threads.PageInfo.HasNextPage {
			return nil
		}
		cursor = &threads.PageInfo.EndCursor
	}
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
