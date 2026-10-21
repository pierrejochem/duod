// Package compose drives one Docker Compose project per slot
// (<app>-blue / <app>-green) through the docker CLI.
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"

	"github.com/pierrejochem/duod/internal/runner"
)

// Project is a Compose project bound to one slot.
type Project struct {
	Prefix []string // e.g. ["docker", "compose"]
	Name   string   // compose project name, e.g. "shop-green"
	Dir    string   // project directory (materialized Git path)
	File   string   // compose file name relative to Dir
	Pull   string   // always|missing|never
	App    string
	Slot   string
	Port   int
	Rev    string
	Runner runner.Runner
}

// Env is passed to every compose call so the compose file can use
// ${BG_PORT}, ${BG_SLOT}, ${BG_REVISION} and ${BG_APP}.
func (p *Project) Env() []string {
	return []string{
		"BG_APP=" + p.App,
		"BG_SLOT=" + p.Slot,
		"BG_PORT=" + strconv.Itoa(p.Port),
		"BG_REVISION=" + p.Rev,
		"COMPOSE_PROJECT_NAME=" + p.Name,
	}
}

func (p *Project) run(ctx context.Context, args ...string) ([]byte, error) {
	base := append([]string{}, p.Prefix[1:]...)
	base = append(base, "-p", p.Name, "--project-directory", p.Dir, "-f", p.File)
	return p.Runner.Run(ctx, runner.Cmd{
		Dir:  p.Dir,
		Env:  p.Env(),
		Name: p.Prefix[0],
		Args: append(base, args...),
	})
}

// Up creates or updates the slot's containers.
func (p *Project) Up(ctx context.Context) error {
	_, err := p.run(ctx, "up", "-d", "--remove-orphans", "--force-recreate", "--pull", p.Pull)
	return err
}

// Start restarts stopped containers of a slot that was scaled down.
func (p *Project) Start(ctx context.Context) error {
	_, err := p.run(ctx, "up", "-d", "--no-recreate", "--remove-orphans")
	return err
}

// Stop scales the slot down but keeps containers for a fast rollback.
func (p *Project) Stop(ctx context.Context) error {
	_, err := p.run(ctx, "stop")
	return err
}

// Down removes the slot's containers and networks (never volumes).
func (p *Project) Down(ctx context.Context) error {
	_, err := p.run(ctx, "down", "--remove-orphans")
	return err
}

// Container is the subset of `compose ps --format json` we care about.
type Container struct {
	Name    string `json:"Name"`
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
}

// PS lists the slot's containers. Handles both the JSON-array and the
// newline-delimited output formats of different compose versions.
func (p *Project) PS(ctx context.Context) ([]Container, error) {
	out, err := p.run(ctx, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, nil
	}
	var list []Container
	if out[0] == '[' {
		err = json.Unmarshal(out, &list)
		return list, err
	}
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var c Container
		if err := json.Unmarshal(line, &c); err != nil {
			return nil, err
		}
		list = append(list, c)
	}
	return list, nil
}
