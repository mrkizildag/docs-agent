#!/usr/bin/env bash
set -euo pipefail
if ! [[ "$PR_NUMBER" =~ ^[0-9]+$ ]]; then
  echo "::error::PR_NUMBER must be a non-negative integer."
  exit 1
fi
out="$RUNNER_TEMP/pollux-agent"
workdir="$RUNNER_TEMP/pollux-agent-cwd"
mkdir -p "$out" "$workdir"
[ -n "$CLAUDE_CODE_OAUTH_TOKEN" ] || unset CLAUDE_CODE_OAUTH_TOKEN
[ -n "$ANTHROPIC_API_KEY" ] || unset ANTHROPIC_API_KEY

if [ "$PR_NUMBER" = "0" ]; then
  schema_file="$ACTION_PATH/scaffold.schema.json"
  prompt="$(cat "$ACTION_PATH/scaffold.md")

## Repository

- Repository checkout: $CHECKOUT
- Commit: $HEAD_SHA (checked out there)
- Default branch: $DEFAULT_BRANCH"
else
  docs_input() { jq -c -r --arg key "$1" -f "$ACTION_PATH/docs.jq" <<< "$DOCS"; }
  has_files="$(docs_input has_files)" || {
    echo "::error::The docs input is not a valid docs input: a JSON array of strings, or an object with review and uncovered arrays of strings, a base_sha string, and files of paths with start/end ranges."
    exit 1
  }
  docs_list="$(docs_input review)"
  uncovered_list="$(docs_input uncovered)"
  base_sha="$(docs_input base_sha)"
  if [ -z "$base_sha" ]; then
    diff_range="origin/$DEFAULT_BRANCH...HEAD"
  elif [[ "$base_sha" =~ ^[0-9a-f]{40}$ ]]; then
    diff_range="$base_sha...HEAD"
  else
    echo "::error::The docs input base_sha is not a 40-character hex commit SHA."
    exit 1
  fi
  git -C "$CHECKOUT" -c core.quotePath=false diff --no-color --no-ext-diff "$diff_range" -- > "$out/pr.diff"
  jq -c -f "$ACTION_PATH/new-doc-schema.jq" "$ACTION_PATH/result.schema.json" > "$out/result.schema.json"
  schema_file="$out/result.schema.json"
  awk -f "$ACTION_PATH/numbered-diff.awk" "$out/pr.diff" > "$out/pr.numbered.diff"
  if [ "$has_files" = true ]; then
    docs_input hunks > "$out/hunks.jsonl"
  else
    awk -f "$ACTION_PATH/diff-hunks.awk" "$out/pr.diff" > "$out/hunks.jsonl"
  fi
  if [ -s "$out/hunks.jsonl" ]; then
    jq -c --slurpfile hunks "$out/hunks.jsonl" -f "$ACTION_PATH/anchor-schema.jq" "$schema_file" > "$out/review.schema.json"
    if [ "$(wc -c < "$out/review.schema.json")" -le 102400 ]; then
      schema_file="$out/review.schema.json"
    else
      echo "::warning::The anchor schema exceeds 100 KiB; the anchor ranges were dropped, so comment anchors are not constrained to the diff."
    fi
  fi
  prompt="$(cat "$ACTION_PATH/prompt.md")

## Pull request

- Number: $PR_NUMBER
- Repository checkout: $CHECKOUT
- Head commit: $HEAD_SHA (checked out there)
- Default branch: $DEFAULT_BRANCH
- Docs to review (JSON-quoted paths at the head checkout):
$docs_list
- Changed files no doc covers (JSON-quoted paths):
$uncovered_list
- Diff of the head against the merge base, with head-side line numbers: $out/pr.numbered.diff"
fi

status=0
cd "$workdir"
claude -p "$prompt" \
  --output-format stream-json --verbose \
  --json-schema "$(cat "$schema_file")" \
  --tools Read,Grep,Glob \
  --permission-mode default \
  --setting-sources user \
  --strict-mcp-config \
  --disallowedTools Bash,Edit,Write,WebFetch,WebSearch,NotebookEdit,Task \
  --add-dir "$CHECKOUT" \
  --add-dir "$out" \
  < /dev/null > "$out/transcript.jsonl" || status=$?

jq -cR 'fromjson? | objects | select(.type == "result")' "$out/transcript.jsonl" | tail -n 1 > "$out/claude.json"

jq -n --arg head_sha "$HEAD_SHA" --arg nonce "$NONCE" --rawfile raw "$out/claude.json" \
  '{head_sha: $head_sha, nonce: $nonce,
    claude: ($raw | try fromjson catch {is_error: true, result: "claude produced no JSON output"})}' \
  > "$out/result.json"

token="$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
echo "::group::Agent trace"
echo "::stop-commands::$token"
jq -rR -f "$ACTION_PATH/trace.jq" "$out/transcript.jsonl" || true
echo "::$token::"
echo "::endgroup::"
exit "$status"
