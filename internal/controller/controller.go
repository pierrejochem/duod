package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/pierrejochem/duod/internal/config"
	"github.com/pierrejochem/duod/internal/runner"
	"github.com/pierrejochem/duod/internal/state"
	"github.com/pierrejochem/duod/internal/traefik"
)

// Controller owns one App reconciler per configured application.
type Controller struct {
	apps   map[string]*App
	names  []string
	log    *slog.Logger
	cfg    *config.Config
	run    runner.Runner
	shared *shared

	mu   sync.Mutex
	root context.Context
}

// New builds reconcilers for all apps in cfg.
func New(cfg *config.Config, r runner.Runner, log *slog.Logger) (*Controller, error) {
	sh := &shared{apacheLock: &sync.Mutex{}, inFlight: &atomicCounter{}}
	if cfg.MaxConcurrentRollouts > 0 {
		sh.sem = make(chan struct{}, cfg.MaxConcurrentRollouts)
	}
	c := &Controller{apps: map[string]*App{}, log: log, cfg: cfg, run: r, shared: sh, root: context.Background()}
	for _, ac := range cfg.Apps {
		a, err := newApp(cfg, ac, r, log, sh)
		if err != nil {
			return nil, err
		}
		c.apps[ac.Name] = a
		c.names = append(c.names, ac.Name)
	}
	sort.Strings(c.names)
	return c, nil
}

// Run starts all reconcile loops and blocks until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	c.mu.Lock()
	c.root = ctx
	c.mu.Unlock()
	if c.usesTraefik() {
		if err := traefik.EnsureNetwork(ctx, c.run, dockerCLI(c.cfg.Docker), c.cfg.Traefik.Network); err != nil {
			c.log.Warn("could not ensure traefik network", "network", c.cfg.Traefik.Network, "err", err)
		}
	}
	var wg sync.WaitGroup
	for _, a := range c.apps {
		wg.Add(1)
		go func(a *App) {
			defer wg.Done()
			a.Run(ctx)
		}(a)
	}
	wg.Wait()
}

// Context is the long-lived context actions run under (not a request context,
// so a disconnecting HTTP client can't abort a promotion half way).
func (c *Controller) Context() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.root
}

// App returns the reconciler for name.
func (c *Controller) App(name string) (*App, error) {
	a, ok := c.apps[name]
	if !ok {
		return nil, fmt.Errorf("unknown app %q", name)
	}
	return a, nil
}

// List returns snapshots of all apps, sorted by name.
func (c *Controller) List() []state.App {
	out := make([]state.App, 0, len(c.names))
	for _, n := range c.names {
		out = append(out, c.apps[n].Snapshot())
	}
	return out
}

func (c *Controller) usesTraefik() bool {
	for _, a := range c.cfg.Apps {
		if a.Proxy.Type == "traefik" {
			return true
		}
	}
	return false
}

// RefreshAll forces a Git fetch on every app (global webhook). Apps whose
// path did not change in the new commit keep their revision, so only the
// touched services roll out, and those roll out concurrently.
func (c *Controller) RefreshAll() int {
	for _, a := range c.apps {
		a.Refresh()
	}
	return len(c.apps)
}

// GlobalWebhookSecret is the secret of /api/v1/webhook.
func (c *Controller) GlobalWebhookSecret() string { return c.cfg.WebhookSecret }

// RolloutsInFlight is the number of apps currently deploying/analysing.
func (c *Controller) RolloutsInFlight() int64 { return c.shared.inFlight.Load() }

// WebhookSecret returns the configured secret for an app.
func (c *Controller) WebhookSecret(name string) string {
	if a, ok := c.apps[name]; ok {
		return a.cfg.Source.WebhookSecret
	}
	return ""
}
