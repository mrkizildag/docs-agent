#!/bin/sh
# Deploys the latest origin/main on this host; rolls back if it doesn't come up healthy.
set -eu

# The body runs from a function so the shell has parsed it all before
# `git switch` replaces this file on disk.
main() {
	cd "${POLLUX_DIR:-$HOME/pollux-agent}"

	git fetch --quiet origin main
	previous=$(git rev-parse HEAD)
	target=$(git rev-parse origin/main)
	echo "deploying $(git log -1 --format='%h %s' "$target")"

	git switch --quiet --detach "$target"
	if ! docker compose up -d --build --wait; then
		echo "deploy of $target failed; rolling back to $previous" >&2
		git switch --quiet --detach "$previous"
		docker compose up -d --build --wait
		exit 1
	fi
	echo "deployed $target"
}

main "$@"
