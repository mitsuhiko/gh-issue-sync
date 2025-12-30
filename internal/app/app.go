package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mitsuhiko/gh-issue-sync/internal/config"
	"github.com/mitsuhiko/gh-issue-sync/internal/ghcli"
	"github.com/mitsuhiko/gh-issue-sync/internal/paths"
	"github.com/mitsuhiko/gh-issue-sync/internal/theme"
)

type App struct {
	Root   string
	Runner ghcli.Runner
	Now    func() time.Time
	Out    io.Writer
	Err    io.Writer
	Theme  *theme.Theme
}

type PullOptions struct {
	All   bool
	Force bool
	Full  bool // Force full sync, bypassing incremental
	Label []string
}

type PushOptions struct {
	DryRun     bool
	NoComments bool
}

type NewOptions struct {
	Labels []string
	Edit   bool
}

type CloseOptions struct {
	Reason string
}

type DiffOptions struct {
	Remote bool
}

type ViewOptions struct {
	Raw bool
}

type ListOptions struct {
	All       bool
	State     string
	Label     []string
	Assignee  string
	Author    string
	Milestone string
	Mention   string
	Limit     int
	Local     bool
	Modified  bool
	Search    string
}

func New(root string, runner ghcli.Runner, out io.Writer, errOut io.Writer) *App {
	return &App{
		Root:   root,
		Runner: runner,
		Now:    time.Now,
		Out:    out,
		Err:    errOut,
		Theme:  theme.Default(),
	}
}

func (a *App) Init(ctx context.Context, owner, repo string) error {
	if owner == "" || repo == "" {
		ownerGuess, repoGuess, err := a.detectRepoFromGit(ctx)
		if err != nil {
			return fmt.Errorf("unable to detect repo from git: %w (use --owner and --repo)", err)
		}
		if owner == "" {
			owner = ownerGuess
		}
		if repo == "" {
			repo = repoGuess
		}
	}

	// Default to placing .issues next to .git
	root := a.Root
	if gitRoot := paths.FindGitRoot(root); gitRoot != "" {
		root = gitRoot
	}

	p := paths.New(root)
	if err := p.EnsureLayout(); err != nil {
		return err
	}
	if _, err := os.Stat(p.ConfigPath); err == nil {
		return fmt.Errorf("config already exists at %s", p.ConfigPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cfg := config.Default(owner, repo)
	if err := config.Save(p.ConfigPath, cfg); err != nil {
		return err
	}
	t := a.Theme
	fmt.Fprintf(a.Out, "%s %s %s %s\n", t.SuccessText("Initialized"), t.AccentText(owner+"/"+repo), t.MutedText("in"), p.IssuesDir)
	return nil
}
