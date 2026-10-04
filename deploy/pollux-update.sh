#!/bin/sh
# Deploys origin/main whenever it moves; pollux-update.timer runs it.
set -eu

# The body runs from a function so the shell has parsed it all before
# `git switch` replaces this file on disk.
main() {
	cd "$(dirname "$0")/.."
	state="${XDG_STATE_HOME:-$HOME/.local/state}/pollux"
	mkdir -p "$state"
	exec 9>"$state/lock"
	flock -n 9 || exit 0

	git fetch --quiet origin main
	target=$(git rev-parse origin/main)
	deployed=$(cat "$state/deployed" 2>/dev/null || true)
	failed=$(cat "$state/failed" 2>/dev/null || true)
	if [ "$target" = "$deployed" ] || [ "$target" = "$failed" ]; then
		exit 0
	fi

	echo "deploying $target"
	git switch --quiet --detach "$target"
	if ! docker compose up -d --build --wait; then
		# Recording the failure stops the timer from retrying a commit that
		# builds but never turns healthy; the next merge clears it.
		echo "$target" >"$state/failed"
		if [ -n "$deployed" ]; then
			echo "deploy of $target failed; rolling back to $deployed"
			git switch --quiet --detach "$deployed"
			docker compose up -d --build --wait
		fi
		exit 1
	fi
	echo "$target" >"$state/deployed"
	echo "deployed $target"
}

main "$@"
