// Package api exposes status, actions, webhooks and Prometheus metrics.
//
//	GET  /healthz
//	GET  /metrics
//	GET  /api/v1/apps
//	GET  /api/v1/apps/{name}
//	POST /api/v1/apps/{name}/{sync|promote|abort|rollback|retry}   (Bearer token)
//	POST /api/v1/apps/{name}/webhook                               (HMAC or token)
//	POST /api/v1/webhook                 refresh ALL apps, e.g. monorepo push
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pierrejochem/duod/internal/controller"
	"github.com/pierrejochem/duod/internal/state"
)

// Server is the HTTP API.
type Server struct {
	Ctrl  *controller.Controller
	Token string
	Log   *slog.Logger
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /api/v1/apps", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.Ctrl.List()) })
	mux.HandleFunc("GET /api/v1/apps/{name}", s.get)
	mux.HandleFunc("POST /api/v1/apps/{name}/webhook", s.webhook)
	mux.HandleFunc("POST /api/v1/webhook", s.globalWebhook)
	mux.HandleFunc("POST /api/v1/apps/{name}/{action}", s.auth(s.action))
	return mux
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	a, err := s.Ctrl.App(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, a.Snapshot())
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	a, err := s.Ctrl.App(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	ctx := s.Ctrl.Context()
	act := r.PathValue("action")
	s.Log.Info("action", "app", r.PathValue("name"), "action", act, "remote", r.RemoteAddr)
	switch act {
	case "sync":
		a.Refresh()
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "refresh scheduled"})
		return
	case "retry":
		a.Retry()
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "failed marker cleared, reconciling"})
		return
	case "promote":
		err = a.Promote(ctx)
	case "abort":
		err = a.Abort(ctx)
	case "rollback":
		err = a.Rollback(ctx)
	default:
		writeErr(w, http.StatusNotFound, fmt.Errorf("unknown action %q", act))
		return
	}
	if errors.Is(err, controller.ErrConflict) {
		writeErr(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a.Snapshot())
}

// webhook accepts GitHub (X-Hub-Signature-256), Gitea/Forgejo
// (X-Gitea-Signature), GitLab (X-Gitlab-Token) or a Bearer token.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, err := s.Ctrl.App(name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !s.verifyWebhook(r, body, s.Ctrl.WebhookSecret(name)) {
		writeErr(w, http.StatusUnauthorized, errors.New("invalid webhook signature"))
		return
	}
	a.Refresh()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "refresh scheduled"})
}

// globalWebhook refreshes every app. Only apps whose source.path changed in
// the pushed commit get a new target revision, and they roll out in parallel.
func (s *Server) globalWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !s.verifyWebhook(r, body, s.Ctrl.GlobalWebhookSecret()) {
		writeErr(w, http.StatusUnauthorized, errors.New("invalid webhook signature"))
		return
	}
	n := s.Ctrl.RefreshAll()
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "refresh scheduled", "apps": n})
}

func (s *Server) verifyWebhook(r *http.Request, body []byte, secret string) bool {
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" {
			return hmac.Equal([]byte(strings.TrimPrefix(sig, "sha256=")), []byte(want))
		}
		if sig := r.Header.Get("X-Gitea-Signature"); sig != "" {
			return hmac.Equal([]byte(sig), []byte(want))
		}
		if tok := r.Header.Get("X-Gitlab-Token"); tok != "" {
			return subtle.ConstantTimeCompare([]byte(tok), []byte(secret)) == 1
		}
	}
	return s.bearerOK(r)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.bearerOK(r) {
			writeErr(w, http.StatusUnauthorized, errors.New("missing or invalid bearer token"))
			return
		}
		next(w, r)
	}
}

func (s *Server) bearerOK(r *http.Request) bool {
	if s.Token == "" {
		return true // explicitly unauthenticated (listen on loopback only!)
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	var b strings.Builder
	b.WriteString("# HELP duod_app_phase Current rollout phase (1 for the active phase).\n# TYPE duod_app_phase gauge\n")
	phases := []state.Phase{state.PhaseNew, state.PhaseProgressing, state.PhasePaused, state.PhaseHealthy, state.PhaseDegraded}
	apps := s.Ctrl.List()
	for _, a := range apps {
		for _, p := range phases {
			fmt.Fprintf(&b, "duod_app_phase{app=%q,phase=%q} %d\n", a.Name, p, b2i(a.Phase == p))
		}
	}
	b.WriteString("# HELP duod_app_synced 1 if the active revision equals the Git target.\n# TYPE duod_app_synced gauge\n")
	for _, a := range apps {
		fmt.Fprintf(&b, "duod_app_synced{app=%q} %d\n", a.Name, b2i(a.Sync == state.Synced))
	}
	b.WriteString("# HELP duod_app_active_healthy 1 if the active slot passes health checks.\n# TYPE duod_app_active_healthy gauge\n")
	for _, a := range apps {
		fmt.Fprintf(&b, "duod_app_active_healthy{app=%q,slot=%q} %d\n", a.Name, a.ActiveSlot, b2i(a.ActiveHealthy))
	}
	b.WriteString("# HELP duod_app_active_since_seconds Unix time of the last promotion.\n# TYPE duod_app_active_since_seconds gauge\n")
	for _, a := range apps {
		fmt.Fprintf(&b, "duod_app_active_since_seconds{app=%q} %d\n", a.Name, unix(a.ActiveSince))
	}
	b.WriteString("# HELP duod_rollouts_in_flight Apps currently deploying or under analysis.\n# TYPE duod_rollouts_in_flight gauge\n")
	fmt.Fprintf(&b, "duod_rollouts_in_flight %d\n", s.Ctrl.RolloutsInFlight())
	_, _ = io.WriteString(w, b.String())
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
