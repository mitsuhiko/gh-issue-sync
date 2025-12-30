# gh-issue-sync

Sync GitHub issues to local Markdown files for offline editing, batch updates,
and seamless integration with coding agents.

The idea here is that you can bring in the state of a bunch of GitHub issues
locally so that you can refine them with an agent until you're satisfied and
sync back up the changes.  It can also be useful if you are working with an
internet for one reason or another and you want to have your issues readable
locally.

## Overview

`gh-issue-sync` mirrors GitHub issues into a local `.issues/` directory as
Markdown files with YAML front matter.  Edit issues in your favorite editor,
create new issues locally, and push changes back to GitHub when ready.

**Key features:**

- **Bidirectional sync** – Pull issues from GitHub, push local changes back
- **Local issue creation** – Create issues locally with temporary IDs (T1, T2), promote to real GitHub issues on push
- **Conflict detection** – Three-way merge logic prevents accidental overwrites
- **Folder-based state** – Move files between `open/` and `closed/` to change issue state
- **Leverages `gh` CLI** – Uses your existing GitHub CLI authentication

## Installation

### Prerequisites

- [Go 1.21+](https://go.dev/dl/)
- [GitHub CLI (`gh`)](https://cli.github.com/) installed and authenticated (`gh auth login`)

### Install with Go

```bash
go install github.com/mitsuhiko/gh-issue-sync/cmd/gh-issue-sync@latest
```

This installs the binary to `$GOBIN` (or `$GOPATH/bin`). Make sure it's in your `PATH`.

### Build from source

```bash
git clone https://github.com/mitsuhiko/gh-issue-sync.git
cd gh-issue-sync
go build -o gh-issue-sync ./cmd/gh-issue-sync
```

Optionally move the binary to your PATH:

```bash
mv gh-issue-sync ~/.local/bin/
# or
sudo mv gh-issue-sync /usr/local/bin/
```

## Quickstart

```bash
# Navigate to your project
cd my-project

# Initialize issue sync (auto-detects repo from git remote)
gh-issue-sync init

# Pull all open issues from GitHub
gh-issue-sync pull

# View your local issues
ls .issues/open/

# Edit an issue
$EDITOR .issues/open/123-fix-login-bug.md
gh-issue-sync edit 123

# Push your changes
gh-issue-sync push

# Or sync both ways (push then pull)
gh-issue-sync sync
```

## Creating Local Issues

Since the issue numbers come from github you can use temporary issue numbers
until then.  `T42` or `TABC` are all valid temporary issue IDs.  But they need
to start with a "T" so that we know they are temporary ones.  After synching
they are given new numbers and all references are updated.

### Sync Both Ways

Push and pull in a single command:

```bash
# Push local changes, then pull remote updates
gh-issue-sync sync

# Include closed issues
gh-issue-sync sync --all

# Filter by label
gh-issue-sync sync --label bug
```

## Sync Behavior

- New issues are created in `open/` or `closed/` based on their state
- Existing issues are updated if unchanged locally
- Local changes are preserved (conflicts are reported, not overwritten)
- Original versions are stored in `.sync/originals/` for conflict detection
- Local-only issues (T1, T2, etc.) are created on GitHub
- After creation, files are renamed with real issue numbers
- References like `#T1` in other issues are automatically updated

### Check Status

See what's changed locally:

```bash
gh-issue-sync status
```

### Create New Issues

Create issues locally before pushing to GitHub:

```bash
# Create with a title
gh-issue-sync new "My new feature idea"

# Create and open in editor
gh-issue-sync new "Fix login bug" --edit

# Create with labels
gh-issue-sync new "Critical bug" --label bug --label urgent

# Create with just the editor (no title required)
gh-issue-sync new --edit
```

Local issues get temporary IDs like `T1`, `T2`. When pushed, they become real
GitHub issues and files are renamed automatically.

### Close and Reopen Issues

```bash
# Close an issue (marks for closing on next push)
gh-issue-sync close 123

# Close with a reason
gh-issue-sync close 123 --reason not_planned

# Reopen a closed issue
gh-issue-sync reopen 456
```

Alternatively, move files manually:
- Move from `open/` to `closed/` to close
- Move from `closed/` to `open/` to reopen

## Issue File Format

Each issue is a Markdown file with YAML front matter:

```markdown
---
number: 123
title: Fix login bug on mobile Safari
labels:
  - bug
  - ios
assignees:
  - alice
  - bob
milestone: v2.0
type: Bug
state: open
state_reason:
synced_at: 2025-12-29T17:00:00Z
---

The login button doesn't respond to taps on iOS Safari 17.x.

## Steps to Reproduce

1. Open the app on iOS Safari
2. Tap the login button
3. Nothing happens

## Expected Behavior

Should open the login modal.
```

### Front Matter Fields

| Field | Type | Description | Editable |
|-------|------|-------------|----------|
| `number` | int/string | Issue number or local ID (T1, T2) | No (managed) |
| `title` | string | Issue title | Yes |
| `labels` | string[] | Label names | Yes |
| `assignees` | string[] | GitHub usernames | Yes |
| `milestone` | string | Milestone name | Yes |
| `type` | string | Issue type (org repos only) | Yes |
| `projects` | string[] | Project names | Yes |
| `state` | string | `open` or `closed` | Via folder |
| `state_reason` | string | `completed` or `not_planned` | Yes |
| `parent` | int | Parent issue number | Yes |
| `blocked_by` | int[] | Blocking issue numbers | Yes |
| `blocks` | int[] | Issues this blocks | Yes |
| `synced_at` | datetime | Last sync time | No (managed) |

### File Naming

Files are named `{number}-{slug}.md` where slug is derived from the title:
- `123-fix-login-bug.md`
- `T1-new-feature.md`

The slug is for readability only—the tool identifies issues by the number prefix.

## Conflict Handling

`gh-issue-sync` uses three-way comparison to detect conflicts:

| Local | Original | Remote | Action |
|-------|----------|--------|--------|
| Same | Same | Same | No action |
| Changed | Same | Same | Push local changes |
| Same | Same | Changed | Pull remote changes |
| Changed | Same | Changed | **Conflict** – skip with warning |

When a conflict occurs:
- On **pull**: Local changes are preserved, remote update is skipped
- On **push**: Remote changes are detected, local push is skipped
- Use `--force` on pull to overwrite local changes

## Agent Skill

`gh-issue-sync` includes a skill file for coding agents. Install it for your agent:

```bash
# For Claude Code / Codex
gh-issue-sync write-skill --agent codex

# For Pi (shittycodingagent.ai)
gh-issue-sync write-skill --agent pi

# For Claude Desktop
gh-issue-sync write-skill --agent claude

# For OpenCode
gh-issue-sync write-skill --agent opencode

# For Amp or other agents using ~/.config/agents/
gh-issue-sync write-skill --agent generic
```

Use `--scope` to choose between user-level (default) or project-level installation:

```bash
# Install to user home directory (default)
gh-issue-sync write-skill --agent codex --scope user

# Install to current project directory
gh-issue-sync write-skill --agent codex --scope project
```

| Agent | User Scope | Project Scope |
|-------|------------|---------------|
| `codex` | `~/.codex/skills/` | `.codex/skills/` |
| `pi` | `~/.pi/skills/` | `.pi/skills/` |
| `claude` | `~/.claude/skills/` | `.claude/skills/` |
| `opencode` | `~/.config/opencode/skill/` | `.opencode/skill/` |
| `amp`, `generic` | `~/.config/agents/skills/` | `.agents/skills/` |

To install to a custom location:

```bash
gh-issue-sync write-skill --output /path/to/skills/gh-issue-sync/
```

## License

MIT
