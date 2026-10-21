// Command duod is a GitOps blue/green operator for Docker Compose behind Apache.
//
//	duod run      -config /etc/duod/duod.yaml   # start the operator
//	duod validate -config /etc/duod/duod.yaml   # check config + templates
//	duod list | get <app> | history <app>
//	duod sync | promote | abort | rollback | retry <app>
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/pierrejochem/duod/internal/apache"
	"github.com/pierrejochem/duod/internal/api"
	"github.com/pierrejochem/duod/internal/config"
	"github.com/pierrejochem/duod/internal/controller"
	"github.com/pierrejochem/duod/internal/proxy"
	"github.com/pierrejochem/duod/internal/runner"
	"github.com/pierrejochem/duod/internal/state"
	"github.com/pierrejochem/duod/internal/traefik"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = runCmd(args)
	case "validate":
		err = validateCmd(args)
	case "list", "ls":
		err = clientCmd("list", args)
	case "get", "status", "history", "sync", "promote", "abort", "rollback", "retry":
		err = clientCmd(cmd, args)
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `duod - GitOps blue/green operator for Docker Compose + Apache

Operator:
  duod run      [-config duod.yaml] [-log-format text|json]
  duod validate [-config duod.yaml]

Client (uses $DUOD_SERVER, default http://127.0.0.1:9800, and $DUOD_TOKEN):
  duod list
  duod get      <app>      status of one app
  duod history  <app>      event history
  duod sync     <app>      fetch Git now (like 'argocd app sync')
  duod promote  <app>      switch traffic to the paused preview
  duod abort    <app>      cancel rollout / discard preview
  duod rollback <app>      switch traffic back to the previous slot
  duod retry    <app>      clear the failed-revision marker
`)
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/duod/duod.yaml", "config file")
	logFormat := fs.String("log-format", "text", "text or json")
	_ = fs.Parse(args)

	var h slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *logFormat == "json" {
		h = slog.NewJSONHandler(os.Stderr, nil)
	}
	log := slog.New(h)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctrl, err := controller.New(cfg, runner.Exec{}, log)
	if err != nil {
		return err
	}
	if cfg.APIToken == "" {
		log.Warn("apiToken is empty: API actions are unauthenticated - keep listen on loopback")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           (&api.Server{Ctrl: ctrl, Token: cfg.APIToken, Log: log}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("api listening", "addr", cfg.Listen, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("api server", "err", err)
			stop()
		}
	}()

	ctrl.Run(ctx)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func validateCmd(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/duod/duod.yaml", "config file")
	_ = fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	sample := proxy.Data{
		Active:  proxy.Slot{Name: "blue", Revision: "0123456789abcdef"},
		Preview: proxy.Slot{Name: "green", Revision: "fedcba9876543210"},
	}
	for _, a := range cfg.Apps {
		sample.App = a.Name
		switch a.Proxy.Type {
		case "traefik":
			b := &traefik.Backend{App: a.Name, Routes: a.Proxy.Routes, Cfg: cfg.Traefik, HealthPath: a.HealthCheck.Path}
			out, _, err := b.Render(sample)
			if err != nil {
				return fmt.Errorf("app %s: %w", a.Name, err)
			}
			fmt.Printf("app %s OK - traefik, %d route(s), %s -> %s\n--- sample (blue active, green preview) ---\n%s\n",
				a.Name, len(a.Proxy.Routes), a.Source.RepoURL, b.File(), out)
		default:
			site, err := apache.NewSite(a.Name, a.Proxy.ConfigFile, a.Proxy.Template, nil, nil, nil)
			if err != nil {
				return err
			}
			sample.Active.Port, sample.Preview.Port = a.Slots.Blue.Port, a.Slots.Green.Port
			out, err := site.Render(apache.Data{App: a.Name, ActiveSlot: "blue", ActivePort: a.Slots.Blue.Port, ActiveRevision: "0123456789abcdef",
				PreviewSlot: "green", PreviewPort: a.Slots.Green.Port, PreviewRevision: "fedcba9876543210"})
			if err != nil {
				return fmt.Errorf("app %s: template: %w", a.Name, err)
			}
			fmt.Printf("app %s OK - apache, %s (blue:%d green:%d) -> %s\n--- sample (blue active, green preview) ---\n%s\n",
				a.Name, a.Source.RepoURL, a.Slots.Blue.Port, a.Slots.Green.Port, a.Proxy.ConfigFile, out)
		}
	}
	return nil
}

// ---- client ---------------------------------------------------------------

func clientCmd(cmd string, args []string) error {
	server := strings.TrimRight(envOr("DUOD_SERVER", "http://127.0.0.1:9800"), "/")
	client := &http.Client{Timeout: 15 * time.Minute}

	if cmd == "list" {
		var apps []state.App
		if err := do(client, http.MethodGet, server+"/api/v1/apps", &apps); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "APP\tPHASE\tSYNC\tACTIVE\tREVISION\tPREVIEW\tTARGET\tMESSAGE")
		for _, a := range apps {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Phase, a.Sync, dash(string(a.ActiveSlot)),
				dash(short(a.ActiveRevision)), dash(previewStr(a)), dash(short(a.TargetRevision)), a.Message)
		}
		return tw.Flush()
	}

	if len(args) != 1 {
		return fmt.Errorf("usage: duod %s <app>", cmd)
	}
	app := args[0]
	var a state.App
	switch cmd {
	case "get", "status", "history":
		if err := do(client, http.MethodGet, server+"/api/v1/apps/"+app, &a); err != nil {
			return err
		}
	default:
		var raw json.RawMessage
		if err := do(client, http.MethodPost, server+"/api/v1/apps/"+app+"/"+cmd, &raw); err != nil {
			return err
		}
		if cmd == "sync" || cmd == "retry" {
			fmt.Printf("%s: %s\n", app, string(raw))
			return nil
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return err
		}
	}
	if cmd == "history" {
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "TIME\tEVENT\tSLOT\tREVISION\tMESSAGE")
		for _, e := range a.History {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Time.Local().Format("2006-01-02 15:04:05"), e.Type, dash(string(e.Slot)), dash(short(e.Revision)), e.Message)
		}
		return tw.Flush()
	}
	printApp(a)
	return nil
}

func printApp(a state.App) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "Name:\t%s\n", a.Name)
	fmt.Fprintf(tw, "Phase:\t%s\n", a.Phase)
	fmt.Fprintf(tw, "Sync:\t%s\n", a.Sync)
	fmt.Fprintf(tw, "Message:\t%s\n", a.Message)
	fmt.Fprintf(tw, "Target:\t%s %s\n", dash(short(a.TargetRevision)), a.TargetSubject)
	fmt.Fprintf(tw, "Active:\t%s @ %s (healthy=%v, since %s)\n", dash(string(a.ActiveSlot)), dash(short(a.ActiveRevision)), a.ActiveHealthy, ts(a.ActiveSince))
	fmt.Fprintf(tw, "Preview:\t%s\n", dash(previewStr(a)))
	prev := "-"
	if a.PreviousSlot != "" {
		prev = fmt.Sprintf("%s @ %s", a.PreviousSlot, short(a.PreviousRevision))
		if !a.ScaleDownAt.IsZero() {
			prev += " (scale down at " + ts(a.ScaleDownAt) + ")"
		} else {
			prev += " (stopped, rollback ready)"
		}
	}
	fmt.Fprintf(tw, "Previous:\t%s\n", prev)
	if a.FailedRevision != "" {
		fmt.Fprintf(tw, "Failed:\t%s\n", short(a.FailedRevision))
	}
	if a.FetchError != "" {
		fmt.Fprintf(tw, "Git error:\t%s\n", a.FetchError)
	}
	_ = tw.Flush()
}

func do(c *http.Client, method, url string, out any) error {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return err
	}
	if tok := os.Getenv("DUOD_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s (HTTP %d)", e.Error, resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

func previewStr(a state.App) string {
	if a.PreviewSlot == "" {
		return ""
	}
	s := fmt.Sprintf("%s @ %s", a.PreviewSlot, short(a.PreviewRevision))
	if !a.PreviewReadySince.IsZero() {
		s += " (healthy)"
	}
	return s
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func ts(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
