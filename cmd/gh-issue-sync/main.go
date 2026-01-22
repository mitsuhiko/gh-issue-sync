package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jessevdk/go-flags"
	"github.com/mitsuhiko/gh-issue-sync/internal/app"
	"github.com/mitsuhiko/gh-issue-sync/internal/ghcli"
	"github.com/mitsuhiko/gh-issue-sync/internal/paths"
	"github.com/mitsuhiko/gh-issue-sync/skill"
)

var version = "dev"

type Options struct {
	Version    bool              `long:"version" short:"v" description:"Show version"`
	Init       InitCommand       `command:"init" description:"Initialize issue sync" long-description:"Create the .issues layout and config. If --owner/--repo are omitted, the git remote is used."`
	Pull       PullCommand       `command:"pull" description:"Pull issues from GitHub" long-description:"Fetch issues from GitHub and write/update local issue files."`
	Push       PushCommand       `command:"push" description:"Push local changes to GitHub" long-description:"Create or update GitHub issues based on local changes."`
	Sync       SyncCommand       `command:"sync" description:"Pull and push issues" long-description:"Push local changes first, then pull updates from GitHub."`
	Status     StatusCommand     `command:"status" description:"Show sync status" long-description:"Show local changes and last full pull time."`
	List       ListCommand       `command:"list" alias:"ls" description:"List local issues" long-description:"Display a formatted list of local issues with filtering options."`
	New        NewCommand        `command:"new" description:"Create a new local issue" long-description:"Create a new local issue file. Use --edit to open an editor for the initial content."`
	Edit       EditCommand       `command:"edit" description:"Open an issue in your editor" long-description:"Open an issue file in your preferred editor ($VISUAL, $EDITOR, or git core.editor)."`
	View       ViewCommand       `command:"view" description:"View an issue" long-description:"Display an issue with nice formatting, showing metadata and body."`
	Close      CloseCommand      `command:"close" description:"Mark an issue for closing" long-description:"Mark an issue as closed locally (use push to sync)." `
	Reopen     ReopenCommand     `command:"reopen" description:"Reopen a closed issue" long-description:"Mark an issue as open locally (use push to sync)."`
	Diff       DiffCommand       `command:"diff" description:"Show diff between local and original/remote" long-description:"Show what changed in a local issue compared to the last synced version or current remote state."`
	WriteSkill WriteSkillCommand `command:"write-skill" description:"Write agent skill file" long-description:"Write the gh-issue-sync skill file for coding agents to the specified location."`
}

type BaseCommand struct {
	App *app.App
}

type InitCommand struct {
	BaseCommand
	Owner string `long:"owner" value-name:"OWNER" description:"GitHub owner (user or org)"`
	Repo  string `long:"repo" value-name:"REPO" description:"GitHub repository name"`
}

type PullCommand struct {
	BaseCommand
	All   bool     `long:"all" description:"Pull all issues (including closed)"`
	Force bool     `long:"force" description:"Overwrite local changes"`
	Full  bool     `long:"full" description:"Force full sync (bypass incremental)"`
	Label []string `long:"label" value-name:"LABEL" description:"Filter by label (repeatable)"`
	Args  struct {
		Issues []string `positional-arg-name:"issue" description:"Issue numbers, local IDs, or paths to pull"`
	} `positional-args:"yes"`
}

type PushCommand struct {
	BaseCommand
	DryRun     bool `long:"dry-run" description:"Show what would happen without pushing"`
	NoComments bool `long:"no-comments" description:"Skip posting pending comments"`
	Force      bool `long:"force" description:"Skip conflict detection and push anyway"`
	Args       struct {
		Issues []string `positional-arg-name:"issue" description:"Issue numbers, local IDs, or paths to push"`
	} `positional-args:"yes"`
}

type SyncCommand struct {
	BaseCommand
	All   bool     `long:"all" description:"Pull all issues (including closed)"`
	Full  bool     `long:"full" description:"Force full sync (bypass incremental)"`
	Label []string `long:"label" value-name:"LABEL" description:"Filter by label (repeatable)"`
}

type StatusCommand struct {
	BaseCommand
}

type ListCommand struct {
	BaseCommand
	All       bool     `long:"all" short:"a" description:"Include closed issues"`
	State     string   `long:"state" choice:"open" choice:"closed" description:"Filter by state"`
	Label     []string `long:"label" short:"l" value-name:"LABEL" description:"Filter by label (repeatable)"`
	Assignee  string   `long:"assignee" value-name:"USER" description:"Filter by assignee"`
	Author    string   `long:"author" short:"A" value-name:"USER" description:"Filter by author"`
	Milestone string   `long:"milestone" short:"M" value-name:"NAME" description:"Filter by milestone"`
	Mention   string   `long:"mention" value-name:"USER" description:"Filter by @mention in body"`
	Limit     int      `long:"limit" short:"L" value-name:"N" description:"Maximum number of issues to show"`
	Local     bool     `long:"local" description:"Show only local (unpushed) issues"`
	Modified  bool     `long:"modified" short:"m" description:"Show only modified issues"`
	Search    string   `long:"search" short:"S" value-name:"QUERY" description:"Search with GitHub-style query (e.g. 'error no:assignee sort:created-asc')"`
}

type NewCommand struct {
	BaseCommand
	Edit   bool     `long:"edit" description:"Open in $EDITOR before creating the file"`
	Labels []string `long:"label" value-name:"LABEL" description:"Add label (repeatable)"`
	Args   struct {
		Title string `positional-arg-name:"title" description:"Issue title (optional with --edit)"`
	} `positional-args:"yes"`
}

type EditCommand struct {
	BaseCommand
	Args struct {
		Number string `positional-arg-name:"issue" description:"Issue number or local ID" required:"yes"`
	} `positional-args:"yes"`
}

type CloseCommand struct {
	BaseCommand
	Reason string `long:"reason" choice:"completed" choice:"not_planned" value-name:"REASON" description:"Close reason (completed or not_planned)"`
	Args   struct {
		Number string `positional-arg-name:"issue" description:"Issue number or local ID" required:"yes"`
	} `positional-args:"yes"`
}

type ReopenCommand struct {
	BaseCommand
	Args struct {
		Number string `positional-arg-name:"issue" description:"Issue number or local ID" required:"yes"`
	} `positional-args:"yes"`
}

type ViewCommand struct {
	BaseCommand
	Raw  bool `long:"raw" description:"Show raw file content"`
	Args struct {
		Issue string `positional-arg-name:"issue" description:"Issue number, local ID, or path" required:"yes"`
	} `positional-args:"yes"`
}

type DiffCommand struct {
	BaseCommand
	Remote bool `long:"remote" description:"Diff against current remote state instead of last synced original"`
	Args   struct {
		Number string `positional-arg-name:"issue" description:"Issue number or local ID (omit to diff all)"`
	} `positional-args:"yes"`
}

type WriteSkillCommand struct {
	Output string `long:"output" short:"o" value-name:"DIR" description:"Output directory (overrides --agent)"`
	Agent  string `long:"agent" short:"a" value-name:"AGENT" description:"Target agent (codex, pi, claude, amp, opencode, generic)"`
	Scope  string `long:"scope" short:"s" value-name:"SCOPE" default:"user" description:"Scope: user (home dir) or project (current dir)"`
}

func (c *InitCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *PullCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *PushCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *SyncCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *StatusCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *ListCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *NewCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *EditCommand) Usage() string {
	return "<issue>"
}

func (c *CloseCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *ReopenCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *ViewCommand) Usage() string {
	return "[OPTIONS] <issue>"
}

func (c *DiffCommand) Usage() string {
	return "[OPTIONS] <issue>"
}

func (c *WriteSkillCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *InitCommand) Execute(_ []string) error {
	return c.App.Init(context.Background(), c.Owner, c.Repo)
}

func (c *PullCommand) Execute(args []string) error {
	opts := app.PullOptions{All: c.All, Force: c.Force, Full: c.Full, Label: c.Label}
	if len(c.Args.Issues) > 0 {
		return c.App.Pull(context.Background(), opts, c.Args.Issues)
	}
	return c.App.Pull(context.Background(), opts, args)
}

func (c *PushCommand) Execute(args []string) error {
	opts := app.PushOptions{DryRun: c.DryRun, NoComments: c.NoComments, Force: c.Force}
	if len(c.Args.Issues) > 0 {
		return c.App.Push(context.Background(), opts, c.Args.Issues)
	}
	return c.App.Push(context.Background(), opts, args)
}

func (c *SyncCommand) Execute(_ []string) error {
	ctx := context.Background()
	if err := c.App.Push(ctx, app.PushOptions{}, nil); err != nil {
		return err
	}
	return c.App.Pull(ctx, app.PullOptions{All: c.All, Force: true, Full: c.Full, Label: c.Label}, nil)
}

func (c *StatusCommand) Execute(_ []string) error {
	return c.App.Status(context.Background())
}

func (c *ListCommand) Execute(_ []string) error {
	opts := app.ListOptions{
		All:       c.All,
		State:     c.State,
		Label:     c.Label,
		Assignee:  c.Assignee,
		Author:    c.Author,
		Milestone: c.Milestone,
		Mention:   c.Mention,
		Limit:     c.Limit,
		Local:     c.Local,
		Modified:  c.Modified,
		Search:    c.Search,
	}
	return c.App.List(context.Background(), opts)
}

func (c *NewCommand) Execute(args []string) error {
	title := c.Args.Title
	if title == "" && len(args) > 0 {
		title = args[0]
	}
	return c.App.NewIssue(context.Background(), title, app.NewOptions{Edit: c.Edit, Labels: c.Labels})
}

func (c *EditCommand) Execute(args []string) error {
	number := c.Args.Number
	if number == "" && len(args) > 0 {
		number = args[0]
	}
	if strings.TrimSpace(number) == "" {
		return fmt.Errorf("issue number is required")
	}
	return c.App.Edit(context.Background(), number)
}

func (c *CloseCommand) Execute(args []string) error {
	number := c.Args.Number
	if number == "" && len(args) > 0 {
		number = args[0]
	}
	if strings.TrimSpace(number) == "" {
		return fmt.Errorf("issue number is required")
	}
	return c.App.Close(context.Background(), number, app.CloseOptions{Reason: c.Reason})
}

func (c *ReopenCommand) Execute(args []string) error {
	number := c.Args.Number
	if number == "" && len(args) > 0 {
		number = args[0]
	}
	if strings.TrimSpace(number) == "" {
		return fmt.Errorf("issue number is required")
	}
	return c.App.Reopen(context.Background(), number)
}

func (c *ViewCommand) Execute(args []string) error {
	issue := c.Args.Issue
	if issue == "" && len(args) > 0 {
		issue = args[0]
	}
	if strings.TrimSpace(issue) == "" {
		return fmt.Errorf("issue is required")
	}
	return c.App.View(context.Background(), issue, app.ViewOptions{Raw: c.Raw})
}

func (c *DiffCommand) Execute(args []string) error {
	number := c.Args.Number
	if number == "" && len(args) > 0 {
		number = args[0]
	}
	if strings.TrimSpace(number) == "" {
		return c.App.DiffAll(context.Background(), app.DiffOptions{Remote: c.Remote})
	}
	return c.App.Diff(context.Background(), number, app.DiffOptions{Remote: c.Remote})
}

func (c *WriteSkillCommand) Execute(args []string) error {
	outputDir := c.Output
	if outputDir == "" {
		if c.Agent == "" {
			return fmt.Errorf("either --agent or --output is required")
		}
		if c.Scope != "user" && c.Scope != "project" {
			return fmt.Errorf("scope must be 'user' or 'project'")
		}

		var baseDir string
		if c.Scope == "user" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("cannot determine home directory: %w", err)
			}
			baseDir = home
		} else {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("cannot determine current directory: %w", err)
			}
			baseDir = cwd
		}

		var agentDir string
		skillSubdir := "skills"
		switch c.Agent {
		case "codex":
			agentDir = ".codex"
		case "pi":
			agentDir = ".pi"
		case "claude":
			agentDir = ".claude"
		case "opencode":
			if c.Scope == "user" {
				agentDir = filepath.Join(".config", "opencode")
			} else {
				agentDir = ".opencode"
			}
			skillSubdir = "skills"
		case "amp", "generic":
			if c.Scope == "user" {
				agentDir = filepath.Join(".config", "agents")
			} else {
				agentDir = ".agents"
			}
		default:
			return fmt.Errorf("unknown agent: %s", c.Agent)
		}
		outputDir = filepath.Join(baseDir, agentDir, skillSubdir, "gh-issue-sync")
	}

	skillPath := filepath.Join(outputDir, "SKILL.md")

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", outputDir, err)
	}

	if err := os.WriteFile(skillPath, []byte(skill.SkillContent), 0644); err != nil {
		return fmt.Errorf("failed to write skill file: %w", err)
	}

	fmt.Printf("Wrote skill to: %s\n", skillPath)
	return nil
}

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	// Find the root directory for .issues
	// For init, we'll use git root; for other commands, find existing .issues
	root := paths.FindIssuesDir(cwd)
	if root == "" {
		// Fall back to cwd (init will handle finding git root)
		root = cwd
	}

	application := app.New(root, ghcli.ExecRunner{}, os.Stdout, os.Stderr)
	opts := Options{}
	opts.Init.App = application
	opts.Pull.App = application
	opts.Push.App = application
	opts.Sync.App = application
	opts.Status.App = application
	opts.List.App = application
	opts.New.App = application
	opts.Edit.App = application
	opts.View.App = application
	opts.Close.App = application
	opts.Reopen.App = application
	opts.Diff.App = application

	parser := flags.NewParser(&opts, flags.HelpFlag|flags.PassDoubleDash)
	parser.ShortDescription = "Sync GitHub issues to local Markdown files."
	parser.LongDescription = "gh-issue-sync mirrors GitHub issues into a local .issues directory.\n\nUse init to create the layout, pull to fetch issues, edit files locally, and push to sync changes.\n\nExamples:\n  gh-issue-sync init --owner acme --repo roadmap\n  gh-issue-sync pull\n  gh-issue-sync new --edit\n  gh-issue-sync push"

	if len(os.Args) == 1 {
		parser.WriteHelp(os.Stdout)
		return
	}

	// Handle --version before parsing (go-flags doesn't support version flag natively)
	for _, arg := range os.Args[1:] {
		if arg == "-v" || arg == "--version" {
			fmt.Println(version)
			return
		}
	}

	if _, err := parser.Parse(); err != nil {
		if flagsErr, ok := err.(*flags.Error); ok {
			if flagsErr.Type == flags.ErrHelp {
				fmt.Fprint(os.Stdout, flagsErr.Message)
				return
			}
			fmt.Fprintf(os.Stderr, "error: %s\n\n", flagsErr.Message)
			parser.WriteHelp(os.Stderr)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		fmt.Fprintln(os.Stderr, "hint: run `gh-issue-sync --help` for usage")
		os.Exit(1)
	}
}
