---
title: Deploy
summary: Run pollux in Docker on one host and expose its webhook through Tailscale Funnel.
covers:
  - Dockerfile
  - .dockerignore
  - compose.yaml
  - backend/cmd/healthcheck/**
  - deploy/**
---

# Deploy

pollux runs as one container on a single host, started with Docker Compose. Tailscale Funnel runs on the host's own `tailscaled` (not in a container) and forwards public HTTPS to the container, so GitHub can reach `POST /webhook` without opening ports on the host.

The image is built in stages from static Go binaries and runs on distroless `static-debian12:nonroot` as uid 65532. There is no shell in it. The image includes a `/healthcheck` binary that the compose health check runs against `/healthz`.

`compose.yaml` at the repo root defines the `pollux` service:

- The port is published on `127.0.0.1:8080` only. Nothing but the host (and Funnel) can reach it.
- The named volume `pollux-data` is mounted at `/data`. The SQLite database lives at `/data/pollux.db` and holds the job queue and PR state.
- `restart: unless-stopped` brings it back after a reboot or crash.

## First deploy

1. Create the GitHub App by following [Registering the GitHub App](github-app.md). Keep the App ID, webhook secret, and downloaded `.pem`.
2. Store the key outside the repo, readable by the container user. The key is normally `0600` owned by you, which uid 65532 cannot read. Make the file world-readable inside a directory only you can enter; Docker mounts it as root, so the directory's mode doesn't stop the container, but it keeps other host users out:

   ```
   install -d -m 700 ~/.config/pollux-agent
   mv <key>.pem ~/.config/pollux-agent/github-app.pem
   chmod 0444 ~/.config/pollux-agent/github-app.pem
   ```

3. Clone the repo to `~/pollux-agent` (`git clone https://github.com/mrkizildag/pollux-agent ~/pollux-agent`); the update timer expects it there. Copy `backend/.env.example` to `.env` at the repo root and fill it in (variables are listed in [Setup](setup.md)). Compose uses `.env` both as the container's `env_file` and for its own interpolation. Set `GITHUB_APP_PRIVATE_KEY_FILE` to the **host** path of the key. Compose bind-mounts it read-only at `/run/secrets/github-app.pem` and overrides the variable inside the container. It also overrides `DATABASE_PATH` and `ADDR`, so those two values in `.env` are ignored under compose.
4. Start it:

   ```
   docker compose up -d --build
   ```

5. Enable Funnel. In the tailnet admin console, allow HTTPS certificates for the tailnet and grant the node the `funnel` node attribute in the policy file. Changing Funnel needs root unless you ran `sudo tailscale set --operator=$USER` once. It replaces whatever Funnel already serves on `/` of port 443. Then on the host:

   ```
   tailscale funnel --bg 8080
   tailscale funnel status
   ```

   `status` prints the public URL. `--bg` keeps the config across reboots. Funnel only serves ports 443, 8443, and 10000 publicly; here it proxies 443 to `localhost:8080`.
6. Set the App's webhook URL to `https://<host>.<tailnet>.ts.net/webhook`.

### Verify

```
curl -fsS http://127.0.0.1:8080/healthz
```

prints `ok`. Then in the App's "Advanced → Recent deliveries", redeliver the `ping`; it should return `202`.

## Updates

A systemd user timer deploys every merge to `main`. Every 2 minutes it fetches `origin/main`; when that differs from the last deployed commit, it checks the commit out (detached) and runs `docker compose up -d --build --wait`. When the build fails or the new container never turns healthy, the script redeploys the last good commit and records the bad one in `~/.local/state/pollux/failed`, so it isn't retried; the next merge, or deleting that file, tries again. A lock keeps a manual run from overlapping the timer's. The checkout must live at `~/pollux-agent` and carry no local changes. Whatever lands on `main` runs on this host next to the App key, so the repo's `protect main` ruleset requires a pull request and blocks force-pushes and deleting `main`; keep it on.

Install it once, after the first deploy:

```
mkdir -p ~/.config/systemd/user
ln -sf ~/pollux-agent/deploy/pollux-update.service ~/pollux-agent/deploy/pollux-update.timer ~/.config/systemd/user/
loginctl enable-linger
systemctl --user daemon-reload
systemctl --user enable --now pollux-update.timer
```

`enable-linger` keeps the timer running while you are logged out. Follow deploys with `journalctl --user -u pollux-update -f`. The last deployed commit is in `~/.local/state/pollux/deployed`; delete it to force a redeploy. To deploy by hand instead, run `deploy/pollux-update.sh`.

The volume keeps the database across rebuilds. Never run `docker compose down -v`: it deletes the volume, and with it the queue and PR state. `docker compose stop` shuts the server down gracefully (SIGTERM) and keeps everything. A deploy or stop interrupts any running job; it is requeued on the next start and may run twice (see [Job queue](../features/job-queue.md)), so a PR's check can get one extra run after a merge to `main`.

The database lives in the `pollux-data` volume. Backups are not covered here.

## Logs and status

- `docker compose ps` shows the service and whether it is `healthy`.
- `docker compose logs -f pollux` follows the logs, one JSON object per line (slog). `LOG_LEVEL` in `.env` controls verbosity.

## Troubleshooting

- **`permission denied` reading the private key**: the container runs as uid 65532. Re-run the `chmod` from step 2 on the host file that `GITHUB_APP_PRIVATE_KEY_FILE` points to.
- **Container `unhealthy`**: check `docker compose logs pollux` for why the server is not serving, then `curl http://127.0.0.1:8080/healthz` from the host. The distroless image has no shell, so `docker compose exec` into it will not work.
- **Exits right after start with a config error**: a required variable in `.env` is missing or invalid; the log line names it. See [Setup](setup.md). Remember `ADDR`, `DATABASE_PATH`, and the key path inside the container are set by compose.
- **A merge did not deploy**: `journalctl --user -u pollux-update -n 50` shows the failing step (fetch, checkout, build, or health wait).
- **Deliveries fail from GitHub but `/healthz` is fine**: run `tailscale funnel status` and confirm the URL and the `/webhook` path match the App's webhook URL.
