# Issue File Format

This document describes the file format used by `gh-issue-sync` to store GitHub
issues locally.

## Directory Structure

Issues are stored in `.issues/` at the repository root:

```
.issues/
├── open/                    # Open issues
│   ├── 42-fix-login-bug.md
│   ├── 123-add-feature.md
│   └── T1-new-idea.md       # Local issue (not yet pushed)
├── closed/                  # Closed issues
│   └── 99-old-bug.md
└── .sync/                   # Internal sync state (do not edit)
    └── originals/           # Original versions for conflict detection
        ├── 42.md
        └── 123.md
```

Moving a file between `open/` and `closed/` changes the issue state on the next
push.

## File Naming

Files are named `{number}-{slug}.md`:

- **number**: The GitHub issue number (e.g., `42`) or a temporary local ID
  (e.g., `T1`, `Tabc`)
- **slug**: A URL-friendly version of the title, for readability only

Examples:
- `42-fix-login-bug.md`
- `123-add-new-feature.md`
- `T1-draft-idea.md`

The tool identifies issues by the number prefix only. You can rename the slug
portion without affecting sync.

## File Structure

Each issue file consists of YAML front matter followed by the issue body:

```markdown
---
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
synced_at: 2025-01-15T10:30:00Z
info:
  author: charlie
  created_at: 2025-01-10T08:00:00Z
  updated_at: 2025-01-14T16:45:00Z
---

The issue body goes here. This supports full GitHub-flavored Markdown.

## Steps to Reproduce

1. Open Safari on iOS 17
2. Navigate to login page
3. Enter credentials and tap "Login"

## Expected Behavior

User should be logged in successfully.
```

## Front Matter Fields

### Editable Fields

These fields can be modified locally and will sync to GitHub:

| Field | Type | Description |
|-------|------|-------------|
| `title` | string | Issue title (required) |
| `labels` | string[] | List of label names |
| `assignees` | string[] | List of GitHub usernames |
| `milestone` | string | Milestone name |
| `type` | string | Issue type (organization repositories only) |
| `projects` | string[] | Project names to add the issue to |
| `state_reason` | string | `completed` or `not_planned` (for closed issues) |
| `parent` | int/string | Parent issue number for sub-issues |
| `blocked_by` | int/string[] | List of blocking issue numbers |
| `blocks` | int/string[] | List of issue numbers this blocks |

### Managed Fields

These fields are managed by the tool and should not be edited manually:

| Field | Type | Description |
|-------|------|-------------|
| `state` | string | `open` or `closed` (derived from folder location) |
| `synced_at` | datetime | Last sync timestamp (ISO 8601 format) |

### Informational Fields (Read-Only)

These fields are synced from GitHub for reference but are never pushed back:

| Field | Type | Description |
|-------|------|-------------|
| `info.author` | string | GitHub username who created the issue |
| `info.created_at` | datetime | When the issue was created |
| `info.updated_at` | datetime | When the issue was last updated |

## Field Details

### Labels

Labels are specified as a YAML list:

```yaml
labels:
  - bug
  - priority:high
  - area/auth
```

Missing labels are created automatically on push.

### Assignees

GitHub usernames of people assigned to the issue:

```yaml
assignees:
  - alice
  - bob
```

### Milestone

The milestone name (not ID):

```yaml
milestone: v2.0
```

Missing milestones are created automatically on push.

### Issue Type

For organization repositories that support issue types:

```yaml
type: Bug
```

Common types: `Bug`, `Feature`, `Task`, `Epic`.

### State and State Reason

The `state` field reflects the folder location:
- Files in `open/` have `state: open`
- Files in `closed/` have `state: closed`

For closed issues, `state_reason` indicates why:

```yaml
state: closed
state_reason: completed    # Issue was resolved
# or
state_reason: not_planned  # Issue won't be fixed
```

### Issue Relationships

For sub-issues and dependencies (requires GitHub's sub-issues feature):

```yaml
parent: 100              # This is a sub-issue of #100
blocked_by:              # This issue is blocked by:
  - 101
  - 102
blocks:                  # This issue blocks:
  - 105
  - 106
```

References can use temporary IDs for local issues:

```yaml
blocked_by:
  - T1
  - 101
```

## Temporary Issue IDs

New issues created locally use temporary IDs starting with `T`:

- `T1`, `T2`, `T3` - simple sequential IDs
- `Tabc`, `Txyz` - any alphanumeric suffix works

When pushed to GitHub:
1. The issue receives a real number
2. The file is renamed (e.g., `T1-feature.md` becomes `42-feature.md`)
3. References like `#T1` in other issues are updated to `#42`

## Issue Body

Everything after the front matter closing `---` is the issue body. It supports
full GitHub-flavored Markdown:

- Headers, lists, code blocks
- Task lists (`- [ ]` and `- [x]`)
- Tables, blockquotes
- Images and links
- @mentions and #references

The body is normalized on save:
- Windows line endings (`\r\n`) are converted to Unix (`\n`)
- Leading blank lines are trimmed
- A trailing newline is ensured

## Pending Comments

To queue a comment for an issue, create a file named `{number}.comment.md`:

```
.issues/open/42.comment.md
.issues/open/42-fix-login-bug.comment.md  # Also valid
```

The comment file contains plain Markdown:

```markdown
Updated the acceptance criteria based on the product review.

cc @alice @bob
```

On push:
1. The comment is posted to the GitHub issue
2. The `.comment.md` file is deleted

Use `--no-comments` with push to skip posting comments.

## Conflict Detection

The `.issues/.sync/originals/` directory stores the last-synced version of each
issue. This enables three-way conflict detection:

| Local | Original | Remote | Result |
|-------|----------|--------|--------|
| Same | Same | Same | No action needed |
| Changed | Same | Same | Local changes are pushed |
| Same | Same | Changed | Remote changes are pulled |
| Changed | Same | Changed | Conflict - operation skipped |

Original files use simple names like `42.md` (no slug).

## Examples

### Minimal Issue

```markdown
---
title: Add dark mode
---

Support dark mode in the application.
```

### Complete Issue

```markdown
---
title: Authentication fails on Safari
labels:
  - bug
  - browser-compat
  - priority:high
assignees:
  - alice
milestone: v2.1
type: Bug
state: open
state_reason:
parent: 50
blocked_by:
  - 48
  - 49
synced_at: 2025-01-15T10:30:00Z
info:
  author: charlie
  created_at: 2025-01-10T08:00:00Z
  updated_at: 2025-01-14T16:45:00Z
---

## Description

Users on Safari 17+ cannot log in. The session cookie is not being set.

## Steps to Reproduce

1. Open Safari 17 on macOS
2. Navigate to /login
3. Enter valid credentials
4. Click "Sign In"

**Expected:** User is logged in and redirected to dashboard
**Actual:** Page refreshes, user remains logged out

## Environment

- Safari 17.2
- macOS Sonoma 14.2

## Workaround

Users can use Chrome or Firefox in the meantime.
```

### Local Issue (Not Yet Pushed)

```markdown
---
title: Improve error messages
labels:
  - enhancement
  - dx
assignees: []
---

Error messages should include:
- Error code for support reference
- Link to documentation
- Suggested next steps
```

File: `.issues/open/T1-improve-error-messages.md`
