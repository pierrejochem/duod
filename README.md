<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/duod-logo-dark.svg">
    <img src="assets/duod-logo.svg" alt="duod" width="420">
  </picture>
</p>

<p align="center"><b>GitOps blue/green for Docker Compose, switched by Traefik</b></p>

**duod** (two slots, one daemon) is a small Go operator that brings the Argo CD and
Argo Rollouts (`blueGreen`) workflow to a plain Docker host. Git is the source
of truth, Docker Compose runs the two colors of every service, and Traefik in
Docker switches the traffic. Each service is independent: it has its own
folder in Git and its own rollout, and several services can roll out at the
same time. Apache on the host is still supported as an alternative switch.

```
                                ┌──────────────────────── Docker host ────────────────────────────┐
 GitOps repo        webhook /   │         ┌─► compose shop-blue   ─┐                              │
  apps/shop/  ───►  poll  ──► duod ───────┼─► compose shop-green  ─┤   network duod-edge          │
  apps/api/                     │  │      ├─► compose api-blue    ─┼──────────────┐               │
  apps/admin/                   │  │      └─► compose api-green   ─┘              │               │
                                │  │                                              ▼               │
                                │  └─ writes /etc/duod/traefik/<app>.yaml ──► Traefik :443 ◄── clients
                                │     probes Traefik :8099 (private) ◄────────────┘               │
                                └─────────────────────────────────────────────────────────────────┘
```

> **Live demo:** [`demo/`](demo/) starts Portus (registry), Traefik, duod, three
> demo services and a live dashboard with one command, and includes a 15-minute
> presenter run-book.

## Try it in five minutes

You need Docker with Compose v2, `git`, `openssl` and `curl`. You don't need Go
for the demo, because duod is built inside Docker.

```bash
make demo-up                          # registry, Traefik, duod, 3 services, dashboard
make demo-open                        # dashboard, shop and Portus in your browser
cd demo && ./demo.sh release shop 1.1.0       # roll out a new version of one service
cd demo && ./demo.sh release api 1.1.0 --broken   # a release that fails its health check
make demo-down                        # stop (make demo-reset wipes all demo state)
```

Run `make help` for every target. The dashboard is at http://dashboard.localhost.

## Independent services, in parallel

- **One app per service.** Each app has its own `source.path`, its own
  `<app>-blue` / `<app>-green` Compose projects, its own state machine and its
  own Traefik file (`<app>.yaml`). A failing rollout of `api` never blocks,
  delays or rolls back `shop`.
- **Concurrent rollouts.** Every app runs its own reconcile loop, so a push that
  changes three services rolls all three out at the same time.
  `maxConcurrentRollouts` caps how many deploy at once. The others are queued
  with a `RolloutQueued` event.
- **Monorepo-aware.** An app's target revision is the newest commit that
  touched *its* `source.path`. Committing to `apps/api` doesn't redeploy
  `apps/shop`, even though both track `main`.
- **One webhook for everything.** `POST /api/v1/webhook` refreshes all apps.
  Only the ones whose folder changed roll out.
- **Coupled services** that must switch together (for example a frontend and
  its BFF) can be listed as several `routes` of one app. They share one
  Traefik file and change color in a single atomic write.

Verified end to end against a real Traefik (3.5 and 3.6): two services were rolled out
concurrently under continuous load (about 7,400 requests each, 0 errors). Then a
broken `api` release was rejected while `shop` shipped at the same time, still
with 0 failed requests on either host.

## How it maps to Argo

| Argo CD / Rollouts                | duod                                                     |
|-----------------------------------|----------------------------------------------------------|
| Application `source`              | `source.repoURL / revision / path`                       |
| Sync / OutOfSync                  | Git target revision vs. active revision                  |
| ReplicaSets (stable / new)        | Compose projects `<app>-blue` and `<app>-green`          |
| active Service                    | Traefik router → `duod-<app>-<route>-<color>` service    |
| preview Service                   | `previewRule` router, plus private probe routers         |
| prePromotionAnalysis              | `healthCheck` through Traefik (n consecutive successes)  |
| `autoPromotionEnabled/Seconds`    | same fields                                              |
| postPromotionAnalysis             | `postPromotionAnalysis` → automatic rollback             |
| `scaleDownDelaySeconds`           | `scaleDownDelay` (old slot is then *stopped*, not removed) |
| `kubectl argo rollouts promote/abort/undo` | `duod promote / abort / rollback`               |
| self-heal                         | restart the active slot, revert manual edits of the proxy config |

## Rollout lifecycle

1. **Detect.** A webhook or poll fetches Git and resolves the newest commit that touched `source.path`.
2. **Preview.** The commit is extracted into the idle slot's own directory, so each slot has an immutable checkout of its revision. Whatever still runs in that slot is removed, then `docker compose up -d` starts the new version as project `<app>-<color>`.
3. **Wiring check.** Every routed container (`<app>-<color>-<service>-<n>`) must exist and be attached to `duod-edge`. A missing `networks:` entry fails immediately with a clear message, not with a health-check timeout.
4. **Pre-promotion analysis.** duod adds probe routers for the new slot on Traefik's private `duod` entrypoint and checks `healthCheck.path` *through Traefik*. That's the same network path real traffic takes. With `revisionHeader`, the app must also echo the new commit SHA.
5. **Paused or promoted.** The rollout waits for `duod promote`, or for `autoPromotionSeconds`. With `autoPromotionEnabled` it promotes at once.
6. **Switch.** The app's Traefik file is rewritten atomically. Every generated file carries a generation hash that Traefik returns in `X-Duod-Generation`. duod waits until Traefik serves the new generation before it reports the promotion. If Traefik doesn't load it within `confirmTimeout`, the old file is put back. Open connections to the old slot finish normally.
7. **Post-promotion analysis (optional).** The public URL is watched. On failure duod switches back automatically.
8. **Scale down.** After `scaleDownDelay` the old slot is stopped but kept, so `duod rollback` only has to restart it, health-check it and switch.

A failed or aborted revision is recorded in `failedRevision` and isn't retried
on every tick. Push a fix (a new commit) or run `duod retry <app>`. A new
commit that arrives while a rollout is paused replaces the preview.

## Requirements

| Needed for | Requirement |
|---|---|
| Running duod | Docker Engine with Compose v2, and `git` on the host (or in the duod container) |
| Traffic switch | Traefik **v3.6 or newer** when the Docker Engine is 29 or newer. Older Traefik clients speak a Docker API version that Engine 29 rejects (minimum 1.40), so the Docker provider fails with a 400. |
| Building from source | Go 1.24 (see `go.mod`) |

## Setup with Traefik in Docker

```bash
# 1. Traefik (file provider + private probe entrypoint on 127.0.0.1:8099)
sudo mkdir -p /etc/duod/traefik
docker network create duod-edge
docker compose -p traefik -f examples/traefik/compose.yaml up -d

# 2. duod on the host (systemd) ...
make build && sudo make install
sudo cp examples/duod.yaml /etc/duod/duod.yaml        # edit apps, hosts, repo
echo "DUOD_TOKEN=$(openssl rand -hex 32)"          | sudo tee    /etc/duod/env
echo "DUOD_WEBHOOK_SECRET=$(openssl rand -hex 32)" | sudo tee -a /etc/duod/env
sudo duod validate -config /etc/duod/duod.yaml        # prints each app's Traefik file
sudo systemctl enable --now duod

# ... or duod in Docker (examples/duod-in-docker; set traefik.probeURL: http://traefik:8099)
```

Traefik's static config needs three things: the file provider watching the
directory duod writes to, a `duod` entrypoint that is not published publicly,
and the `duod-edge` network. `examples/traefik/compose.yaml` has all three, plus
Let's Encrypt. Middlewares you define yourself (in `examples/traefik/shared.yaml`
or through the Docker provider) can be referenced from routes, as in
`secure-headers@file`.

### Webhook

Point one push webhook at `/api/v1/webhook` (all apps, verified with
`webhookSecret`), or one per app at `/api/v1/apps/<app>/webhook` (verified with
`source.webhookSecret`). duod verifies GitHub (`X-Hub-Signature-256`),
Gitea/Forgejo (`X-Gitea-Signature`) and GitLab (`X-Gitlab-Token`). The webhook
only triggers a fetch, so the payload is never trusted. You can expose the
endpoint through Traefik, for example with a router for `PathPrefix(/duod-hook)`
on your admin host pointing at `http://<host>:9800`.

## Writing a service's compose file

duod runs the same file twice and injects `BG_APP`, `BG_SLOT` and `BG_REVISION`
(see `examples/deploy-repo`).

- Attach every **routed** service to the external network `duod-edge`. Traefik
  reaches it as `<app>-<color>-<service>-<n>:<port>`.
- Don't publish `ports` and don't set `container_name`, because both colors run side by side.
- Set `routes[].replicas` to the service's `deploy.replicas`.
- Keep **stateful services out of the slot**. Run the database, Redis and
  queues in a separate Compose project. Both colors use them during the switch.
- Keep **migrations backward-compatible** (expand, then contract). The old
  color must keep working against the new schema until it's scaled down, and
  also if you roll back.
- Background workers in a slot run twice while both colors are up. Make jobs
  idempotent, or move singleton workers into the shared project.

## CLI

```bash
export DUOD_SERVER=http://127.0.0.1:9800 DUOD_TOKEN=...
duod list
duod get shop          # phase, sync, active / preview / previous slot
duod history shop      # event log (RolloutQueued, RolloutStarted, PreviewHealthy, Promoted, ...)
duod sync shop         # fetch Git now
duod promote shop      # Paused -> switch traffic
duod abort shop        # cancel the analysis or discard the paused preview
duod rollback shop     # back to the previous slot, no Git change needed
duod retry shop        # clear the failed-revision marker
```

```
APP   PHASE     SYNC       ACTIVE  REVISION  PREVIEW  TARGET    MESSAGE
api   Degraded  OutOfSync  green   578ef707  -        64dc3ce3  revision 64dc3ce3 failed (...); still serving 578ef707 from green
shop  Healthy   Synced     blue    64dc3ce3  -        64dc3ce3  serving 64dc3ce3 from blue
```

The REST API underneath:

| Method | Path | |
|---|---|---|
| GET  | `/api/v1/apps`, `/api/v1/apps/{app}` | status as JSON |
| POST | `/api/v1/apps/{app}/{sync,promote,abort,rollback,retry}` | Bearer token |
| POST | `/api/v1/apps/{app}/webhook` | per-app HMAC or token |
| POST | `/api/v1/webhook` | refresh all apps (global HMAC or token) |
| GET  | `/metrics` | `duod_app_phase`, `duod_app_synced`, `duod_app_active_healthy`, `duod_rollouts_in_flight`, ... |

## Apache instead of Traefik

Set `proxy.type: apache` on an app (see `examples/duod-apache.yaml` and
`examples/apache`). Each slot then publishes on its own loopback port
(`127.0.0.1:${BG_PORT}`). duod rewrites an include file, runs `apachectl
configtest` (restoring the old file on failure) and then `apachectl graceful`.
Apache apps are serialized on the configtest and reload step, because Apache
validates its whole configuration at once. Their deploys and analyses still run
in parallel.

## Development

```bash
make build              # bin/duod
make build-arm64        # bin/duod-linux-arm64
make test               # go test -race ./...
make lint               # gofmt + go vet
make validate-examples  # run `duod validate` on every example config
make help               # list all targets, including the demo-* ones
```

## Layout

```
cmd/duod             CLI + operator entry point
internal/controller  per-app reconcile loops, blue/green state machine, rollout limiter
internal/proxy       traffic-switch interface
internal/traefik     dynamic file per app, probe routers, generation confirm, network check
internal/apache      include file, configtest, graceful reload
internal/gitsrc      git mirror, path-scoped revision, per-slot checkout
internal/compose     docker compose per slot
internal/health      pre/post-promotion analysis
internal/api         REST, webhooks, metrics
internal/state       persisted state (/var/lib/duod/state/<app>.json)
```

State survives restarts. If duod dies in the middle of an analysis, the rollout
is redone. A paused preview stays paused.

## Limitations

- One Docker host per operator instance. For several hosts, run one duod per
  host with the same Git repo.
- Traffic switching is all-at-once (blue/green). Canary weights would map
  naturally onto Traefik's `weighted` services and could be added later.
- A path-scoped revision only reacts to changes inside `source.path`. Files
  shared between services (outside their folders) don't trigger a rollout.
- Images are pulled during the rollout (`pull: always`). Build them in CI and
  reference them by tag or digest in Git.
- Requires Traefik v3 (the `Header()` rule syntax and the `noop@internal` service).
  Use v3.6 or newer with Docker Engine 29+.

## License

Apache License 2.0, see [LICENSE](LICENSE).
