---
title: Sign in with GitHub
summary: How the dashboard knows who is asking and which repos they may see, and the invariants that keep sessions and access checks safe on a public URL.
covers:
  - backend/internal/auth/**
  - backend/internal/github/user*.go
  - backend/internal/httpapi/auth.go
  - backend/internal/httpapi/access.go
  - backend/internal/httpapi/webhook_limit.go
  - backend/internal/gate/sqlite/sessions.go
---

# Sign in with GitHub

The dashboard shows repo activity, so it must know who is asking and which repos they may see.

## Why GitHub is the identity

GitHub already knows both answers: where pollux is installed, and what each user can read there. pollux keeps no accounts, passwords, or teams of its own, so there is nothing to drift from GitHub's view of who belongs. The cost is that access comes from GitHub on every check, not from a table we own.

## Flow

1. `GET /auth/login` creates a login attempt and redirects to GitHub's authorize page. The attempt carries three secrets: `state`, a PKCE (S256) verifier, and a binding value. The database stores only hashes of `state` and the binding, plus the verifier. The binding goes to the browser in a short-lived `/auth`-scoped cookie.
2. `GET /auth/callback` accepts the attempt only if `state` matches, the binding cookie matches the same attempt, and the attempt is unexpired (10 minutes). Taking an attempt deletes it, so a reused `state` fails. The binding cookie is what stops an attacker from handing a victim a callback link for the attacker's own login. The code is then exchanged with the verifier. Any wrong, reused, or expired `state`, or a failed exchange, answers 400 and creates no session.
3. A session is created and its id set as the session cookie.

## Session invariants

- The cookie holds 32 random bytes and nothing else. The database keeps only `sha256(id)`, so a leaked database cannot be replayed as cookies. The cookie is `HttpOnly`, `Secure`, `SameSite=Lax`, with the `__Host-` prefix.
- The user's GitHub access and refresh tokens are stored server-side as one AES-GCM blob sealed with the session key, with the id hash as associated data so a blob cannot be moved to another session. They never reach the browser. They are kept (not discarded after login) so access can be re-checked.
- Sessions slide: each use extends them, with `last_used_at` written at most once a minute. A session unused for 7 days stops authenticating. It is also bounded by the GitHub refresh token's own expiry.
- GitHub user tokens last 8 hours. An expired one is refreshed on use. GitHub rotates both tokens on every refresh, so two concurrent refreshes would invalidate each other: refresh is serialized per session, the session is re-read under the lock, and the swap is compare-and-swap on a version. A refresh that fails with `bad_refresh_token` deletes the session; any other failure answers 401 and keeps it. A failed refresh is never a 500.
- Logout deletes the server-side session and revokes the GitHub token best-effort, and the cookie is cleared even when logout errors. The old cookie then returns 401.

## Access rule

A viewer may see a repo only if pollux is installed on it and the user can read it on GitHub. Both come from one call: the repos of this App's installations that the user has at least read on. The result is cached per session for 5 minutes, so losing access (repo removed, user removed, app uninstalled) takes effect within that window, and gaining it needs no new login.

A repo the user cannot see answers 404, the same as a repo that does not exist or where pollux is not installed. The status never reveals which case it is.

## Config

`GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, `PUBLIC_URL`, and `SESSION_KEY` are all or none. With none, the dashboard routes are not registered (404) and the webhook is unaffected. With some but not all, startup fails and names the missing ones. `PUBLIC_URL` must be https, because the cookies are `Secure` and GitHub redirects back to `PUBLIC_URL/auth/callback`. See [Setup](../guides/setup.md).

## Rate limits

`/auth/*` is public behind Funnel, so it has its own per-IP and global token buckets, separate from the webhook's: a webhook burst cannot lock people out of sign-in, and the reverse. Per IP it allows 10 requests a minute after a burst of 5, since people sign in a few times a day and each callback costs a GitHub round trip and a database write. Client IPs come from `X-Forwarded-For` only when the TCP peer is loopback, as for the webhook. `/api/*` is not limited here; it is gated by the session.

## Cleanup

Expired sessions and login attempts are deleted at each new login. There is no background ticker: abandoned logins cost a row until the next login, which is cheap and keeps the process free of another goroutine to own.
