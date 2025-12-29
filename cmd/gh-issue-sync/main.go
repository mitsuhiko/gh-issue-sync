package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jessevdk/go-flags"
	"github.com/mitsuhiko/gh-issue-sync/internal/app"
	"github.com/mitsuhiko/gh-issue-sync/internal/ghcli"
)

type Options struct {
	Init   InitCommand   `command:"init" description:"Initialize issue sync" long-description:"Create the .issues layout and config. If --owner/--repo are omitted, the git remote is used."`
	Pull   PullCommand   `command:"pull" description:"Pull issues from GitHub" long-description:"Fetch issues from GitHub and write/update local issue files."`
	Push   PushCommand   `command:"push" description:"Push local changes to GitHub" long-description:"Create or update GitHub issues based on local changes."`
	Status StatusCommand `command:"status" description:"Show sync status" long-description:"Show local changes and last full pull time."`
	New    NewCommand    `command:"new" description:"Create a new local issue" long-description:"Create a new local issue file. Use --edit to open an editor for the initial content."`
	Close  CloseCommand  `command:"close" description:"Mark an issue for closing" long-description:"Mark an issue as closed locally (use push to sync)." `
	Reopen ReopenCommand `command:"reopen" description:"Reopen a closed issue" long-description:"Mark an issue as open locally (use push to sync)."`
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
	Label []string `long:"label" value-name:"LABEL" description:"Filter by label (repeatable)"`
	Args  struct {
		Issues []string `positional-arg-name:"issue" description:"Issue numbers, local IDs, or paths to pull"`
	} `positional-args:"yes"`
}

type PushCommand struct {
	BaseCommand
	DryRun bool `long:"dry-run" description:"Show what would happen without pushing"`
	Args   struct {
		Issues []string `positional-arg-name:"issue" description:"Issue numbers, local IDs, or paths to push"`
	} `positional-args:"yes"`
}

type StatusCommand struct {
	BaseCommand
}

type NewCommand struct {
	BaseCommand
	Edit   bool     `long:"edit" description:"Open in $EDITOR before creating the file"`
	Labels []string `long:"label" value-name:"LABEL" description:"Add label (repeatable)"`
	Args   struct {
		Title string `positional-arg-name:"title" description:"Issue title (optional with --edit)"`
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

func (c *InitCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *PullCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *PushCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *StatusCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *NewCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *CloseCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *ReopenCommand) Usage() string {
	return "[OPTIONS]"
}

func (c *InitCommand) Execute(_ []string) error {
	return c.App.Init(context.Background(), c.Owner, c.Repo)
}

func (c *PullCommand) Execute(args []string) error {
	opts := app.PullOptions{All: c.All, Force: c.Force, Label: c.Label}
	if len(c.Args.Issues) > 0 {
		return c.App.Pull(context.Background(), opts, c.Args.Issues)
	}
	return c.App.Pull(context.Background(), opts, args)
}

func (c *PushCommand) Execute(args []string) error {
	opts := app.PushOptions{DryRun: c.DryRun}
	if len(c.Args.Issues) > 0 {
		return c.App.Push(context.Background(), opts, c.Args.Issues)
	}
	return c.App.Push(context.Background(), opts, args)
}

func (c *StatusCommand) Execute(_ []string) error {
	return c.App.Status(context.Background())
}

func (c *NewCommand) Execute(args []string) error {
	title := c.Args.Title
	if title == "" && len(args) > 0 {
		title = args[0]
	}
	return c.App.NewIssue(context.Background(), title, app.NewOptions{Edit: c.Edit, Labels: c.Labels})
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

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	application := app.New(cwd, ghcli.ExecRunner{}, os.Stdout, os.Stderr)
	opts := Options{}
	opts.Init.App = application
	opts.Pull.App = application
	opts.Push.App = application
	opts.Status.App = application
	opts.New.App = application
	opts.Close.App = application
	opts.Reopen.App = application

	parser := flags.NewParser(&opts, flags.HelpFlag|flags.PassDoubleDash)
	parser.ShortDescription = "Sync GitHub issues to local Markdown files."
	parser.LongDescription = "gh-issue-sync mirrors GitHub issues into a local .issues directory.\n\nUse init to create the layout, pull to fetch issues, edit files locally, and push to sync changes.\n\nExamples:\n  gh-issue-sync init --owner acme --repo roadmap\n  gh-issue-sync pull\n  gh-issue-sync new --edit\n  gh-issue-sync push"

	if len(os.Args) == 1 {
		parser.WriteHelp(os.Stdout)
		return
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
