package gate

import (
	"context"
	"fmt"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// postComments lists the PR's comments when there is anything to reconcile,
// performs Reconcile's writes, and records created comment IDs and URLs in the
// returned state, which is prev otherwise unchanged.
func (s *Service) postComments(ctx context.Context, prev PRState, pr PullRequest, verdict review.Verdict, changed []review.ChangedFile) (PRState, error) {
	proposals, _ := verdict.(review.Proposals)
	if len(prev.Proposals) == 0 && prev.SummaryCommentID == 0 && len(proposals) == 0 {
		return prev, nil
	}
	existing, err := s.gh.ListComments(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return PRState{}, fmt.Errorf("list comments: %w", err)
	}

	next, writes := Reconcile(prev, pr, verdict, changed, existing)
	// Saving before the first create means a run that stops after posting
	// leaves state behind, so the next run lists comments and adopts them by marker.
	newSummary := writes.Summary && next.SummaryCommentID == 0
	if len(writes.Creates) > 0 || newSummary {
		if err := s.store.SavePR(ctx, next); err != nil {
			return PRState{}, fmt.Errorf("save state before creating comments: %w", err)
		}
	}
	// GitHub orders comments by creation time, so a new summary is created
	// before the review comments to sit above them, then edited with their links.
	if newSummary && (len(writes.Creates) > 0 || len(writes.Edits) > 0) {
		if err := s.writeSummary(ctx, pr, &next); err != nil {
			return PRState{}, err
		}
	}
	for _, w := range writes.Creates {
		if err := s.createProposalComment(ctx, pr, &next, w); err != nil {
			return PRState{}, err
		}
	}
	for _, w := range writes.Edits {
		if err := s.gh.EditReviewComment(ctx, pr.InstallationID, pr.Owner, pr.Repo, w.ID, w.Body); err != nil {
			return PRState{}, fmt.Errorf("edit review comment for %s: %w", next.Proposals[w.Index].DocPath, err)
		}
	}
	if writes.Summary {
		if err := s.writeSummary(ctx, pr, &next); err != nil {
			return PRState{}, err
		}
	}
	return next, nil
}

// postFailureSummary writes the summary comment with prev's failure cause, leaving proposals untouched.
func (s *Service) postFailureSummary(ctx context.Context, prev PRState, pr PullRequest) (PRState, error) {
	existing, err := s.gh.ListComments(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return PRState{}, fmt.Errorf("list comments: %w", err)
	}
	next := reconcileFailure(prev, existing)
	if err := s.writeSummary(ctx, pr, &next); err != nil {
		return PRState{}, err
	}
	return next, nil
}

func (s *Service) createProposalComment(ctx context.Context, pr PullRequest, next *PRState, w ProposalCreate) error {
	ps := &next.Proposals[w.Index]
	c, err := s.gh.CreateReviewComment(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number, w.Comment)
	if err != nil {
		return fmt.Errorf("create review comment for %s: %w", ps.DocPath, err)
	}
	ps.CommentID, ps.CommentURL = c.ID, c.URL
	return nil
}

func (s *Service) writeSummary(ctx context.Context, pr PullRequest, next *PRState) error {
	body := renderSummary(*next)
	if next.SummaryCommentID != 0 {
		if err := s.gh.EditIssueComment(ctx, pr.InstallationID, pr.Owner, pr.Repo, next.SummaryCommentID, body); err != nil {
			return fmt.Errorf("edit summary comment: %w", err)
		}
		return nil
	}
	c, err := s.gh.CreateIssueComment(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number, body)
	if err != nil {
		return fmt.Errorf("create summary comment: %w", err)
	}
	next.SummaryCommentID = c.ID
	return nil
}
