// Package controller contains the reconcile loop: it compares the Git target
// revision with the live blue/green state and drives the rollout state machine
//
//	New ──► Progressing ──► Paused ──promote──► Healthy
//	             │             │                   │
//	             └──fail/abort─┴──► Degraded ◄─────┘ (active unhealthy)
//
// modelled on Argo Rollouts' blueGreen strategy, with Traefik (or Apache) as
// the "active service" and two Docker Compose projects as the ReplicaSets.
// Every app runs its own loop, so different apps roll out concurrently.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pierrejochem/duod/internal/apache"
	"github.com/pierrejochem/duod/internal/compose"
	"github.com/pierrejochem/duod/internal/config"
	"github.com/pierrejochem/duod/internal/gitsrc"
	"github.com/pierrejochem/duod/internal/health"
	"github.com/pierrejochem/duod/internal/proxy"
	"github.com/pierrejochem/duod/internal/runner"
	"github.com/pierrejochem/duod/internal/state"
	"github.com/pierrejochem/duod/internal/traefik"
)

// ErrConflict is returned when an action is not valid in the current phase.
var ErrConflict = errors.New("conflict")

// App reconciles one application.
type App struct {
	cfg     config.App
	global  *config.Config
	run     runner.Runner
	repo    *gitsrc.Repo
	px      proxy.Proxy
	shared  *shared
	store   state.Store
	log     *slog.Logger
	probe   *health.Probe
	ppProbe *health.Probe

	mu sync.Mutex // serializes the state machine and all actions
	st *state.App

	snapMu sync.RWMutex
	snap   state.App

	trigger    chan struct{}
	forceFetch chan struct{}

	cancelMu      sync.Mutex
	cancelRollout context.CancelFunc
	abortReason   string
}

// shared holds what all apps of one controller have in common.
type shared struct {
	sem        chan struct{} // rollout concurrency limit (nil = unlimited)
	apacheLock *sync.Mutex
	inFlight   *atomicCounter
}

func newProxy(global *config.Config, cfg config.App, r runner.Runner, sh *shared) (proxy.Proxy, error) {
	switch cfg.Proxy.Type {
	case "traefik":
		return &traefik.Backend{
			App:        cfg.Name,
			Routes:     cfg.Proxy.Routes,
			Cfg:        global.Traefik,
			HealthPath: cfg.HealthCheck.Path,
			Docker:     dockerCLI(global.Docker),
			Runner:     r,
			Client:     &http.Client{Timeout: cfg.HealthCheck.Timeout.Duration},
		}, nil
	default:
		site, err := apache.NewSite(cfg.Name, cfg.Proxy.ConfigFile, cfg.Proxy.Template, global.Apache.ConfigTest, global.Apache.Reload, r)
		if err != nil {
			return nil, err
		}
		site.HealthHost, site.HealthPath, site.Lock = cfg.HealthCheck.Host, cfg.HealthCheck.Path, sh.apacheLock
		return site, nil
	}
}

// dockerCLI turns the compose prefix (["docker", "--context", "x", "compose"])
// into the plain docker CLI prefix (["docker", "--context", "x"]).
func dockerCLI(composePrefix []string) []string {
	if n := len(composePrefix); n > 1 && composePrefix[n-1] == "compose" {
		return append([]string{}, composePrefix[:n-1]...)
	}
	return []string{"docker"}
}

func newApp(global *config.Config, cfg config.App, r runner.Runner, log *slog.Logger, sh *shared) (*App, error) {
	px, err := newProxy(global, cfg, r, sh)
	if err != nil {
		return nil, err
	}
	store := state.Store{Dir: filepath.Join(global.StateDir, "state")}
	st, err := store.Load(cfg.Name)
	if err != nil {
		return nil, fmt.Errorf("app %s: load state: %w", cfg.Name, err)
	}
	a := &App{
		cfg:    cfg,
		global: global,
		run:    r,
		repo: &gitsrc.Repo{
			URL:    cfg.Source.RepoURL,
			Dir:    filepath.Join(global.StateDir, "repos", cfg.Name+".git"),
			Runner: r,
		},
		px:         px,
		shared:     sh,
		store:      store,
		log:        log.With("app", cfg.Name),
		st:         st,
		trigger:    make(chan struct{}, 1),
		forceFetch: make(chan struct{}, 1),
	}
	a.probe = health.NewProbe(cfg.HealthCheck.Timeout.Duration, cfg.HealthCheck.ExpectStatus)
	a.probe.RevisionHeader = cfg.HealthCheck.RevisionHeader
	if p := cfg.Strategy.PostPromotionAnalysis; p != nil {
		a.ppProbe = health.NewProbe(cfg.HealthCheck.Timeout.Duration, p.ExpectStatus)
	}
	a.snap = st.Clone()
	return a, nil
}

// Snapshot returns a copy of the current state for readers.
func (a *App) Snapshot() state.App {
	a.snapMu.RLock()
	defer a.snapMu.RUnlock()
	return a.snap.Clone()
}

// publish persists the state and refreshes the snapshot. Caller holds mu.
func (a *App) publish() {
	if err := a.store.Save(a.st); err != nil {
		a.log.Error("save state", "err", err)
	}
	a.snapMu.Lock()
	a.snap = a.st.Clone()
	a.snapMu.Unlock()
}

// Trigger schedules a reconcile as soon as possible.
func (a *App) Trigger() {
	select {
	case a.trigger <- struct{}{}:
	default:
	}
}

// Refresh forces a Git fetch on the next reconcile (webhook / `duod sync`).
func (a *App) Refresh() {
	select {
	case a.forceFetch <- struct{}{}:
	default:
	}
	a.Trigger()
}

// Run is the per-app control loop.
func (a *App) Run(ctx context.Context) {
	t := time.NewTicker(a.global.ReconcileInterval.Duration)
	defer t.Stop()
	a.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.trigger:
		}
		a.reconcile(ctx)
	}
}

func (a *App) reconcile(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.publish()

	force := false
	select {
	case <-a.forceFetch:
		force = true
	default:
	}
	if force || time.Since(a.st.LastFetch) >= a.cfg.Source.PollInterval.Duration {
		a.fetch(ctx)
	}
	a.scaleDown(ctx)
	a.ensureActive(ctx)
	a.drive(ctx)
}

// ---- Git -------------------------------------------------------------------

func (a *App) fetch(ctx context.Context) {
	st := a.st
	st.LastFetch = time.Now().UTC()
	fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := a.repo.Fetch(fctx); err != nil {
		st.FetchError = err.Error()
		a.log.Warn("git fetch failed", "err", err)
		return
	}
	c, err := a.repo.Resolve(fctx, a.cfg.Source.Revision, a.cfg.Source.Path)
	if err != nil {
		st.FetchError = err.Error()
		a.log.Warn("resolve revision failed", "err", err)
		return
	}
	st.FetchError = ""
	if c.SHA != st.TargetRevision {
		a.log.Info("new target revision", "sha", c.Short(), "subject", c.Subject)
		st.Record("TargetChanged", c.SHA, "", c.Subject)
	}
	st.TargetRevision, st.TargetSubject = c.SHA, c.Subject
}

// ---- state machine --------------------------------------------------------

func (a *App) drive(ctx context.Context) {
	st := a.st
	target := st.TargetRevision
	defer a.updateStatus()
	if target == "" {
		return
	}

	switch st.Phase {
	case state.PhaseProgressing:
		// duod was restarted in the middle of a rollout: redo it.
		if st.PreviewRevision != "" {
			a.rollout(ctx, st.PreviewRevision)
			return
		}
	case state.PhasePaused:
		switch {
		case target == st.ActiveRevision:
			a.discardPreview(ctx, "Git moved back to the active revision")
		case target != st.PreviewRevision && target != st.FailedRevision:
			a.log.Info("new commit while paused, replacing preview", "sha", short(target))
			a.rollout(ctx, target)
		case a.cfg.Strategy.AutoPromotionSeconds > 0 &&
			time.Since(st.PreviewReadySince) >= time.Duration(a.cfg.Strategy.AutoPromotionSeconds)*time.Second:
			_ = a.promote(ctx, fmt.Sprintf("auto-promotion after %ds", a.cfg.Strategy.AutoPromotionSeconds))
		default:
			a.ensureProxy(ctx)
		}
		return
	}

	if target != st.ActiveRevision && target != st.FailedRevision {
		a.rollout(ctx, target)
		return
	}
	a.ensureProxy(ctx)
}

// updateStatus derives Sync, steady-state Phase and Message.
func (a *App) updateStatus() {
	st := a.st
	switch {
	case st.TargetRevision == "":
		st.Sync = state.Unknown
	case st.TargetRevision == st.ActiveRevision && st.PreviewSlot == "":
		st.Sync = state.Synced
	default:
		st.Sync = state.OutOfSync
	}
	if st.Phase == state.PhaseProgressing || st.Phase == state.PhasePaused {
		return
	}
	if st.ActiveSlot == "" {
		if st.FailedRevision != "" {
			st.Phase = state.PhaseDegraded
		} else {
			st.Phase = state.PhaseNew
		}
		return
	}
	switch {
	case !st.ActiveHealthy:
		st.Phase = state.PhaseDegraded
		st.Message = fmt.Sprintf("active slot %s (%s) is failing health checks", st.ActiveSlot, short(st.ActiveRevision))
	case st.FailedRevision != "" && st.FailedRevision == st.TargetRevision:
		st.Phase = state.PhaseDegraded
		st.Message = fmt.Sprintf("revision %s failed (%s); still serving %s from %s - push a fix or run `duod retry %s`",
			short(st.FailedRevision), st.FailureReason, short(st.ActiveRevision), st.ActiveSlot, st.Name)
	default:
		st.Phase = state.PhaseHealthy
		st.Message = fmt.Sprintf("serving %s from %s", short(st.ActiveRevision), st.ActiveSlot)
	}
}

// rollout deploys rev to the idle slot and runs the pre-promotion analysis.
func (a *App) rollout(ctx context.Context, rev string) {
	st := a.st
	preview := st.ActiveSlot.Other()
	if st.PreviousSlot == preview {
		// The idle slot still holds the rollback target; it is about to be replaced.
		st.PreviousSlot, st.PreviousRevision, st.ScaleDownAt = "", "", time.Time{}
	}
	st.Phase = state.PhaseProgressing
	st.PreviewSlot, st.PreviewRevision, st.PreviewReadySince = preview, rev, time.Time{}
	st.Message = fmt.Sprintf("deploying %s to %s", short(rev), preview)
	st.Record("RolloutStarted", rev, preview, st.Message)
	a.log.Info("rollout started", "sha", short(rev), "slot", preview)
	a.publish()

	rctx, cancel := context.WithCancel(ctx)
	a.setCancel(cancel)
	defer func() { a.setCancel(nil); cancel() }()

	// Different apps roll out in parallel; maxConcurrentRollouts caps how many
	// pull images and start containers at the same time.
	if a.shared.sem != nil {
		select {
		case a.shared.sem <- struct{}{}:
		default:
			st.Message = fmt.Sprintf("queued: waiting for a free rollout slot (maxConcurrentRollouts=%d)", cap(a.shared.sem))
			st.Record("RolloutQueued", rev, preview, st.Message)
			a.publish()
			select {
			case a.shared.sem <- struct{}{}:
			case <-rctx.Done():
				a.failRollout(ctx, rev, errors.New("cancelled while queued"))
				return
			}
		}
		defer func() { <-a.shared.sem }()
	}
	a.shared.inFlight.add(1)
	defer a.shared.inFlight.add(-1)

	// Remove whatever still runs in the idle slot (an old revision kept for
	// rollback, or a superseded preview) so its port is free and a stale
	// container can never pass the health check on behalf of the new one.
	if _, err := os.Stat(a.composeDir(preview)); err == nil {
		a.teardown(ctx, preview, "")
	}

	dir, err := a.repo.Materialize(rctx, rev, a.cfg.Source.Path, a.slotDir(preview))
	if err != nil {
		a.failRollout(ctx, rev, fmt.Errorf("checkout: %w", err))
		return
	}
	proj := a.project(preview, dir, rev)
	if err := proj.Up(rctx); err != nil {
		a.failRollout(ctx, rev, fmt.Errorf("compose up: %w", err))
		return
	}
	if err := a.px.CheckSlot(rctx, a.pslot(preview, rev)); err != nil {
		a.failRollout(ctx, rev, err)
		return
	}
	// probe routes (Traefik) / preview route (Apache) for the new slot
	a.ensureProxy(ctx)

	targets := a.px.Targets(a.pslot(preview, rev))
	st.Message = fmt.Sprintf("analysing %s on %s", short(rev), describe(targets))
	a.publish()
	if err := a.waitHealthy(rctx, targets, rev); err != nil {
		a.failRollout(ctx, rev, fmt.Errorf("pre-promotion analysis: %w", err))
		return
	}
	st.PreviewReadySince = time.Now().UTC()
	st.Record("PreviewHealthy", rev, preview, "pre-promotion analysis passed")

	switch {
	case st.ActiveSlot == "":
		_ = a.promote(ctx, "initial deployment")
	case a.cfg.Strategy.AutoPromotionEnabled:
		_ = a.promote(ctx, "autoPromotionEnabled")
	default:
		st.Phase = state.PhasePaused
		st.Message = fmt.Sprintf("preview %s healthy on %s - waiting for `duod promote %s`", short(rev), preview, st.Name)
		st.Record("Paused", rev, preview, st.Message)
	}
}

func (a *App) failRollout(ctx context.Context, rev string, err error) {
	st := a.st
	reason := err.Error()
	evt := "RolloutFailed"
	if r := a.takeAbortReason(); r != "" {
		reason, evt = r, "RolloutAborted"
	}
	a.log.Warn("rollout failed", "sha", short(rev), "err", reason)
	if st.PreviewSlot != "" {
		a.teardown(ctx, st.PreviewSlot, rev)
	}
	st.PreviewSlot, st.PreviewRevision, st.PreviewReadySince = "", "", time.Time{}
	st.FailedRevision, st.FailureReason = rev, reason
	st.Phase = state.PhaseDegraded
	st.Message = fmt.Sprintf("rollout of %s failed: %s", short(rev), reason)
	st.Record(evt, rev, "", reason)
	a.ensureProxy(ctx) // drop the preview / probe routes
}

// promote switches the proxy to the preview slot. Caller holds mu.
func (a *App) promote(ctx context.Context, reason string) error {
	st := a.st
	if st.PreviewSlot == "" || st.PreviewReadySince.IsZero() {
		return fmt.Errorf("%w: no healthy preview to promote", ErrConflict)
	}
	fromSlot, fromRev := st.ActiveSlot, st.ActiveRevision
	toSlot, toRev := st.PreviewSlot, st.PreviewRevision

	data := proxy.Data{App: st.Name, Active: a.pslot(toSlot, toRev), Previous: a.pslot(fromSlot, fromRev)}
	if err := a.px.Apply(ctx, data); err != nil {
		st.Message = "promotion failed: " + err.Error()
		st.Record("PromotionFailed", toRev, toSlot, err.Error())
		a.log.Error("promotion failed", "err", err)
		return err
	}
	st.ActiveSlot, st.ActiveRevision, st.ActiveSince = toSlot, toRev, time.Now().UTC()
	st.PreviewSlot, st.PreviewRevision, st.PreviewReadySince = "", "", time.Time{}
	st.ActiveHealthy, st.ActiveFailures = true, 0
	st.Record("Promoted", toRev, toSlot, reason)
	a.log.Info("promoted", "sha", short(toRev), "slot", toSlot, "reason", reason)

	if p := a.cfg.Strategy.PostPromotionAnalysis; p != nil && fromSlot != "" {
		st.Phase = state.PhaseProgressing
		st.Message = fmt.Sprintf("post-promotion analysis of %s for %s", p.URL, p.Duration.Duration)
		a.publish()
		if err := a.ppProbe.Watch(ctx, p.URL, p.Interval.Duration, p.Duration.Duration, p.FailureThreshold); err != nil {
			a.log.Warn("post-promotion analysis failed, rolling back", "err", err)
			back := proxy.Data{App: st.Name, Active: a.pslot(fromSlot, fromRev)}
			if aerr := a.px.Apply(ctx, back); aerr != nil {
				st.Record("AutoRollbackFailed", fromRev, fromSlot, aerr.Error())
			}
			st.ActiveSlot, st.ActiveRevision = fromSlot, fromRev
			a.teardown(ctx, toSlot, toRev)
			st.FailedRevision, st.FailureReason = toRev, "post-promotion analysis: "+err.Error()
			st.Phase = state.PhaseDegraded
			st.Record("AutoRollback", fromRev, fromSlot, "post-promotion analysis failed: "+err.Error())
			return err
		}
		st.Record("PostPromotionAnalysisPassed", toRev, toSlot, "")
	}

	if fromSlot != "" {
		st.PreviousSlot, st.PreviousRevision = fromSlot, fromRev
		st.ScaleDownAt = time.Now().Add(a.cfg.Strategy.ScaleDownDelay.Duration).UTC()
	}
	if st.FailedRevision == toRev {
		st.FailedRevision, st.FailureReason = "", ""
	}
	st.Phase = state.PhaseHealthy
	return nil
}

func (a *App) discardPreview(ctx context.Context, reason string) {
	st := a.st
	if st.PreviewSlot == "" {
		return
	}
	rev := st.PreviewRevision
	a.teardown(ctx, st.PreviewSlot, rev)
	st.PreviewSlot, st.PreviewRevision, st.PreviewReadySince = "", "", time.Time{}
	st.Phase = state.PhaseHealthy
	st.Record("PreviewDiscarded", rev, "", reason)
	a.ensureProxy(ctx)
}

// scaleDown stops the previous slot once scaleDownDelay has passed. The
// containers are kept (stopped) so a rollback only needs `compose start`.
func (a *App) scaleDown(ctx context.Context) {
	st := a.st
	if st.ScaleDownAt.IsZero() || time.Now().Before(st.ScaleDownAt) || st.PreviousSlot == "" {
		return
	}
	proj := a.project(st.PreviousSlot, a.composeDir(st.PreviousSlot), st.PreviousRevision)
	if err := proj.Stop(ctx); err != nil {
		a.log.Warn("scale down failed", "slot", st.PreviousSlot, "err", err)
		return
	}
	st.ScaleDownAt = time.Time{}
	st.Record("ScaledDown", st.PreviousRevision, st.PreviousSlot, "previous slot stopped (kept for rollback)")
}

// ensureActive probes the active slot and self-heals it (compose up) when
// it keeps failing.
func (a *App) ensureActive(ctx context.Context) {
	st := a.st
	if st.ActiveSlot == "" || st.Phase == state.PhaseProgressing {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, a.cfg.HealthCheck.Timeout.Duration+time.Second)
	err := a.probe.CheckAll(cctx, a.px.Targets(a.pslot(st.ActiveSlot, st.ActiveRevision)), st.ActiveRevision)
	cancel()
	if err == nil {
		if !st.ActiveHealthy {
			st.Record("ActiveHealthy", st.ActiveRevision, st.ActiveSlot, "active slot recovered")
		}
		st.ActiveHealthy, st.ActiveFailures = true, 0
		return
	}
	st.ActiveFailures++
	if st.ActiveFailures < a.cfg.HealthCheck.FailureThreshold {
		return
	}
	if st.ActiveHealthy {
		st.Record("ActiveUnhealthy", st.ActiveRevision, st.ActiveSlot, err.Error())
		a.log.Warn("active slot unhealthy", "slot", st.ActiveSlot, "err", err)
	}
	st.ActiveHealthy = false
	if !*a.cfg.Strategy.SelfHeal || time.Since(st.LastSelfHeal) < time.Minute {
		return
	}
	st.LastSelfHeal = time.Now().UTC()
	dir := a.composeDir(st.ActiveSlot)
	if _, statErr := os.Stat(dir); statErr != nil {
		if dir, err = a.repo.Materialize(ctx, st.ActiveRevision, a.cfg.Source.Path, a.slotDir(st.ActiveSlot)); err != nil {
			st.Record("SelfHealFailed", st.ActiveRevision, st.ActiveSlot, err.Error())
			return
		}
	}
	if err := a.project(st.ActiveSlot, dir, st.ActiveRevision).Start(ctx); err != nil {
		st.Record("SelfHealFailed", st.ActiveRevision, st.ActiveSlot, err.Error())
		return
	}
	st.Record("SelfHeal", st.ActiveRevision, st.ActiveSlot, "compose up -d on active slot")
}

// ensureProxy corrects drift between the desired and the actual proxy config.
func (a *App) ensureProxy(ctx context.Context) {
	st := a.st
	data := a.proxyData()
	ok, err := a.px.InSync(data)
	if err != nil {
		a.log.Error("render proxy config", "err", err)
		return
	}
	if ok {
		return
	}
	if err := a.px.Apply(ctx, data); err != nil {
		st.Record("ProxyApplyFailed", st.ActiveRevision, st.ActiveSlot, err.Error())
		a.log.Error("apply proxy config", "err", err)
		return
	}
	msg := "Apache config written and gracefully reloaded"
	if a.px.Kind() == "traefik" {
		msg = "Traefik dynamic config written and confirmed"
	}
	st.Record("ProxySynced", st.ActiveRevision, st.ActiveSlot, msg)
}

// ---- actions (API / CLI) --------------------------------------------------

// Promote switches traffic to a paused preview.
func (a *App) Promote(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.publish()
	defer a.updateStatus()
	if a.st.Phase != state.PhasePaused {
		return fmt.Errorf("%w: app is %s, not Paused", ErrConflict, a.st.Phase)
	}
	return a.promote(ctx, "manual promotion")
}

// Abort cancels a running rollout or discards a paused preview. The revision
// is marked failed so it is not redeployed until Git changes or Retry.
func (a *App) Abort(ctx context.Context) error {
	if a.cancelRunning("aborted by user") {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.publish()
	defer a.updateStatus()
	st := a.st
	if st.PreviewSlot == "" {
		return fmt.Errorf("%w: nothing to abort", ErrConflict)
	}
	rev := st.PreviewRevision
	a.discardPreview(ctx, "aborted by user")
	st.FailedRevision, st.FailureReason = rev, "aborted by user"

	return nil
}

// Rollback switches traffic back to the previous slot (restarting it if it
// was scaled down) without touching Git. The current revision is marked
// failed so the GitOps loop does not immediately redeploy it.
func (a *App) Rollback(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.publish()
	defer a.updateStatus()
	st := a.st
	if st.PreviewSlot != "" {
		return fmt.Errorf("%w: a preview occupies the other slot - abort it first", ErrConflict)
	}
	if st.PreviousSlot == "" {
		return fmt.Errorf("%w: no previous revision to roll back to", ErrConflict)
	}
	prevSlot, prevRev := st.PreviousSlot, st.PreviousRevision
	proj := a.project(prevSlot, a.composeDir(prevSlot), prevRev)
	if err := proj.Start(ctx); err != nil {
		return fmt.Errorf("start previous slot: %w", err)
	}
	a.ensureProxy(ctx) // make sure the previous slot is probe-able
	if err := a.waitHealthy(ctx, a.px.Targets(a.pslot(prevSlot, prevRev)), prevRev); err != nil {
		st.Record("RollbackFailed", prevRev, prevSlot, err.Error())
		return fmt.Errorf("previous slot not healthy: %w", err)
	}
	badSlot, badRev := st.ActiveSlot, st.ActiveRevision
	data := proxy.Data{App: st.Name, Active: a.pslot(prevSlot, prevRev), Previous: a.pslot(badSlot, badRev)}
	if err := a.px.Apply(ctx, data); err != nil {
		return err
	}
	st.ActiveSlot, st.ActiveRevision, st.ActiveSince = prevSlot, prevRev, time.Now().UTC()
	st.ActiveHealthy, st.ActiveFailures = true, 0
	st.PreviousSlot, st.PreviousRevision = badSlot, badRev
	st.ScaleDownAt = time.Now().Add(a.cfg.Strategy.ScaleDownDelay.Duration).UTC()
	st.FailedRevision, st.FailureReason = badRev, "rolled back by user"
	st.Phase = state.PhaseHealthy
	st.Record("RolledBack", prevRev, prevSlot, fmt.Sprintf("traffic switched back from %s (%s)", badSlot, short(badRev)))
	a.log.Info("rolled back", "to", short(prevRev), "slot", prevSlot)
	return nil
}

// Retry clears the failed-revision marker and reconciles.
func (a *App) Retry() {
	a.mu.Lock()
	a.st.FailedRevision, a.st.FailureReason = "", ""
	a.st.Record("Retry", a.st.TargetRevision, "", "failed marker cleared")
	a.publish()
	a.mu.Unlock()
	a.Refresh()
}

func (a *App) setCancel(c context.CancelFunc) {
	a.cancelMu.Lock()
	a.cancelRollout = c
	a.cancelMu.Unlock()
}

func (a *App) cancelRunning(reason string) bool {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	if a.cancelRollout == nil {
		return false
	}
	a.abortReason = reason
	a.cancelRollout()
	return true
}

func (a *App) takeAbortReason() string {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	r := a.abortReason
	a.abortReason = ""
	return r
}

// ---- helpers --------------------------------------------------------------

func (a *App) teardown(ctx context.Context, slot state.Slot, rev string) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if err := a.project(slot, a.composeDir(slot), rev).Down(dctx); err != nil {
		a.log.Warn("teardown failed", "slot", slot, "err", err)
	}
}

func (a *App) proxyData() proxy.Data {
	st := a.st
	d := proxy.Data{
		App:     st.Name,
		Active:  a.pslot(st.ActiveSlot, st.ActiveRevision),
		Preview: a.pslot(st.PreviewSlot, st.PreviewRevision),
	}
	if st.PreviousSlot != "" && st.PreviousSlot != st.ActiveSlot && st.PreviousSlot != st.PreviewSlot {
		d.Previous = a.pslot(st.PreviousSlot, st.PreviousRevision)
	}
	return d
}

func (a *App) pslot(s state.Slot, rev string) proxy.Slot {
	if s == "" {
		return proxy.Slot{}
	}
	return proxy.Slot{Name: string(s), Revision: rev, Port: a.port(s)}
}

func (a *App) waitHealthy(ctx context.Context, targets []proxy.Target, rev string) error {
	if len(targets) == 0 {
		return errors.New("no health check targets configured")
	}
	hc := a.cfg.HealthCheck
	check := func(c context.Context) error { return a.probe.CheckAll(c, targets, rev) }
	return health.WaitHealthy(ctx, check, hc.Interval.Duration, hc.SuccessThreshold, hc.ProgressDeadline.Duration)
}

func describe(ts []proxy.Target) string {
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Name)
	}
	return strings.Join(names, ", ")
}

func (a *App) project(slot state.Slot, dir, rev string) *compose.Project {
	return &compose.Project{
		Prefix: a.global.Docker,
		Name:   a.cfg.Name + "-" + string(slot),
		Dir:    dir,
		File:   a.cfg.Source.ComposeFile,
		Pull:   a.cfg.Source.Pull,
		App:    a.cfg.Name,
		Slot:   string(slot),
		Port:   a.port(slot),
		Rev:    rev,
		Runner: a.run,
	}
}

func (a *App) port(s state.Slot) int {
	switch s {
	case state.Blue:
		return a.cfg.Slots.Blue.Port
	case state.Green:
		return a.cfg.Slots.Green.Port
	}
	return 0
}

func (a *App) slotDir(s state.Slot) string {
	return filepath.Join(a.global.StateDir, "slots", a.cfg.Name, string(s))
}

func (a *App) composeDir(s state.Slot) string {
	return filepath.Join(a.slotDir(s), filepath.FromSlash(filepath.Clean(a.cfg.Source.Path)))
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

type atomicCounter struct{ n atomic.Int64 }

func (c *atomicCounter) add(d int64) { c.n.Add(d) }

// Load returns the current value.
func (c *atomicCounter) Load() int64 { return c.n.Load() }
