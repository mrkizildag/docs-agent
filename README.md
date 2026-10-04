# pollux

A GitHub App that keeps project docs up to date. On every pull request it reads the diff, finds the docs that describe the changed code, and proposes updates. A required check blocks the merge until the docs are updated or the change is waived.

**Status:** early development. The backend skeleton runs; the webhook flow is next.

## How it works

1. Docs live in the repo's `docs/` folder. Each doc declares the code it covers in its frontmatter.
2. When a PR changes covered code, pollux asks an LLM whether those docs still hold.
3. It proposes edits in a `pollux-agent` check run. Apply, edit, or waive them to unblock the merge.

The docs are plain markdown with relative links, so the folder also opens as an [Obsidian](https://obsidian.md) vault. See this repo's own [docs/](docs/README.md) for the structure.

## Roadmap

1. In-repo docs with a default structure and a required merge check.
2. Web dashboard (TypeScript + shadcn/ui) with configurable structures.
3. One-way sync to Notion.
4. Custom structures with custom prompts.

## Development

```sh
make check   # lint, test, govulncheck
make run     # backend on :8080
```

Details in [docs/guides/setup.md](docs/guides/setup.md).

## License

[MIT](LICENSE)
