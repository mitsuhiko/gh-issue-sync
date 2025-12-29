package ghcli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		cmdLine := name
		if len(args) > 0 {
			cmdLine = cmdLine + " " + strings.Join(args, " ")
		}
		stderrText := strings.TrimSpace(stderr.String())
		if stderrText != "" {
			return stdout.String(), fmt.Errorf("%s failed: %s", cmdLine, stderrText)
		}
		return stdout.String(), fmt.Errorf("%s failed: %w", cmdLine, err)
	}
	return stdout.String(), nil
}
