// Package runner executes external commands (git, docker compose, apachectl).
// It is an interface so the controller can be tested without Docker or Apache.
package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Cmd describes one command invocation.
type Cmd struct {
	Dir  string
	Env  []string // extra KEY=VALUE pairs appended to the process environment
	Name string
	Args []string
}

func (c Cmd) String() string {
	return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
}

// Runner runs a command and returns its stdout.
type Runner interface {
	Run(ctx context.Context, c Cmd) ([]byte, error)
}

// Exec is the real implementation backed by os/exec.
type Exec struct{}

// Run executes the command; on failure the error contains trimmed stderr.
func (Exec) Run(ctx context.Context, c Cmd) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return stdout.Bytes(), fmt.Errorf("%s: %w: %s", c.String(), err, msg)
	}
	return stdout.Bytes(), nil
}
