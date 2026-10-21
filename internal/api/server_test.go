package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pierrejochem/duod/internal/config"
	"github.com/pierrejochem/duod/internal/controller"
	"github.com/pierrejochem/duod/internal/runner"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg, err := config.Parse([]byte(`
stateDir: ` + t.TempDir() + `
apps:
  - name: shop
    source: {repoURL: /nonexistent, webhookSecret: s3cret}
    slots: {blue: {port: 18081}, green: {port: 18082}}
    proxy: {configFile: /tmp/x.conf}
`))
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := controller.New(cfg, runner.Exec{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Server{Ctrl: ctrl, Token: "tok", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url string, hdr map[string]string, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestAuthAndWebhook(t *testing.T) {
	srv := newServer(t)
	base := srv.URL + "/api/v1/apps/shop"

	if c := post(t, base+"/promote", nil, ""); c != http.StatusUnauthorized {
		t.Fatalf("promote without token = %d", c)
	}
	if c := post(t, base+"/promote", map[string]string{"Authorization": "Bearer tok"}, ""); c != http.StatusConflict {
		t.Fatalf("promote on New app = %d, want 409", c)
	}
	if c := post(t, base+"/sync", map[string]string{"Authorization": "Bearer tok"}, ""); c != http.StatusAccepted {
		t.Fatalf("sync = %d", c)
	}

	body := `{"ref":"refs/heads/main"}`
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(body))
	sig := hex.EncodeToString(mac.Sum(nil))
	if c := post(t, base+"/webhook", map[string]string{"X-Hub-Signature-256": "sha256=" + sig}, body); c != http.StatusAccepted {
		t.Fatalf("github webhook = %d", c)
	}
	if c := post(t, base+"/webhook", map[string]string{"X-Gitea-Signature": sig}, body); c != http.StatusAccepted {
		t.Fatalf("gitea webhook = %d", c)
	}
	if c := post(t, base+"/webhook", map[string]string{"X-Hub-Signature-256": "sha256=deadbeef"}, body); c != http.StatusUnauthorized {
		t.Fatalf("bad signature = %d", c)
	}
	if c := post(t, srv.URL+"/api/v1/apps/nope/webhook", nil, body); c != http.StatusNotFound {
		t.Fatalf("unknown app = %d", c)
	}

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `duod_app_phase{app="shop",phase="New"} 1`) {
		t.Fatalf("metrics:\n%s", b)
	}
}
