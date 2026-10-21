// Package config loads and validates the duod operator configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Duration is a time.Duration that unmarshals from strings like "30s" or "5m".
type Duration struct{ time.Duration }

// UnmarshalYAML implements the interface-style YAML unmarshaler.
func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// Config is the top-level operator configuration.
type Config struct {
	// Listen is the address of the REST API / webhook server.
	Listen string `yaml:"listen"`
	// StateDir holds the persisted rollout state and per-slot checkouts.
	StateDir string `yaml:"stateDir"`
	// APIToken protects mutating API endpoints (Bearer token). Supports ${ENV}.
	APIToken string `yaml:"apiToken"`
	// ReconcileInterval is how often the state machine is evaluated.
	ReconcileInterval Duration `yaml:"reconcileInterval"`
	// Docker is the command prefix used to call Docker Compose.
	Docker []string `yaml:"docker"`
	// Apache holds the commands used to validate and gracefully reload Apache.
	Apache ApacheCommands `yaml:"apache"`
	// Traefik configures the Traefik (file provider) backend.
	Traefik TraefikConfig `yaml:"traefik"`
	// WebhookSecret protects the global webhook (/api/v1/webhook) that
	// refreshes every app at once, e.g. one push to a monorepo.
	WebhookSecret string `yaml:"webhookSecret"`
	// MaxConcurrentRollouts limits how many apps deploy at the same time
	// (image pulls, container starts). 0 = unlimited.
	MaxConcurrentRollouts int   `yaml:"maxConcurrentRollouts"`
	Apps                  []App `yaml:"apps"`
}

// TraefikConfig describes how duod talks to a Traefik running in Docker.
type TraefikConfig struct {
	// DynamicDir is watched by Traefik's file provider
	// (providers.file.directory); duod writes one <app>.yaml per app.
	DynamicDir string `yaml:"dynamicDir"`
	// Network is the Docker network shared by Traefik and the slots.
	// duod creates it if it is missing.
	Network string `yaml:"network"`
	// ProbeURL is the internal Traefik entrypoint duod uses for health
	// checks and switch confirmation (http://127.0.0.1:8099 on the host,
	// http://traefik:8099 when duod runs in Docker).
	ProbeURL string `yaml:"probeURL"`
	// ProbeEntryPoint is that entrypoint's name in Traefik's static config.
	ProbeEntryPoint string `yaml:"probeEntryPoint"`
	// ConfirmTimeout bounds the wait until Traefik has loaded a new file.
	ConfirmTimeout Duration `yaml:"confirmTimeout"`
}

// ApacheCommands are argv lists, e.g. ["apachectl", "graceful"].
type ApacheCommands struct {
	ConfigTest []string `yaml:"configTest"`
	Reload     []string `yaml:"reload"`
}

// App describes one blue/green managed application.
type App struct {
	Name        string      `yaml:"name"`
	Source      Source      `yaml:"source"`
	Slots       Slots       `yaml:"slots"`
	HealthCheck HealthCheck `yaml:"healthCheck"`
	Strategy    Strategy    `yaml:"strategy"`
	Proxy       Proxy       `yaml:"proxy"`
}

// Source is the Git location of the desired state (Argo CD "source").
type Source struct {
	RepoURL       string   `yaml:"repoURL"`
	Revision      string   `yaml:"revision"` // branch, tag or commit
	Path          string   `yaml:"path"`     // directory inside the repo
	ComposeFile   string   `yaml:"composeFile"`
	PollInterval  Duration `yaml:"pollInterval"`
	WebhookSecret string   `yaml:"webhookSecret"` // GitHub/Gitea HMAC secret, supports ${ENV}
	Pull          string   `yaml:"pull"`          // compose --pull policy: always|missing|never
}

// Slots configure the two colors. Each slot publishes the app on its own loopback port.
type Slots struct {
	Blue  SlotConfig `yaml:"blue"`
	Green SlotConfig `yaml:"green"`
}

// SlotConfig is the per-color configuration.
type SlotConfig struct {
	Port int `yaml:"port"`
}

// HealthCheck is the pre-promotion analysis run against the preview slot.
type HealthCheck struct {
	Host             string   `yaml:"host"`
	Path             string   `yaml:"path"`
	ExpectStatus     int      `yaml:"expectStatus"`
	Interval         Duration `yaml:"interval"`
	Timeout          Duration `yaml:"timeout"`
	SuccessThreshold int      `yaml:"successThreshold"`
	FailureThreshold int      `yaml:"failureThreshold"`
	ProgressDeadline Duration `yaml:"progressDeadline"`
	// RevisionHeader (optional, e.g. "X-Revision"): the app must echo the
	// commit it was deployed from (duod passes it as ${BG_REVISION}).
	RevisionHeader string `yaml:"revisionHeader"`
}

// Strategy mirrors the Argo Rollouts blueGreen strategy fields.
type Strategy struct {
	AutoPromotionEnabled  bool                   `yaml:"autoPromotionEnabled"`
	AutoPromotionSeconds  int                    `yaml:"autoPromotionSeconds"`
	ScaleDownDelay        Duration               `yaml:"scaleDownDelay"`
	PostPromotionAnalysis *PostPromotionAnalysis `yaml:"postPromotionAnalysis"`
	SelfHeal              *bool                  `yaml:"selfHeal"`
}

// PostPromotionAnalysis probes the public URL (through Apache) after the switch
// and rolls back automatically when it fails.
type PostPromotionAnalysis struct {
	URL              string   `yaml:"url"`
	Duration         Duration `yaml:"duration"`
	Interval         Duration `yaml:"interval"`
	FailureThreshold int      `yaml:"failureThreshold"`
	ExpectStatus     int      `yaml:"expectStatus"`
}

// Proxy selects the traffic switch for this app.
type Proxy struct {
	// Type is "traefik" or "apache".
	Type string `yaml:"type"`

	// apache: the include file duod owns and its template.
	ConfigFile string `yaml:"configFile"`
	Template   string `yaml:"template"`

	// traefik: every route is switched atomically (one file write).
	Routes []Route `yaml:"routes"`
}

// Route publishes one compose service of the app through Traefik.
type Route struct {
	Name     string `yaml:"name"`     // route id, e.g. "web"
	Service  string `yaml:"service"`  // compose service name
	Port     int    `yaml:"port"`     // container port
	Replicas int    `yaml:"replicas"` // compose replicas (container index 1..n)
	Scheme   string `yaml:"scheme"`   // http (default) or https/h2c
	// Traefik router settings for public traffic.
	Rule        string         `yaml:"rule"`
	EntryPoints []string       `yaml:"entryPoints"`
	Middlewares []string       `yaml:"middlewares"`
	TLS         map[string]any `yaml:"tls"`
	Priority    int            `yaml:"priority"`
	// PreviewRule (optional) exposes the preview slot, e.g. Host(`preview.example.com`).
	PreviewRule string `yaml:"previewRule"`
	// HealthPath (optional) is checked on this route during analysis, in
	// addition to healthCheck.path on the first route.
	HealthPath string `yaml:"healthPath"`
}

// DefaultTemplate routes all traffic to the active slot and, if a preview
// exists, exposes it under /_preview/ for smoke tests.
const DefaultTemplate = `ProxyPreserveHost On
ProxyPass        /_preview/ http://127.0.0.1:{{.PreviewOrActivePort}}/
ProxyPassReverse /_preview/ http://127.0.0.1:{{.PreviewOrActivePort}}/
ProxyPass        / http://127.0.0.1:{{.ActivePort}}/
ProxyPassReverse / http://127.0.0.1:{{.ActivePort}}/
`

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// Load reads, defaults and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse parses YAML bytes into a validated Config.
func Parse(raw []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:9800"
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/duod"
	}
	c.APIToken = os.ExpandEnv(c.APIToken)
	if c.ReconcileInterval.Duration == 0 {
		c.ReconcileInterval.Duration = 5 * time.Second
	}
	if len(c.Docker) == 0 {
		c.Docker = []string{"docker", "compose"}
	}
	c.WebhookSecret = os.ExpandEnv(c.WebhookSecret)
	if c.Traefik.DynamicDir == "" {
		c.Traefik.DynamicDir = "/etc/duod/traefik"
	}
	if c.Traefik.Network == "" {
		c.Traefik.Network = "duod-edge"
	}
	if c.Traefik.ProbeURL == "" {
		c.Traefik.ProbeURL = "http://127.0.0.1:8099"
	}
	c.Traefik.ProbeURL = strings.TrimRight(c.Traefik.ProbeURL, "/")
	if c.Traefik.ProbeEntryPoint == "" {
		c.Traefik.ProbeEntryPoint = "duod"
	}
	if c.Traefik.ConfirmTimeout.Duration == 0 {
		c.Traefik.ConfirmTimeout.Duration = 30 * time.Second
	}
	if len(c.Apache.ConfigTest) == 0 {
		c.Apache.ConfigTest = []string{"apachectl", "configtest"}
	}
	if len(c.Apache.Reload) == 0 {
		c.Apache.Reload = []string{"apachectl", "graceful"}
	}
	for i := range c.Apps {
		a := &c.Apps[i]
		a.Source.RepoURL = os.ExpandEnv(a.Source.RepoURL)
		a.Source.WebhookSecret = os.ExpandEnv(a.Source.WebhookSecret)
		if a.Source.Revision == "" {
			a.Source.Revision = "main"
		}
		if a.Source.Path == "" {
			a.Source.Path = "."
		}
		if a.Source.ComposeFile == "" {
			a.Source.ComposeFile = "compose.yaml"
		}
		if a.Source.PollInterval.Duration == 0 {
			a.Source.PollInterval.Duration = time.Minute
		}
		if a.Source.Pull == "" {
			a.Source.Pull = "missing"
		}
		h := &a.HealthCheck
		if h.Host == "" {
			h.Host = "127.0.0.1"
		}
		if h.Path == "" {
			h.Path = "/"
		}
		if h.ExpectStatus == 0 {
			h.ExpectStatus = 200
		}
		if h.Interval.Duration == 0 {
			h.Interval.Duration = 2 * time.Second
		}
		if h.Timeout.Duration == 0 {
			h.Timeout.Duration = 2 * time.Second
		}
		if h.SuccessThreshold == 0 {
			h.SuccessThreshold = 3
		}
		if h.FailureThreshold == 0 {
			h.FailureThreshold = 3
		}
		if h.ProgressDeadline.Duration == 0 {
			h.ProgressDeadline.Duration = 5 * time.Minute
		}
		if a.Strategy.ScaleDownDelay.Duration == 0 {
			a.Strategy.ScaleDownDelay.Duration = 10 * time.Minute
		}
		if a.Strategy.SelfHeal == nil {
			t := true
			a.Strategy.SelfHeal = &t
		}
		if p := a.Strategy.PostPromotionAnalysis; p != nil {
			if p.Duration.Duration == 0 {
				p.Duration.Duration = time.Minute
			}
			if p.Interval.Duration == 0 {
				p.Interval.Duration = 5 * time.Second
			}
			if p.FailureThreshold == 0 {
				p.FailureThreshold = 3
			}
			if p.ExpectStatus == 0 {
				p.ExpectStatus = 200
			}
		}
		if a.Proxy.Type == "" {
			a.Proxy.Type = "apache"
			if len(a.Proxy.Routes) > 0 {
				a.Proxy.Type = "traefik"
			}
		}
		if a.Proxy.Template == "" {
			a.Proxy.Template = DefaultTemplate
		}
		for j := range a.Proxy.Routes {
			r := &a.Proxy.Routes[j]
			if r.Service == "" {
				r.Service = r.Name
			}
			if r.Replicas == 0 {
				r.Replicas = 1
			}
			if r.Scheme == "" {
				r.Scheme = "http"
			}
			if len(r.EntryPoints) == 0 {
				r.EntryPoints = []string{"websecure"}
			}
		}
	}
}

func (c *Config) validate() error {
	var errs []string
	seen := map[string]bool{}
	ports := map[int]string{}
	if len(c.Apps) == 0 {
		errs = append(errs, "no apps configured")
	}
	for _, a := range c.Apps {
		if !nameRe.MatchString(a.Name) {
			errs = append(errs, fmt.Sprintf("app name %q must match %s", a.Name, nameRe))
		}
		if seen[a.Name] {
			errs = append(errs, fmt.Sprintf("duplicate app %q", a.Name))
		}
		seen[a.Name] = true
		if a.Source.RepoURL == "" {
			errs = append(errs, fmt.Sprintf("app %q: source.repoURL is required", a.Name))
		}
		if strings.Contains(a.Source.Path, "..") {
			errs = append(errs, fmt.Sprintf("app %q: source.path must not contain '..'", a.Name))
		}
		switch a.Source.Pull {
		case "always", "missing", "never":
		default:
			errs = append(errs, fmt.Sprintf("app %q: source.pull must be always|missing|never", a.Name))
		}
		switch a.Proxy.Type {
		case "traefik":
			errs = append(errs, validateRoutes(a)...)
		case "apache":
			for color, p := range map[string]int{"blue": a.Slots.Blue.Port, "green": a.Slots.Green.Port} {
				if p <= 0 || p > 65535 {
					errs = append(errs, fmt.Sprintf("app %q: slots.%s.port is required", a.Name, color))
					continue
				}
				if owner, ok := ports[p]; ok {
					errs = append(errs, fmt.Sprintf("app %q: port %d already used by %s", a.Name, p, owner))
				}
				ports[p] = a.Name + "/" + color
			}
			if a.Proxy.ConfigFile == "" {
				errs = append(errs, fmt.Sprintf("app %q: proxy.configFile is required", a.Name))
			}
		default:
			errs = append(errs, fmt.Sprintf("app %q: proxy.type must be traefik or apache", a.Name))
		}
		if p := a.Strategy.PostPromotionAnalysis; p != nil && p.URL == "" {
			errs = append(errs, fmt.Sprintf("app %q: postPromotionAnalysis.url is required", a.Name))
		}
	}
	if c.MaxConcurrentRollouts < 0 {
		errs = append(errs, "maxConcurrentRollouts must be >= 0")
	}
	if len(errs) > 0 {
		return errors.New("invalid config:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

var routeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

func validateRoutes(a App) []string {
	var errs []string
	if len(a.Proxy.Routes) == 0 {
		return []string{fmt.Sprintf("app %q: proxy.routes is required for traefik", a.Name)}
	}
	seen := map[string]bool{}
	for _, r := range a.Proxy.Routes {
		where := fmt.Sprintf("app %q route %q", a.Name, r.Name)
		if !routeNameRe.MatchString(r.Name) {
			errs = append(errs, where+": name must match "+routeNameRe.String())
		}
		if seen[r.Name] {
			errs = append(errs, where+": duplicate route")
		}
		seen[r.Name] = true
		if r.Port <= 0 || r.Port > 65535 {
			errs = append(errs, where+": port (container port) is required")
		}
		if r.Rule == "" {
			errs = append(errs, where+": rule is required, e.g. Host(`app.example.com`)")
		}
		if r.Replicas < 1 || r.Replicas > 50 {
			errs = append(errs, where+": replicas must be 1..50")
		}
		switch r.Scheme {
		case "http", "https", "h2c":
		default:
			errs = append(errs, where+": scheme must be http, https or h2c")
		}
	}
	return errs
}
