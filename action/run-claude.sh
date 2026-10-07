#!/usr/bin/env bash
set -euo pipefail
if ! [[ "$PR_NUMBER" =~ ^[0-9]+$ ]]; then
  echo "::error::PR_NUMBER must be a non-negative integer."
  exit 1
fi
out="$RUNNER_TEMP/pollux-agent"
workdir="$RUNNER_TEMP/pollux-agent-cwd"
mkdir -p "$workdir"
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
  docs_list="$(jq -r --arg key review -f "$ACTION_PATH/docs.jq" <<< "$DOCS")" && uncovered_list="$(jq -r --arg key uncovered -f "$ACTION_PATH/docs.jq" <<< "$DOCS")" || {
    echo "::error::The docs input is not a JSON array of strings or an object with review and uncovered arrays of strings."
    exit 1
  }
  schema_file="$ACTION_PATH/result.schema.json"
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
- Diff of the head against the merge base, unified format: $out/pr.diff"
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
