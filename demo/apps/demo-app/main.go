// demo-app is the sample service of the duod live demo. One binary, two
// faces: KIND=web renders a full-screen page that shows which color and
// version answered (and updates itself every second), KIND=api answers JSON.
//
// Build-time (ldflags): version, accent, healthy. Runtime (env from the slot's
// compose file): APP_NAME, BG_SLOT, BG_REVISION, KIND, START_DELAY.
package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

var (
	version = "dev"
	accent  = "#1D8CF8"
	healthy = "true"
)

type info struct {
	App      string `json:"app"`
	Kind     string `json:"kind"`
	Version  string `json:"version"`
	Accent   string `json:"accent"`
	Slot     string `json:"slot"`
	Revision string `json:"revision"`
	Host     string `json:"host"`
	Uptime   string `json:"uptime"`
	Requests int64  `json:"requests"`
}

var (
	started  = time.Now()
	requests atomic.Int64
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func current() info {
	host, _ := os.Hostname()
	rev := os.Getenv("BG_REVISION")
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return info{
		App: env("APP_NAME", "demo"), Kind: env("KIND", "web"), Version: version, Accent: accent,
		Slot: env("BG_SLOT", "?"), Revision: rev, Host: host,
		Uptime: time.Since(started).Round(time.Second).String(), Requests: requests.Load(),
	}
}

func common(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		i := current()
		w.Header().Set("X-Revision", i.Revision) // duod healthCheck.revisionHeader
		w.Header().Set("X-Slot", i.Slot)
		w.Header().Set("X-Version", i.Version)
		w.Header().Set("Access-Control-Allow-Origin", "*") // the dashboard polls us
		w.Header().Set("Access-Control-Expose-Headers", "X-Revision, X-Slot, X-Version")
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	}
}

func main() {
	if d, err := strconv.Atoi(os.Getenv("START_DELAY")); err == nil && d > 0 {
		log.Printf("simulating a slow start: %ds", d)
		time.Sleep(time.Duration(d) * time.Second)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", common(func(w http.ResponseWriter, _ *http.Request) {
		if healthy != "true" {
			http.Error(w, "unhealthy: this build is broken on purpose", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	}))
	mux.HandleFunc("/api/info", common(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(current())
	}))
	mux.HandleFunc("/", common(func(w http.ResponseWriter, r *http.Request) {
		if current().Kind == "api" || r.URL.Path != "/" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"service": current(), "hint": "GET /api/info"})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.Execute(w, current())
	}))
	addr := ":" + env("PORT", "8080")
	log.Printf("%s %s (%s) listening on %s", env("APP_NAME", "demo"), version, env("BG_SLOT", "?"), addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

var page = template.Must(template.New("p").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.App}} {{.Version}}</title>
<style>
:root{--accent:{{.Accent}};--blue:#1D8CF8;--green:#20C38B;--navy:#0B2545}
*{box-sizing:border-box}html,body{margin:0;height:100%}
body{font-family:ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;background:var(--navy);color:#fff;display:flex;flex-direction:column}
header{padding:20px 32px;display:flex;justify-content:space-between;align-items:center;font-size:18px;letter-spacing:.04em;text-transform:uppercase;opacity:.85}
main{flex:1;display:flex;flex-direction:column;align-items:center;justify-content:center;gap:28px;padding:24px;text-align:center}
.app{font-size:clamp(28px,5vw,56px);font-weight:700}
.ver{font-size:clamp(72px,16vw,200px);font-weight:800;line-height:1;color:var(--accent);font-variant-numeric:tabular-nums}
.slot{font-size:clamp(20px,3vw,32px);font-weight:700;padding:12px 28px;border-radius:999px;text-transform:uppercase;letter-spacing:.08em}
.slot.blue{background:var(--blue)}.slot.green{background:var(--green);color:#062b1e}
footer{padding:16px 32px;font:14px ui-monospace,SFMono-Regular,Menlo,monospace;opacity:.75;display:flex;gap:24px;flex-wrap:wrap;justify-content:center}
.dot{width:12px;height:12px;border-radius:50%;background:var(--green);display:inline-block;margin-right:8px}
.err .dot{background:#E5484D}
</style></head><body>
<header><span><span class="dot" id="dot"></span>live</span><span>duod demo</span></header>
<main>
  <div class="app" id="app">{{.App}}</div>
  <div class="ver" id="ver">{{.Version}}</div>
  <div class="slot {{.Slot}}" id="slot">served by {{.Slot}}</div>
</main>
<footer><span id="rev">rev {{.Revision}}</span><span id="host">{{.Host}}</span><span id="upd">updated live, no reload needed</span></footer>
<script>
async function tick(){
  try{
    const r = await fetch('/api/info',{cache:'no-store'}); const i = await r.json();
    document.getElementById('ver').textContent = i.version;
    document.documentElement.style.setProperty('--accent', i.accent);
    const s = document.getElementById('slot'); s.className = 'slot ' + i.slot; s.textContent = 'served by ' + i.slot;
    document.getElementById('rev').textContent = 'rev ' + i.revision;
    document.getElementById('host').textContent = i.host;
    document.title = i.app + ' ' + i.version; document.body.classList.remove('err');
  }catch(e){ document.body.classList.add('err'); }
}
setInterval(tick, 1000);
</script></body></html>`))
