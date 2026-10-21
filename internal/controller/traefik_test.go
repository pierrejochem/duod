package controller

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/pierrejochem/duod/internal/config"
	"github.com/pierrejochem/duod/internal/state"
	"github.com/pierrejochem/duod/internal/traefik"
)

// fakeTraefik serves the probe entrypoint by really parsing the dynamic
// files duod writes: header rule -> router -> service -> container name ->
// fake compose project. Freeze() simulates a Traefik that stops reloading.
type fakeTraefik struct {
	t      *testing.T
	dir    string
	docker *fakeDocker
	frozen atomic.Bool

	mu    sync.Mutex
	cache map[string][]byte // file -> last loaded content
}

func (ft *fakeTraefik) load() map[string]map[string]any {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if !ft.frozen.Load() {
		ft.cache = map[string][]byte{}
		files, _ := filepath.Glob(filepath.Join(ft.dir, "*.yaml"))
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err == nil {
				ft.cache[f] = raw
			}
		}
	}
	out := map[string]map[string]any{}
	for f, raw := range ft.cache {
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			ft.t.Errorf("traefik: invalid yaml in %s: %v", f, err)
			continue
		}
		out[f] = doc["http"].(map[string]any)
	}
	return out
}

func m(v any) map[string]any { r, _ := v.(map[string]any); return r }

func (ft *fakeTraefik) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, h := range ft.load() {
		routers, services, mws := m(h["routers"]), m(h["services"]), m(h["middlewares"])
		for _, rv := range routers {
			rt := m(rv)
			rule, _ := rt["rule"].(string)
			match := false
			for _, hdr := range []string{traefik.TargetHeader, traefik.GenCheckHeader} {
				if v := r.Header.Get(hdr); v != "" && strings.Contains(rule, "`"+hdr+"`") && strings.Contains(rule, "`"+v+"`") {
					match = true
				}
			}
			if !match {
				continue
			}
			for _, mw := range rt["middlewares"].([]any) {
				for k, v := range m(m(m(mws[mw.(string)])["headers"])["customResponseHeaders"]) {
					w.Header().Set(k, fmt.Sprint(v))
				}
			}
			if rt["service"] == "noop@internal" {
				w.WriteHeader(http.StatusTeapot)
				return
			}
			servers := m(m(services[rt["service"].(string)])["loadBalancer"])["servers"].([]any)
			url := m(servers[0])["url"].(string) // http://shop-green-web-1:3000
			host := strings.TrimPrefix(url, "http://")
			host = host[:strings.LastIndex(host, ":")]
			ft.docker.mu.Lock()
			ok := false
			for proj, running := range ft.docker.running {
				if running && ft.docker.healthy[proj] && strings.HasPrefix(host, proj+"-") {
					ok = true
				}
			}
			ft.docker.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

type tHarness struct {
	t       *testing.T
	ctrl    *Controller
	fake    *fakeDocker
	traefik *fakeTraefik
	repo    gitRepo
	dynDir  string
}

func newTraefikHarness(t *testing.T, extra string, apps ...string) *tHarness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	repo := gitRepo{t: t, dir: filepath.Join(tmp, "src")}
	_ = os.MkdirAll(repo.dir, 0o755)
	repo.run("init", "-q", "-b", "main")

	fake := newFakeDocker()
	dyn := filepath.Join(tmp, "traefik")
	ft := &fakeTraefik{t: t, dir: dyn, docker: fake}
	srv := httptest.NewServer(ft)
	t.Cleanup(srv.Close)

	var b strings.Builder
	fmt.Fprintf(&b, "stateDir: %s\n%s\ntraefik:\n  dynamicDir: %s\n  probeURL: %s\n  confirmTimeout: 400ms\napps:\n",
		filepath.Join(tmp, "state"), extra, dyn, srv.URL)
	for _, app := range apps {
		fmt.Fprintf(&b, `  - name: %[1]s
    source: {repoURL: %[2]s, revision: main, path: apps/%[1]s, pollInterval: 1h}
    healthCheck: {path: /health, interval: 10ms, timeout: 200ms, successThreshold: 3, failureThreshold: 2, progressDeadline: 500ms}
    strategy: {scaleDownDelay: 1h}
    proxy:
      type: traefik
      routes:
        - name: web
          port: 3000
          rule: Host(`+"`%[1]s.example.com`"+`)
          previewRule: Host(`+"`preview.%[1]s.example.com`"+`)
          tls: {certResolver: le}
        - name: api
          port: 8080
          replicas: 2
          rule: Host(`+"`%[1]s.example.com`"+`) && PathPrefix(`+"`/api`"+`)
          middlewares: [strip-api@file]
          healthPath: /api/health
`, app, repo.dir)
	}
	cfg, err := config.Parse([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := New(cfg, fake, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &tHarness{t: t, ctrl: ctrl, fake: fake, traefik: ft, repo: repo, dynDir: dyn}
}

func (h *tHarness) commit(app, msg, compose string) string {
	h.t.Helper()
	dir := filepath.Join(h.repo.dir, "apps", app)
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o644); err != nil {
		h.t.Fatal(err)
	}
	h.repo.run("add", "-A")
	h.repo.run("commit", "-q", "-m", msg)
	return h.repo.run("rev-parse", "HEAD")
}

func (h *tHarness) sync(app string) state.App {
	a, _ := h.ctrl.App(app)
	a.Refresh()
	a.reconcile(context.Background())
	return a.Snapshot()
}

// routedTo returns the service a public router points at, from the file.
func (h *tHarness) routedTo(app, route string) string {
	raw, err := os.ReadFile(filepath.Join(h.dynDir, app+".yaml"))
	if err != nil {
		return ""
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		h.t.Fatal(err)
	}
	r := m(m(m(doc["http"])["routers"])["duod-"+app+"-"+route])
	if r == nil {
		return ""
	}
	return r["service"].(string)
}

const tCompose = "services:\n  web: {image: shop, networks: [default, duod-edge]}\n  api: {image: api, deploy: {replicas: 2}, networks: [default, duod-edge]}\nnetworks:\n  duod-edge: {external: true}\n"

func TestTraefikMultiRouteBlueGreen(t *testing.T) {
	h := newTraefikHarness(t, "", "shop")
	v1 := h.commit("shop", "v1", tCompose)
	st := h.sync("shop")
	expect(t, st.Phase == state.PhaseHealthy && st.ActiveSlot == state.Blue && st.ActiveRevision == v1, "v1: %s %s", st.Phase, st.Message)
	expect(t, h.routedTo("shop", "web") == "duod-shop-web-blue" && h.routedTo("shop", "api") == "duod-shop-api-blue", "both routes on blue")

	raw, _ := os.ReadFile(filepath.Join(h.dynDir, "shop.yaml"))
	for _, want := range []string{
		"url: http://shop-blue-web-1:3000", "url: http://shop-blue-api-1:8080", "url: http://shop-blue-api-2:8080",
		"certResolver: le", "strip-api@file", "Host(`shop.example.com`) && PathPrefix(`/api`)",
		"entryPoints:\n      - duod", "X-Duod-Generation:",
	} {
		expect(t, strings.Contains(string(raw), want), "dynamic config misses %q:\n%s", want, raw)
	}

	v2 := h.commit("shop", "v2", tCompose+"# v2\n")
	st = h.sync("shop")
	expect(t, st.Phase == state.PhasePaused && st.PreviewSlot == state.Green && st.PreviewRevision == v2, "v2 paused: %s %s", st.Phase, st.Message)
	expect(t, h.routedTo("shop", "web") == "duod-shop-web-blue" && h.routedTo("shop", "api") == "duod-shop-api-blue", "traffic stays on blue while paused")
	expect(t, h.routedTo("shop", "web-preview") == "duod-shop-web-green", "preview router -> green")
	expect(t, h.routedTo("shop", "api-preview") == "", "api has no previewRule")

	a, _ := h.ctrl.App("shop")
	expect(t, a.Promote(context.Background()) == nil, "promote")
	expect(t, h.routedTo("shop", "web") == "duod-shop-web-green" && h.routedTo("shop", "api") == "duod-shop-api-green", "both routes switched together")
	expect(t, h.routedTo("shop", "web-preview") == "", "preview router removed")

	// rollback needs the previous slot's probe routers, which stay in the file
	expect(t, a.Rollback(context.Background()) == nil, "rollback")
	expect(t, h.routedTo("shop", "web") == "duod-shop-web-blue" && h.routedTo("shop", "api") == "duod-shop-api-blue", "rolled back to blue")
}

func TestTraefikNetworkCheck(t *testing.T) {
	h := newTraefikHarness(t, "", "shop")
	h.commit("shop", "v1", tCompose)
	h.sync("shop")
	v2 := h.commit("shop", "v2 forgot network", tCompose+"# NONET\n")
	st := h.sync("shop")
	expect(t, st.FailedRevision == v2 && st.ActiveSlot == state.Blue, "should fail: %s", st.Message)
	expect(t, strings.Contains(st.Message, "not attached to network \"duod-edge\""), "clear message: %s", st.Message)
	expect(t, h.routedTo("shop", "web") == "duod-shop-web-blue", "traffic untouched")
}

func TestTraefikNotLoadedReverts(t *testing.T) {
	h := newTraefikHarness(t, "", "shop")
	h.commit("shop", "v1", tCompose)
	h.sync("shop")
	h.commit("shop", "v2", tCompose+"# v2\n")
	h.sync("shop")
	before, _ := os.ReadFile(filepath.Join(h.dynDir, "shop.yaml"))

	h.traefik.frozen.Store(true) // Traefik stops picking up changes
	a, _ := h.ctrl.App("shop")
	err := a.Promote(context.Background())
	expect(t, err != nil && strings.Contains(err.Error(), "did not load"), "promote must fail: %v", err)
	after, _ := os.ReadFile(filepath.Join(h.dynDir, "shop.yaml"))
	expect(t, string(before) == string(after), "file must be reverted")
	st := a.Snapshot()
	expect(t, st.ActiveSlot == state.Blue && st.Phase == state.PhasePaused, "still blue + paused: %s %s", st.ActiveSlot, st.Phase)

	h.traefik.frozen.Store(false)
	expect(t, a.Promote(context.Background()) == nil, "promote works once Traefik reloads again")
}

// Two apps from one monorepo: a commit to one app's folder only rolls out
// that app; commits to both roll out concurrently and independently.
func TestConcurrentAppsMonorepo(t *testing.T) {
	h := newTraefikHarness(t, "", "shop", "billing")
	h.commit("shop", "shop v1", tCompose)
	billingV1 := h.commit("billing", "billing v1", tCompose)
	shop, _ := h.ctrl.App("shop")
	billing, _ := h.ctrl.App("billing")

	runBoth := func() (maxInFlight int64) {
		var wg sync.WaitGroup
		stop := make(chan struct{})
		var peak atomic.Int64
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
					if n := h.ctrl.RolloutsInFlight(); n > peak.Load() {
						peak.Store(n)
					}
					time.Sleep(time.Millisecond)
				}
			}
		}()
		for _, a := range []*App{shop, billing} {
			wg.Add(1)
			go func(a *App) {
				defer wg.Done()
				a.Refresh()
				a.reconcile(context.Background())
			}(a)
		}
		wg.Wait()
		close(stop)
		return peak.Load()
	}

	peak := runBoth()
	expect(t, shop.Snapshot().Phase == state.PhaseHealthy && billing.Snapshot().Phase == state.PhaseHealthy, "both deployed")
	expect(t, peak == 2, "both apps should deploy at the same time, peak in-flight = %d", peak)

	// only shop changes -> billing keeps its revision, no rollout
	h.fake.reset()
	h.commit("shop", "shop v2", tCompose+"# v2\n")
	runBoth()
	expect(t, shop.Snapshot().Phase == state.PhasePaused, "shop v2 paused, got %s", shop.Snapshot().Phase)
	b := billing.Snapshot()
	expect(t, b.Phase == state.PhaseHealthy && b.TargetRevision == billingV1 && b.Sync == state.Synced, "billing untouched: %s %s", b.Phase, b.Sync)
	expect(t, !h.fake.saw("-p billing-green"), "billing must not deploy: %v", h.fake.cmds)

	// independent files: each app has its own dynamic config
	files, _ := filepath.Glob(filepath.Join(h.dynDir, "*.yaml"))
	expect(t, len(files) == 2, "one file per app, got %v", files)
}

func TestMaxConcurrentRollouts(t *testing.T) {
	h := newTraefikHarness(t, "maxConcurrentRollouts: 1", "shop", "billing")
	h.commit("shop", "shop v1", tCompose)
	h.commit("billing", "billing v1", tCompose)
	var wg sync.WaitGroup
	var peak atomic.Int64
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				if n := h.ctrl.RolloutsInFlight(); n > peak.Load() {
					peak.Store(n)
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()
	for _, name := range []string{"shop", "billing"} {
		a, _ := h.ctrl.App(name)
		wg.Add(1)
		go func(a *App) { defer wg.Done(); a.Refresh(); a.reconcile(context.Background()) }(a)
	}
	wg.Wait()
	close(done)
	expect(t, peak.Load() == 1, "limit 1, peak %d", peak.Load())
	queued := 0
	for _, name := range []string{"shop", "billing"} {
		a, _ := h.ctrl.App(name)
		st := a.Snapshot()
		expect(t, st.Phase == state.PhaseHealthy, "%s deployed: %s %s", name, st.Phase, st.Message)
		for _, e := range st.History {
			if e.Type == "RolloutQueued" {
				queued++
			}
		}
	}
	expect(t, queued == 1, "exactly one app waited in the queue, got %d", queued)
}
