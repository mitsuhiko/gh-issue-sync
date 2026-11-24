# Development Rules

## First Message
If the user did not give you a concrete task in their first message, read README.md

## Commands
- NEVER commit unless user asks

## GitHub Issues
- We're dogfooding hard here, so always read them locally (unless you need comments)

go run ./cmd/gh-issue-sync pull
cat .issues/open/NUMBER-*.md

Don't use `gh issue view 42`, use `cat .issues/open/42-*.md` (unless you notice it's missing)

## Tools
- Use our own github issue sync (go run ./cmd/gh-issue-sync) for most operations
- GitHub CLI for issue comments/PRs
- TUI interaction: use tmux

## Style
- Keep answers short and concise
- No emojis in commits, issues, PR comments, or code
- No fluff or cheerful filler text

## Local Installation

From the repository root, use the Go version specified in `go.mod` and run
`make install`. It replaces an existing `gh-issue-sync` on your `PATH` or detects
the first existing directory on your `PATH` among `~/.local/bin`, `~/.bin`, and
`~/bin`. If detection fails, add a local bin directory to your `PATH` and retry.

Verify the install with `command -v gh-issue-sync` and `gh-issue-sync --version`.
