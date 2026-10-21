package config

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultsAndDurations(t *testing.T) {
	t.Setenv("WEBHOOK_SECRET", "abc")
	c, err := Parse([]byte(`
apps:
  - name: shop
    source:
      repoURL: https://example.com/deploy.git
      webhookSecret: ${WEBHOOK_SECRET}
    slots: {blue: {port: 8081}, green: {port: 8082}}
    healthCheck: {progressDeadline: 90s}
    proxy:
      configFile: /etc/apache2/duod/shop.conf
      template: |
        ProxyPass / http://127.0.0.1:{{.ActivePort}}/ # ${APACHE_LOG_DIR} untouched
`))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Apps[0]
	if a.Source.WebhookSecret != "abc" || a.Source.Revision != "main" || a.Source.ComposeFile != "compose.yaml" {
		t.Fatalf("source defaults: %+v", a.Source)
	}
	if a.HealthCheck.ProgressDeadline.Duration != 90*time.Second || a.Strategy.ScaleDownDelay.Duration != 10*time.Minute {
		t.Fatalf("durations: %v %v", a.HealthCheck.ProgressDeadline, a.Strategy.ScaleDownDelay)
	}
	if !strings.Contains(a.Proxy.Template, "${APACHE_LOG_DIR}") {
		t.Fatal("apache variables in templates must not be env-expanded")
	}
}

func TestValidation(t *testing.T) {
	_, err := Parse([]byte(`
apps:
  - name: Shop_1
    source: {repoURL: "", path: ../etc}
    slots: {blue: {port: 8081}, green: {port: 8081}}
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"app name", "repoURL", "'..'", "already used", "configFile"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}
