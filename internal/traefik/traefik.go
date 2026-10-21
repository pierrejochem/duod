// Package traefik switches traffic through Traefik's file provider.
//
// Every app owns exactly one file, <dynamicDir>/<app>.yaml, so apps never
// block each other: Traefik hot-reloads the directory and each file is
// written atomically. All routes of an app (web, api, admin, ...) live in
// that one file and therefore switch color together.
//
// Besides the public routers duod adds, on a private entrypoint:
//   - one probe router per running slot and route, selected by the request
//     header X-Duod-Target: <app>.<route>.<slot>, used for health checks
//     before any public traffic reaches a slot;
//   - a generation router; every probe response carries
//     X-Duod-Generation: <hash of the file>, which lets duod confirm that
//     Traefik really serves the new file before it reports a promotion.
package traefik

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/pierrejochem/duod/internal/config"
	"github.com/pierrejochem/duod/internal/health"
	"github.com/pierrejochem/duod/internal/proxy"
	"github.com/pierrejochem/duod/internal/runner"
)

// Header names used on the probe entrypoint.
const (
	TargetHeader     = "X-Duod-Target"
	GenCheckHeader   = "X-Duod-Generation-Check"
	GenerationHeader = "X-Duod-Generation"
)

// Backend implements proxy.Proxy for one app.
type Backend struct {
	App        string
	Routes     []config.Route
	Cfg        config.TraefikConfig
	HealthPath string
	Docker     []string // plain docker CLI prefix, e.g. ["docker"]
	Runner     runner.Runner
	Client     *http.Client
}

var _ proxy.Proxy = (*Backend)(nil)

// Kind implements proxy.Proxy.
func (b *Backend) Kind() string { return "traefik" }

// File is the dynamic configuration file of this app.
func (b *Backend) File() string { return filepath.Join(b.Cfg.DynamicDir, b.App+".yaml") }

// ---- rendering -----------------------------------------------------------

type dynamic struct {
	HTTP httpSection `yaml:"http"`
}

type httpSection struct {
	Routers     map[string]router     `yaml:"routers"`
	Services    map[string]service    `yaml:"services"`
	Middlewares map[string]middleware `yaml:"middlewares"`
}

type router struct {
	Rule        string         `yaml:"rule"`
	EntryPoints []string       `yaml:"entryPoints"`
	Service     string         `yaml:"service"`
	Middlewares []string       `yaml:"middlewares,omitempty"`
	Priority    int            `yaml:"priority,omitempty"`
	TLS         map[string]any `yaml:"tls,omitempty"`
}

type service struct {
	LoadBalancer loadBalancer `yaml:"loadBalancer"`
}

type loadBalancer struct {
	Servers        []server `yaml:"servers"`
	PassHostHeader bool     `yaml:"passHostHeader"`
}

type server struct {
	URL string `yaml:"url"`
}

type middleware struct {
	Headers headers `yaml:"headers"`
}

type headers struct {
	CustomResponseHeaders map[string]string `yaml:"customResponseHeaders"`
}

func (b *Backend) name(parts ...string) string {
	return "duod-" + b.App + "-" + strings.Join(parts, "-")
}

// Containers returns the compose container names of a route in a slot.
func (b *Backend) Containers(slot string, r config.Route) []string {
	out := make([]string, 0, r.Replicas)
	for i := 1; i <= r.Replicas; i++ {
		out = append(out, fmt.Sprintf("%s-%s-%s-%d", b.App, slot, r.Service, i))
	}
	return out
}

func (b *Backend) svc(slot string, r config.Route) service {
	lb := loadBalancer{PassHostHeader: true}
	for _, c := range b.Containers(slot, r) {
		lb.Servers = append(lb.Servers, server{URL: fmt.Sprintf("%s://%s:%d", r.Scheme, c, r.Port)})
	}
	return service{LoadBalancer: lb}
}

func probeValue(app, route, slot string) string { return app + "." + route + "." + slot }

// Render returns the file content and its generation id.
func (b *Backend) Render(d proxy.Data) ([]byte, string, error) {
	dyn := dynamic{HTTP: httpSection{
		Routers:     map[string]router{},
		Services:    map[string]service{},
		Middlewares: map[string]middleware{},
	}}
	genMW := b.name("generation")
	probeEP := []string{b.Cfg.ProbeEntryPoint}

	slots := []string{}
	for _, s := range []proxy.Slot{d.Active, d.Preview, d.Previous} {
		if s.Name != "" && !contains(slots, s.Name) {
			slots = append(slots, s.Name)
		}
	}
	for _, r := range b.Routes {
		for _, slot := range slots {
			svcName := b.name(r.Name, slot)
			dyn.HTTP.Services[svcName] = b.svc(slot, r)
			dyn.HTTP.Routers[b.name(r.Name, slot, "probe")] = router{
				Rule:        fmt.Sprintf("Header(`%s`, `%s`)", TargetHeader, probeValue(b.App, r.Name, slot)),
				EntryPoints: probeEP,
				Service:     svcName,
				Middlewares: []string{genMW},
				Priority:    10000,
			}
		}
		if d.Active.Name != "" {
			dyn.HTTP.Routers[b.name(r.Name)] = router{
				Rule: r.Rule, EntryPoints: r.EntryPoints, Service: b.name(r.Name, d.Active.Name),
				Middlewares: r.Middlewares, Priority: r.Priority, TLS: r.TLS,
			}
		}
		if d.Preview.Name != "" && r.PreviewRule != "" {
			dyn.HTTP.Routers[b.name(r.Name, "preview")] = router{
				Rule: r.PreviewRule, EntryPoints: r.EntryPoints, Service: b.name(r.Name, d.Preview.Name),
				Middlewares: r.Middlewares, Priority: r.Priority, TLS: r.TLS,
			}
		}
	}
	dyn.HTTP.Routers[b.name("generation")] = router{
		Rule:        fmt.Sprintf("Header(`%s`, `%s`)", GenCheckHeader, b.App),
		EntryPoints: probeEP,
		Service:     "noop@internal",
		Middlewares: []string{genMW},
		Priority:    10000,
	}

	// generation = hash of everything except the generation value itself
	dyn.HTTP.Middlewares[genMW] = middleware{Headers: headers{CustomResponseHeaders: map[string]string{GenerationHeader: "-"}}}
	body, err := yaml.Marshal(dyn)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	gen := hex.EncodeToString(sum[:])[:12]
	dyn.HTTP.Middlewares[genMW] = middleware{Headers: headers{CustomResponseHeaders: map[string]string{GenerationHeader: gen}}}
	body, err = yaml.Marshal(dyn)
	if err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# Managed by duod for app %q - DO NOT EDIT (changes are reverted).\n", b.App)
	fmt.Fprintf(&buf, "# generation=%s active=%s preview=%s previous=%s\n", gen,
		slotStr(d.Active), slotStr(d.Preview), slotStr(d.Previous))
	buf.Write(body)
	return buf.Bytes(), gen, nil
}

// ---- proxy.Proxy ----------------------------------------------------------

// InSync compares the file on disk with the desired content.
func (b *Backend) InSync(d proxy.Data) (bool, error) {
	if empty(d) {
		_, err := os.Stat(b.File())
		return errors.Is(err, os.ErrNotExist), nil
	}
	want, _, err := b.Render(d)
	if err != nil {
		return false, err
	}
	have, err := os.ReadFile(b.File())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bytes.Equal(want, have), nil
}

// Apply writes the file atomically and waits until Traefik serves it. If
// Traefik does not pick it up within ConfirmTimeout the old file is put back.
func (b *Backend) Apply(ctx context.Context, d proxy.Data) error {
	if empty(d) {
		if err := os.Remove(b.File()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	want, gen, err := b.Render(d)
	if err != nil {
		return err
	}
	old, readErr := os.ReadFile(b.File())
	if err := writeAtomic(b.File(), want); err != nil {
		return err
	}
	if err := b.confirm(ctx, gen, d); err != nil {
		if readErr == nil {
			_ = writeAtomic(b.File(), old)
		} else {
			_ = os.Remove(b.File())
		}
		return fmt.Errorf("traefik did not load the new configuration, change reverted: %w", err)
	}
	return nil
}

// confirm polls the probe entrypoint until a response carries the new
// generation id. The generation router (noop@internal) works even when no
// slot is healthy; the slot probe routers are a fallback.
func (b *Backend) confirm(ctx context.Context, gen string, d proxy.Data) error {
	ctx, cancel := context.WithTimeout(ctx, b.Cfg.ConfirmTimeout.Duration)
	defer cancel()
	targets := []proxy.Target{{
		Name:   "generation router",
		URL:    b.Cfg.ProbeURL + "/",
		Header: http.Header{GenCheckHeader: {b.App}},
	}}
	for _, s := range []proxy.Slot{d.Active, d.Preview} {
		if s.Name != "" {
			targets = append(targets, b.Targets(s)[0])
		}
	}
	var last string
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		for _, tg := range targets {
			_, hdr, err := health.Get(ctx, b.Client, tg)
			switch {
			case err != nil:
				last = err.Error()
			case hdr.Get(GenerationHeader) == gen:
				return nil
			default:
				last = fmt.Sprintf("%s answered generation %q, want %q", tg.Name, hdr.Get(GenerationHeader), gen)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("after %s: %s (is Traefik running with the file provider on %s and the %q entrypoint at %s?)",
				b.Cfg.ConfirmTimeout.Duration, last, b.Cfg.DynamicDir, b.Cfg.ProbeEntryPoint, b.Cfg.ProbeURL)
		case <-t.C:
		}
	}
}

// Targets returns one health check per route that has a health path; the
// first route always uses the app's healthCheck.path.
func (b *Backend) Targets(s proxy.Slot) []proxy.Target {
	var out []proxy.Target
	for i, r := range b.Routes {
		path := r.HealthPath
		if i == 0 && path == "" {
			path = b.HealthPath
		}
		if path == "" {
			continue
		}
		out = append(out, proxy.Target{
			Name:   fmt.Sprintf("%s/%s %s", r.Name, s.Name, path),
			URL:    b.Cfg.ProbeURL + path,
			Header: http.Header{TargetHeader: {probeValue(b.App, r.Name, s.Name)}},
		})
	}
	return out
}

// CheckSlot verifies that every routed container exists and is attached to
// the Traefik network, so a missing `networks:` entry fails fast with a
// clear message instead of a timed-out health check.
func (b *Backend) CheckSlot(ctx context.Context, s proxy.Slot) error {
	for _, r := range b.Routes {
		for _, c := range b.Containers(s.Name, r) {
			out, err := b.Runner.Run(ctx, runner.Cmd{
				Name: b.Docker[0],
				Args: append(append([]string{}, b.Docker[1:]...), "inspect", "--format", "{{json .NetworkSettings.Networks}}", c),
			})
			if err != nil {
				return fmt.Errorf("route %s: container %s is not running - check that compose service %q exists and starts (replicas: %d)", r.Name, c, r.Service, r.Replicas)
			}
			var nets map[string]json.RawMessage
			if err := json.Unmarshal(bytes.TrimSpace(out), &nets); err != nil {
				return fmt.Errorf("inspect %s: %w", c, err)
			}
			if _, ok := nets[b.Cfg.Network]; !ok {
				return fmt.Errorf("route %s: container %s is not attached to network %q - add it to service %q's networks and declare it external",
					r.Name, c, b.Cfg.Network, r.Service)
			}
		}
	}
	return nil
}

// EnsureNetwork creates the shared Docker network if it does not exist.
func EnsureNetwork(ctx context.Context, r runner.Runner, docker []string, name string) error {
	base := append([]string{}, docker[1:]...)
	if _, err := r.Run(ctx, runner.Cmd{Name: docker[0], Args: append(base, "network", "inspect", name)}); err == nil {
		return nil
	}
	_, err := r.Run(ctx, runner.Cmd{Name: docker[0], Args: append(base, "network", "create", name)})
	return err
}

// ---- helpers --------------------------------------------------------------

func empty(d proxy.Data) bool {
	return d.Active.Name == "" && d.Preview.Name == "" && d.Previous.Name == ""
}

func slotStr(s proxy.Slot) string {
	if s.Name == "" {
		return "none"
	}
	rev := s.Revision
	if len(rev) > 8 {
		rev = rev[:8]
	}
	return s.Name + "@" + rev
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// writeAtomic writes via a hidden temp file without a .yaml suffix, which
// Traefik's file provider ignores, then renames it into place.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".duod-*.tmp")
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
