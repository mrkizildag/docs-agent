package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

func TestSavePR_FailedHistoryWriteRollsBackState(t *testing.T) {
	t.Parallel()

	store, path := open(t)
	ctx := t.Context()

	before := gate.PRState{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha1"}
	if err := store.SavePR(ctx, before); err != nil {
		t.Fatalf("SavePR(before) = %v, want nil error", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q) = %v", path, err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `DROP TABLE pr_events`); err != nil {
		t.Fatalf("drop pr_events = %v", err)
	}

	after := before
	after.HeadSHA = "sha2"
	after.History.Events = []gate.PREvent{{Key: "outdated/p1/sha2", Kind: gate.EventOutdated, ProposalID: "p1", HeadSHA: "sha2"}}
	if err := store.SavePR(ctx, after); err == nil {
		t.Fatal("SavePR(after) = nil, want an error when the event cannot be written")
	}

	got, err := store.LoadPR(ctx, "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("LoadPR() = %v", err)
	}
	if got.HeadSHA != "sha1" {
		t.Errorf("LoadPR().HeadSHA = %q after a failed history write, want %q (state rolled back)", got.HeadSHA, "sha1")
	}
}
