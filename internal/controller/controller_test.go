package controller

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pierrejochem/duod/internal/config"
	"github.com/pierrejochem/duod/internal/runner"
	"github.com/pierrejochem/duod/internal/state"
)

// fakeDocker runs git for real but simulates docker compose and apachectl.
// A slot is "healthy" when its compose file does not contain "BROKEN".
type fakeDocker struct {
	mu           sync.Mutex
	real         runner.Exec
	running      map[string]bool // compose project -> running
	healthy      map[string]bool
	rev          map[string]string
	cmds         []string
	failApache   bool
	portProject  map[int]string
	noNet        map[string]bool
	networkCalls []string
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{running: map[string]bool{}, healthy: map[string]bool{}, rev: map[string]string{}, noNet: map[string]bool{}, portProject: map[int]string{}}
}

func (f *fakeDocker) Run(ctx context.Context, c runner.Cmd) ([]byte, error) {
	if c.Name == "git" {
		return f.real.Run(ctx, c)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, c.String())
	switch c.Name {
	case "apachectl":
		if f.failApache && len(c.Args) > 0 && c.Args[0] == "configtest" {
			return nil, fmt.Errorf("Syntax error on line 1")
		}
		return nil, nil
	case "docker":
		if len(c.Args) > 0 && c.Args[0] == "network" {
			f.networkCalls = append(f.networkCalls, strings.Join(c.Args, " "))
			return nil, nil
		}
		if len(c.Args) > 0 && c.Args[0] == "inspect" {
			name := c.Args[len(c.Args)-1]
			for proj, running := range f.running {
				if running && strings.HasPrefix(name, proj+"-") {
					if f.noNet[proj] {
						return []byte(`{"bridge":{}}`), nil
					}
					return []byte(`{"duod-edge":{},"` + proj + `_default":{}}`), nil
				}
			}
			return nil, fmt.Errorf("Error: No such object: %s", name)
		}
		var project, file, verb string
		for i := 0; i < len(c.Args); i++ {
			switch c.Args[i] {
			case "-p":
				project = c.Args[i+1]
				i++
			case "-f":
				file = c.Args[i+1]
				i++
			case "--project-directory":
				i++
			case "compose":
			default:
				if verb == "" {
					verb = c.Args[i]
				}
			}
		}
		port := 0
		for _, e := range c.Env {
			if v, ok := strings.CutPrefix(e, "BG_PORT="); ok {
				port, _ = strconv.Atoi(v)
			}
		}
		f.portProject[port] = project
		switch verb {
		case "up":
			raw, err := os.ReadFile(filepath.Join(c.Dir, file))
			if err != nil {
				return nil, fmt.Errorf("compose file missing: %w", err)
			}
			f.running[project] = true
			f.healthy[project] = !strings.Contains(string(raw), "BROKEN")
			f.noNet[project] = strings.Contains(string(raw), "NONET")
			f.rev[project] = ""
			for _, e := range c.Env {
				if v, ok := strings.CutPrefix(e, "BG_REVISION="); ok && !strings.Contains(string(raw), "WRONGREV") {
					f.rev[project] = v[:8]
				}
			}
		case "stop", "down":
			f.running[project] = false
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command %s", c.String())
}

func (f *fakeDocker) serving(port int) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.portProject[port]
	return f.running[p] && f.healthy[p], f.rev[p]
}

func (f *fakeDocker) kill(project string) {
	f.mu.Lock()
	f.running[project] = false
	f.mu.Unlock()
}

func (f *fakeDocker) saw(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func (f *fakeDocker) reset() {
	f.mu.Lock()
	f.cmds = nil
	f.mu.Unlock()
}

// slotServer emulates the container published on a loopback port.
func slotServer(t *testing.T, f *fakeDocker) (*httptest.Server, int) {
	t.Helper()
	var port int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, rev := f.serving(port); ok {
			w.Header().Set("X-Revision", rev)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ = strconv.Atoi(p)
	t.Cleanup(srv.Close)
	return srv, port
}

type gitRepo struct {
	t   *testing.T
	dir string
}

func (g gitRepo) run(args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (g gitRepo) commit(msg, compose string) string {
	g.t.Helper()
	if err := os.MkdirAll(filepath.Join(g.dir, "apps/shop"), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.dir, "apps/shop/compose.yaml"), []byte(compose), 0o644); err != nil {
		g.t.Fatal(err)
	}
	g.run("add", "-A")
	g.run("commit", "-q", "-m", msg)
	return g.run("rev-parse", "HEAD")
}

type harness struct {
	t      *testing.T
	app    *App
	fake   *fakeDocker
	repo   gitRepo
	blue   int
	green  int
	apache string
}

func newHarness(t *testing.T, mutate func(*config.App)) *harness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	repo := gitRepo{t: t, dir: filepath.Join(tmp, "src")}
	if err := os.MkdirAll(repo.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	repo.run("init", "-q", "-b", "main")

	fake := newFakeDocker()
	_, blue := slotServer(t, fake)
	_, green := slotServer(t, fake)
	apacheFile := filepath.Join(tmp, "apache", "shop.conf")

	yml := fmt.Sprintf(`
stateDir: %s
apps:
  - name: shop
    source:
      repoURL: %s
      revision: main
      path: apps/shop
      pollInterval: 1h
    slots:
      blue:  {port: %d}
      green: {port: %d}
    healthCheck:
      path: /health
      interval: 10ms
      timeout: 200ms
      successThreshold: 2
      failureThreshold: 2
      progressDeadline: 400ms
    strategy:
      scaleDownDelay: 1h
    proxy:
      configFile: %s
`, filepath.Join(tmp, "state"), repo.dir, blue, green, apacheFile)
	cfg, err := config.Parse([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(&cfg.Apps[0])
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctrl, err := New(cfg, fake, log)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := ctrl.App("shop")
	return &harness{t: t, app: a, fake: fake, repo: repo, blue: blue, green: green, apache: apacheFile}
}

func (h *harness) sync() state.App {
	h.app.Refresh()
	h.app.reconcile(context.Background())
	return h.app.Snapshot()
}

func (h *harness) tick() state.App {
	h.app.reconcile(context.Background())
	return h.app.Snapshot()
}

func (h *harness) proxyRoutesTo(port int) bool {
	raw, err := os.ReadFile(h.apache)
	if err != nil {
		h.t.Fatal(err)
	}
	return strings.Contains(string(raw), fmt.Sprintf("ProxyPass        / http://127.0.0.1:%d/", port))
}

func expect(t *testing.T, cond bool, format string, args ...any) {
	t.Helper()
	if !cond {
		t.Fatalf(format, args...)
	}
}

const good = "services:\n  web:\n    image: nginx\n    ports: [\"127.0.0.1:${BG_PORT}:80\"]\n"

func TestFullBlueGreenLifecycle(t *testing.T) {
	h := newHarness(t, nil)

	// 1. initial deployment goes straight to blue and becomes active
	v1 := h.repo.commit("v1", good)
	st := h.sync()
	expect(t, st.Phase == state.PhaseHealthy, "phase after v1 = %s (%s)", st.Phase, st.Message)
	expect(t, st.ActiveSlot == state.Blue && st.ActiveRevision == v1, "active = %s@%s", st.ActiveSlot, st.ActiveRevision)
	expect(t, st.Sync == state.Synced, "sync = %s", st.Sync)
	expect(t, h.proxyRoutesTo(h.blue), "apache should route to blue")
	expect(t, h.fake.saw("apachectl graceful"), "apache must be gracefully reloaded")

	// 2. new commit: preview on green, paused, traffic still on blue
	v2 := h.repo.commit("v2", good+"# v2\n")
	st = h.sync()
	expect(t, st.Phase == state.PhasePaused, "phase after v2 = %s (%s)", st.Phase, st.Message)
	expect(t, st.PreviewSlot == state.Green && st.PreviewRevision == v2, "preview = %s@%s", st.PreviewSlot, st.PreviewRevision)
	expect(t, st.Sync == state.OutOfSync, "sync while paused = %s", st.Sync)
	expect(t, h.proxyRoutesTo(h.blue), "traffic must stay on blue while paused")
	raw, _ := os.ReadFile(h.apache)
	expect(t, strings.Contains(string(raw), fmt.Sprintf("/_preview/ http://127.0.0.1:%d/", h.green)), "preview route missing:\n%s", raw)

	// staying paused across ticks
	st = h.tick()
	expect(t, st.Phase == state.PhasePaused, "should stay paused, got %s", st.Phase)

	// 3. promote
	expect(t, h.app.Promote(context.Background()) == nil, "promote failed")
	st = h.app.Snapshot()
	expect(t, st.Phase == state.PhaseHealthy && st.ActiveSlot == state.Green && st.ActiveRevision == v2, "after promote: %+v", st)
	expect(t, st.PreviousSlot == state.Blue && st.PreviousRevision == v1, "previous = %s@%s", st.PreviousSlot, st.PreviousRevision)
	expect(t, h.proxyRoutesTo(h.green), "apache should route to green")
	expect(t, !st.ScaleDownAt.IsZero(), "scale down should be scheduled")

	// 4. rollback to blue without Git change, v2 marked failed (no redeploy loop)
	expect(t, h.app.Rollback(context.Background()) == nil, "rollback failed")
	st = h.tick()
	expect(t, st.ActiveSlot == state.Blue && st.ActiveRevision == v1, "after rollback active = %s@%s", st.ActiveSlot, st.ActiveRevision)
	expect(t, st.FailedRevision == v2 && st.Phase == state.PhaseDegraded && st.Sync == state.OutOfSync, "after rollback: phase=%s sync=%s failed=%s", st.Phase, st.Sync, st.FailedRevision)
	expect(t, h.proxyRoutesTo(h.blue), "apache should route back to blue")

	// 5. a broken commit fails pre-promotion analysis; blue keeps serving
	h.fake.reset()
	v3 := h.repo.commit("v3 broken", good+"# BROKEN\n")
	st = h.sync()
	expect(t, st.Phase == state.PhaseDegraded && st.FailedRevision == v3, "after v3: phase=%s failed=%s msg=%s", st.Phase, st.FailedRevision, st.Message)
	expect(t, st.ActiveSlot == state.Blue && st.PreviewSlot == "", "active/preview after failure: %s/%s", st.ActiveSlot, st.PreviewSlot)
	expect(t, h.fake.saw("-p shop-green --project-directory") && h.fake.saw(" down "), "failed preview must be torn down: %v", h.fake.cmds)
	expect(t, h.proxyRoutesTo(h.blue), "traffic must stay on blue")

	// failed revision is not retried on every tick
	h.fake.reset()
	st = h.tick()
	expect(t, !h.fake.saw(" up "), "failed revision must not be redeployed: %v", h.fake.cmds)

	// 6. fix pushed -> new rollout, auto promotion via abort/promote path
	v4 := h.repo.commit("v4 fix", good+"# v4\n")
	st = h.sync()
	expect(t, st.Phase == state.PhasePaused && st.PreviewRevision == v4, "v4 should be paused, got %s %s", st.Phase, st.Message)

	// abort discards preview and marks it failed
	expect(t, h.app.Abort(context.Background()) == nil, "abort failed")
	st = h.tick()
	expect(t, st.PreviewSlot == "" && st.FailedRevision == v4 && st.ActiveSlot == state.Blue, "after abort: %+v", st)

	// retry redeploys it
	h.app.Retry()
	st = h.tick()
	expect(t, st.Phase == state.PhasePaused && st.PreviewRevision == v4, "retry should redeploy v4: %s %s", st.Phase, st.Message)
	expect(t, h.app.Promote(context.Background()) == nil, "promote v4")
	st = h.tick()
	expect(t, st.Phase == state.PhaseHealthy && st.Sync == state.Synced && st.ActiveSlot == state.Green, "final: %s %s %s", st.Phase, st.Sync, st.ActiveSlot)
}

func TestAutoPromotionAndScaleDown(t *testing.T) {
	h := newHarness(t, func(a *config.App) {
		a.Strategy.AutoPromotionEnabled = true
		a.Strategy.ScaleDownDelay.Duration = 20 * time.Millisecond
	})
	h.repo.commit("v1", good)
	h.sync()
	v2 := h.repo.commit("v2", good+"# v2\n")
	st := h.sync()
	expect(t, st.Phase == state.PhaseHealthy && st.ActiveRevision == v2 && st.ActiveSlot == state.Green, "auto promotion: %s %s", st.Phase, st.Message)

	time.Sleep(30 * time.Millisecond)
	h.fake.reset()
	st = h.tick()
	expect(t, h.fake.saw("-p shop-blue") && h.fake.saw(" stop"), "blue should be stopped: %v", h.fake.cmds)
	expect(t, st.ScaleDownAt.IsZero() && st.PreviousSlot == state.Blue, "previous kept for rollback: %+v", st)

	// rollback restarts the stopped slot
	h.fake.reset()
	expect(t, h.app.Rollback(context.Background()) == nil, "rollback")
	expect(t, h.fake.saw("-p shop-blue") && h.fake.saw("--no-recreate"), "blue should be started: %v", h.fake.cmds)
	expect(t, h.proxyRoutesTo(h.blue), "routes to blue")
}

func TestAutoPromotionSeconds(t *testing.T) {
	h := newHarness(t, func(a *config.App) { a.Strategy.AutoPromotionSeconds = 1 })
	h.repo.commit("v1", good)
	h.sync()
	h.repo.commit("v2", good+"# v2\n")
	st := h.sync()
	expect(t, st.Phase == state.PhasePaused, "paused first, got %s", st.Phase)
	time.Sleep(1100 * time.Millisecond)
	st = h.tick()
	expect(t, st.Phase == state.PhaseHealthy && st.ActiveSlot == state.Green, "promoted after delay: %s", st.Phase)
}

func TestNewCommitWhilePausedReplacesPreview(t *testing.T) {
	h := newHarness(t, nil)
	h.repo.commit("v1", good)
	h.sync()
	h.repo.commit("v2", good+"# v2\n")
	h.sync()
	v3 := h.repo.commit("v3", good+"# v3\n")
	st := h.sync()
	expect(t, st.Phase == state.PhasePaused && st.PreviewRevision == v3 && st.PreviewSlot == state.Green, "preview replaced: %+v", st)
}

func TestApacheConfigtestFailureKeepsTraffic(t *testing.T) {
	h := newHarness(t, nil)
	h.repo.commit("v1", good)
	h.sync()
	h.repo.commit("v2", good+"# v2\n")
	h.sync()
	before, _ := os.ReadFile(h.apache)

	h.fake.mu.Lock()
	h.fake.failApache = true
	h.fake.mu.Unlock()
	err := h.app.Promote(context.Background())
	expect(t, err != nil, "promote should fail when configtest fails")
	after, _ := os.ReadFile(h.apache)
	expect(t, string(before) == string(after), "apache file must be restored")
	st := h.app.Snapshot()
	expect(t, st.ActiveSlot == state.Blue && st.Phase == state.PhasePaused, "still blue+paused: %s %s", st.ActiveSlot, st.Phase)
	expect(t, !h.fake.saw("apachectl graceful") || h.proxyRoutesTo(h.blue), "no reload to green")
}

func TestSelfHealAndProxyDrift(t *testing.T) {
	h := newHarness(t, nil)
	h.repo.commit("v1", good)
	h.sync()

	h.fake.kill("shop-blue")
	h.fake.reset()
	h.tick()
	st := h.tick() // failureThreshold = 2
	expect(t, h.fake.saw("-p shop-blue") && h.fake.saw("--no-recreate"), "self-heal should start blue: %v", h.fake.cmds)
	st = h.tick()
	expect(t, st.ActiveHealthy && st.Phase == state.PhaseHealthy, "recovered: %s healthy=%v", st.Phase, st.ActiveHealthy)

	// someone edits the managed Apache file by hand
	if err := os.WriteFile(h.apache, []byte("ProxyPass / http://127.0.0.1:1/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st = h.tick()
	expect(t, h.proxyRoutesTo(h.blue), "drift must be corrected")
	expect(t, st.History[len(st.History)-1].Type == "ProxySynced", "event recorded: %s", st.History[len(st.History)-1].Type)
}

func TestPostPromotionAnalysisRollsBack(t *testing.T) {
	// The "public" URL fails, so promotion must be reverted automatically.
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(502) }))
	defer public.Close()
	h := newHarness(t, func(a *config.App) {
		a.Strategy.AutoPromotionEnabled = true
		a.Strategy.PostPromotionAnalysis = &config.PostPromotionAnalysis{
			URL: public.URL, Duration: config.Duration{Duration: 200 * time.Millisecond},
			Interval: config.Duration{Duration: 10 * time.Millisecond}, FailureThreshold: 2, ExpectStatus: 200,
		}
	})
	v1 := h.repo.commit("v1", good)
	h.sync()
	v2 := h.repo.commit("v2", good+"# v2\n")
	st := h.sync()
	expect(t, st.ActiveSlot == state.Blue && st.ActiveRevision == v1, "should be back on blue: %s@%s", st.ActiveSlot, short(st.ActiveRevision))
	expect(t, st.FailedRevision == v2 && st.Phase == state.PhaseDegraded, "v2 failed: %s %s", st.Phase, st.FailedRevision)
	expect(t, h.proxyRoutesTo(h.blue), "apache back on blue")
}

func TestRestartRecoversState(t *testing.T) {
	h := newHarness(t, nil)
	h.repo.commit("v1", good)
	h.sync()
	v2 := h.repo.commit("v2", good+"# v2\n")
	h.sync()

	// a second reconciler on the same state dir (= duod restarted)
	a2, err := newApp(h.app.global, h.app.cfg, h.fake, slog.New(slog.NewTextHandler(io.Discard, nil)), h.app.shared)
	if err != nil {
		t.Fatal(err)
	}
	st := a2.Snapshot()
	expect(t, st.Phase == state.PhasePaused && st.PreviewRevision == v2, "state restored: %s", st.Phase)
	expect(t, a2.Promote(context.Background()) == nil, "promote after restart")
}

func TestRevisionHeaderVerification(t *testing.T) {
	h := newHarness(t, func(a *config.App) { a.HealthCheck.RevisionHeader = "X-Revision" })
	v1 := h.repo.commit("v1", good)
	st := h.sync()
	expect(t, st.Phase == state.PhaseHealthy && st.ActiveRevision == v1, "v1 with header: %s %s", st.Phase, st.Message)

	// the new containers answer 200 but report the wrong revision -> must fail
	v2 := h.repo.commit("v2", good+"# WRONGREV\n")
	st = h.sync()
	expect(t, st.FailedRevision == v2 && st.ActiveSlot == state.Blue, "wrong revision must fail analysis: %s %s", st.Phase, st.Message)
	expect(t, strings.Contains(st.Message, "X-Revision"), "message should name the header: %s", st.Message)
}

func TestIdleSlotIsCleanedBeforeRollout(t *testing.T) {
	h := newHarness(t, nil)
	h.repo.commit("v1", good)
	h.sync()
	h.repo.commit("v2", good+"# v2\n")
	h.sync()
	expect(t, h.app.Promote(context.Background()) == nil, "promote")
	// blue still runs v1 (kept for rollback); next rollout must take it down first
	h.fake.reset()
	h.repo.commit("v3", good+"# v3\n")
	h.sync()
	h.fake.mu.Lock()
	cmds := append([]string(nil), h.fake.cmds...)
	h.fake.mu.Unlock()
	down, up := -1, -1
	for i, c := range cmds {
		if strings.Contains(c, "-p shop-blue") && strings.Contains(c, " down ") && down < 0 {
			down = i
		}
		if strings.Contains(c, "-p shop-blue") && strings.Contains(c, " up ") && up < 0 {
			up = i
		}
	}
	expect(t, down >= 0 && up > down, "expected down before up on blue: %v", cmds)
}
