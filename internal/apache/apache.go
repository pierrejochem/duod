// Package apache owns one include file per app and switches traffic by
// rewriting it and running a graceful reload (open connections are kept).
package apache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"text/template"

	"github.com/pierrejochem/duod/internal/proxy"
	"github.com/pierrejochem/duod/internal/runner"
)

// Data is passed to the per-app template.
type Data struct {
	App             string
	ActiveSlot      string
	ActivePort      int
	ActiveRevision  string
	PreviewSlot     string // empty when there is no preview
	PreviewPort     int    // 0 when there is no preview
	PreviewRevision string
}

// PreviewOrActivePort lets templates expose a preview route that falls back
// to the active slot when no rollout is in progress.
func (d Data) PreviewOrActivePort() int {
	if d.PreviewPort != 0 {
		return d.PreviewPort
	}
	return d.ActivePort
}

func fromProxy(d proxy.Data) Data {
	return Data{
		App: d.App, ActiveSlot: d.Active.Name, ActivePort: d.Active.Port, ActiveRevision: d.Active.Revision,
		PreviewSlot: d.Preview.Name, PreviewPort: d.Preview.Port, PreviewRevision: d.Preview.Revision,
	}
}

// Site manages the include file of one app.
type Site struct {
	File       string
	Template   *template.Template
	ConfigTest []string
	Reload     []string
	Runner     runner.Runner
	HealthHost string
	HealthPath string
	// Lock is shared by all apps: configtest validates the WHOLE Apache
	// config, so two apps must never write+test+reload at the same time.
	Lock *sync.Mutex
}

var _ proxy.Proxy = (*Site)(nil)

// NewSite parses the template once at startup so typos fail fast.
func NewSite(app, file, tmpl string, configTest, reload []string, r runner.Runner) (*Site, error) {
	t, err := template.New(app).Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return nil, fmt.Errorf("app %s: proxy template: %w", app, err)
	}
	return &Site{File: file, Template: t, ConfigTest: configTest, Reload: reload, Runner: r,
		HealthHost: "127.0.0.1", HealthPath: "/", Lock: &sync.Mutex{}}, nil
}

// Kind implements proxy.Proxy.
func (s *Site) Kind() string { return "apache" }

// Render produces the desired file content.
func (s *Site) Render(d Data) ([]byte, error) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# Managed by duod for app %q - DO NOT EDIT (changes are reverted).\n", d.App)
	fmt.Fprintf(&buf, "# active=%s@%s preview=%s@%s\n", d.ActiveSlot, short(d.ActiveRevision), orNone(d.PreviewSlot), short(d.PreviewRevision))
	if err := s.Template.Execute(&buf, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// InSync reports whether the file on disk matches the desired content.
// Without an active slot Apache has nothing to route, so it is always in sync.
func (s *Site) InSync(pd proxy.Data) (bool, error) {
	if pd.Active.Name == "" {
		return true, nil
	}
	want, err := s.Render(fromProxy(pd))
	if err != nil {
		return false, err
	}
	have, err := os.ReadFile(s.File)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bytes.Equal(want, have), nil
}

// Apply writes the file atomically, validates the whole Apache config and
// gracefully reloads. If validation fails the previous file is restored and
// Apache is not touched, so traffic keeps flowing to the old slot.
func (s *Site) Apply(ctx context.Context, pd proxy.Data) error {
	if pd.Active.Name == "" {
		return nil
	}
	want, err := s.Render(fromProxy(pd))
	if err != nil {
		return err
	}
	s.Lock.Lock()
	defer s.Lock.Unlock()

	old, readErr := os.ReadFile(s.File)
	hadOld := readErr == nil

	if err := writeAtomic(s.File, want); err != nil {
		return err
	}
	if _, err := s.exec(ctx, s.ConfigTest); err != nil {
		if hadOld {
			_ = writeAtomic(s.File, old)
		} else {
			_ = os.Remove(s.File)
		}
		return fmt.Errorf("apache configtest failed, change reverted: %w", err)
	}
	if _, err := s.exec(ctx, s.Reload); err != nil {
		return fmt.Errorf("apache reload: %w", err)
	}
	return nil
}

// Targets probes the slot directly on its loopback port.
func (s *Site) Targets(sl proxy.Slot) []proxy.Target {
	return []proxy.Target{{
		Name: fmt.Sprintf("%s:%d%s", sl.Name, sl.Port, s.HealthPath),
		URL:  fmt.Sprintf("http://%s:%d%s", s.HealthHost, sl.Port, s.HealthPath),
	}}
}

// CheckSlot has nothing to verify for Apache.
func (s *Site) CheckSlot(context.Context, proxy.Slot) error { return nil }

func (s *Site) exec(ctx context.Context, argv []string) ([]byte, error) {
	return s.Runner.Run(ctx, runner.Cmd{Name: argv[0], Args: argv[1:]})
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".duod-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	if s == "" {
		return "-"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
