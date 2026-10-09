package sqlite_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
)

func TestTakeLogin(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_000_000, 0)
	want := auth.Login{Verifier: "v", BindingHash: []byte("b"), ExpiresAt: now.Add(time.Minute)}

	t.Run("works once", func(t *testing.T) {
		t.Parallel()
		store, _ := open(t)
		if err := store.CreateLogin(t.Context(), []byte("s"), want); err != nil {
			t.Fatalf("CreateLogin() = %v, want nil error", err)
		}

		got, ok, err := store.TakeLogin(t.Context(), []byte("s"), now)
		if err != nil || !ok {
			t.Fatalf("first TakeLogin() = %v, %v, want ok", ok, err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("TakeLogin() (-want +got):\n%s", diff)
		}
		if _, ok, err := store.TakeLogin(t.Context(), []byte("s"), now); err != nil || ok {
			t.Errorf("second TakeLogin() = ok %v, err %v, want not found", ok, err)
		}
	})

	t.Run("expired is refused and spent", func(t *testing.T) {
		t.Parallel()
		store, _ := open(t)
		if err := store.CreateLogin(t.Context(), []byte("s"), want); err != nil {
			t.Fatalf("CreateLogin() = %v, want nil error", err)
		}

		if _, ok, err := store.TakeLogin(t.Context(), []byte("s"), want.ExpiresAt); err != nil || ok {
			t.Errorf("TakeLogin() at expiry = ok %v, err %v, want not found", ok, err)
		}
		if _, ok, _ := store.TakeLogin(t.Context(), []byte("s"), now); ok {
			t.Error("TakeLogin() after an expired take = ok, want the attempt gone")
		}
	})
}

func TestSessionLifecycle(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	want := auth.Session{
		IDHash:           []byte("id"),
		Profile:          auth.Profile{Login: "octocat", AvatarURL: "https://avatars.example/o.png"},
		SealedTokens:     []byte("sealed"),
		AccessExpiresAt:  time.Unix(1_500_000, 0),
		RefreshExpiresAt: time.Unix(2_000_000, 0),
		LastUsedAt:       time.Unix(1_000_000, 0),
	}
	if err := store.CreateSession(t.Context(), want); err != nil {
		t.Fatalf("CreateSession() = %v, want nil error", err)
	}

	got, ok, err := store.Session(t.Context(), []byte("id"))
	if err != nil || !ok {
		t.Fatalf("Session() = ok %v, err %v, want ok", ok, err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Session() (-want +got):\n%s", diff)
	}

	if err := store.DeleteSession(t.Context(), []byte("id")); err != nil {
		t.Fatalf("DeleteSession() = %v, want nil error", err)
	}
	if _, ok, err := store.Session(t.Context(), []byte("id")); err != nil || ok {
		t.Errorf("Session() after delete = ok %v, err %v, want not found", ok, err)
	}
}

func TestTouchAndSwapSession(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	if err := store.CreateSession(t.Context(), auth.Session{IDHash: []byte("id"), SealedTokens: []byte("a"), LastUsedAt: time.Unix(10, 0)}); err != nil {
		t.Fatalf("CreateSession() = %v, want nil error", err)
	}

	if err := store.TouchSession(t.Context(), []byte("id"), time.Unix(99, 0)); err != nil {
		t.Fatalf("TouchSession() = %v, want nil error", err)
	}
	got, _, _ := store.Session(t.Context(), []byte("id"))
	if !got.LastUsedAt.Equal(time.Unix(99, 0)) {
		t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, time.Unix(99, 0))
	}

	swap := func(prev int64, sealed string) bool {
		t.Helper()
		ok, err := store.SwapTokens(t.Context(), []byte("id"), prev, []byte(sealed), time.Unix(500, 0), time.Unix(600, 0))
		if err != nil {
			t.Fatalf("SwapTokens(%d) = %v, want nil error", prev, err)
		}
		return ok
	}
	if !swap(0, "b") {
		t.Fatal("SwapTokens(0) = false, want true at the current version")
	}
	if swap(0, "c") {
		t.Error("SwapTokens(0) again = true, want false after the version moved")
	}
	got, _, _ = store.Session(t.Context(), []byte("id"))
	want := auth.Session{
		IDHash: []byte("id"), SealedTokens: []byte("b"), Version: 1, LastUsedAt: time.Unix(99, 0),
		AccessExpiresAt: time.Unix(500, 0), RefreshExpiresAt: time.Unix(600, 0),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Session() after swap (-want +got):\n%s", diff)
	}
}

func TestDeleteExpired(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	now := time.Unix(1_000_000, 0)
	idleBefore := now.Add(-time.Hour)
	for name, exp := range map[string]time.Time{"fresh-login": now.Add(time.Minute), "old-login": now} {
		if err := store.CreateLogin(t.Context(), []byte(name), auth.Login{Verifier: "v", BindingHash: []byte("b"), ExpiresAt: exp}); err != nil {
			t.Fatalf("CreateLogin(%s) = %v", name, err)
		}
	}
	sessions := map[string]auth.Session{
		"live":    {LastUsedAt: now},
		"idle":    {LastUsedAt: idleBefore.Add(-time.Second)},
		"expired": {LastUsedAt: now, RefreshExpiresAt: now},
		"forever": {LastUsedAt: now, RefreshExpiresAt: time.Time{}},
	}
	for id, s := range sessions {
		s.IDHash, s.SealedTokens = []byte(id), []byte("x")
		if err := store.CreateSession(t.Context(), s); err != nil {
			t.Fatalf("CreateSession(%s) = %v", id, err)
		}
	}

	n, err := store.DeleteExpired(t.Context(), idleBefore, now)
	if err != nil || n != 3 {
		t.Fatalf("DeleteExpired() = %d, %v, want 3 rows", n, err)
	}
	for id, wantKept := range map[string]bool{"live": true, "forever": true, "idle": false, "expired": false} {
		if _, ok, _ := store.Session(t.Context(), []byte(id)); ok != wantKept {
			t.Errorf("session %q kept = %v, want %v", id, ok, wantKept)
		}
	}
	if _, ok, _ := store.TakeLogin(t.Context(), []byte("fresh-login"), now); !ok {
		t.Error("fresh login was deleted, want it kept")
	}
}
