# pollux

A GitHub App that keeps project docs up to date. On every pull request it reads the diff, finds the docs that describe the changed code, and proposes updates. A required `pollux-agent` check blocks the merge until the docs are updated or the change is skipped.

**Status:** phase 1 (in-repo docs and the merge check) is nearly done. Analysis, proposals, Apply, Skip, and failure handling with Re-run work end to end. Still open: a starter `docs/` folder for repos that have none.

## How it works

1. Docs live in the repo's `docs/` folder. Each doc lists the code it covers in its frontmatter.
2. When a PR changes covered code, pollux checks whether those docs still describe it, using an LLM agent with read-only access to the PR's code.
3. Each proposed edit becomes a review comment on the PR, and one summary comment lists them all. Until they are dealt with, the check is `action_required`.
4. Anyone with write access can apply one edit or all of them, which commits to the PR branch. They can also skip the check for one commit or for the whole PR, with a reason. Apply and Skip are checkboxes in the comments, or the `/pollux-agent apply`, `skip <reason>`, and `skip-pr <reason>` commands.

Analysis runs in one of two places. Repos that add the pollux-agent workflow run it in their own GitHub Actions with Claude Code and their own Claude credential ([guide](docs/guides/actions-runner.md)). All other repos are analyzed on the pollux server with its own LLM key.

The docs are plain markdown with relative links, so the folder also opens as an [Obsidian](https://obsidian.md) vault. This repo's own [docs/](docs/README.md) show the structure.

## Roadmap

1. In-repo docs with a default structure and a required merge check.
2. Web dashboard (TypeScript + shadcn/ui) with configurable structures.
3. One-way sync to Notion.
4. Custom structures with custom prompts.
5. Agentic reviews.
6. More to come.

## Development

```sh
make check   # lint, test, govulncheck
make run     # backend on :8080
```

Details are in [docs/guides/setup.md](docs/guides/setup.md). To register the GitHub App, see [docs/guides/github-app.md](docs/guides/github-app.md). To deploy, see [docs/guides/deploy.md](docs/guides/deploy.md).

## License

[MIT](LICENSE)
