# duod live demo: Portus, Traefik, three independent services

A complete, self-contained setup for presenting duod live on one laptop:

| Component | Role in the demo | URL |
|---|---|---|
| **Portus 2.4.3** + Docker Registry 2.8 + MariaDB | Private registry with users, namespaces and token auth. Every release is pushed here, and duod pulls from here. | http://localhost:3000, registry `localhost:5050` |
| **Traefik v3** | Traffic switch, driven by duod's file provider | http://localhost:8081/dashboard/ |
| **duod** | The GitOps blue/green operator (in a container) | API on `127.0.0.1:9800` |
| **shop**, **api** (2 replicas), **docs** | Three independent demo services, each in its own folder of the GitOps repo | http://shop.localhost, http://api.localhost/api/info, http://docs.localhost |
| **Live dashboard** | State of every service, plus live traffic showing which color answered and every failed request | http://dashboard.localhost |
| **GitOps repo** | A local bare Git repo with a push hook that calls duod's webhook | `.state/gitops` (working copy) |

```
 ./demo.sh release shop 1.1.0
   │ docker build + push ──► registry :5050 ──(token)──► Portus :3000
   │ edit apps/shop/compose.yaml, git push ──► hook ──► duod webhook
   ▼
 duod ── docker compose -p shop-green up (pulls from Portus) ──► analysis via Traefik :8099
      └─ writes /dynamic/shop.yaml ──► Traefik ──► http://shop.localhost now served by green
```

## Requirements

- Docker Desktop (macOS/Windows) or Docker Engine with Compose v2, plus `git`, `openssl` and `curl`.
- About 4 GB of free RAM. Free ports: **80**, 3000, 5050, 8081 and 9800.
- Chrome, Edge or Firefox for the browser parts, since they resolve `*.localhost`
  automatically. For Safari, run `./demo.sh hosts` and add the line to `/etc/hosts`.
- Apple Silicon works. The Portus image is amd64-only and runs emulated, so its
  first start takes a minute or two.

> **About Portus.** SUSE archived Portus in April 2023, and 2.4.3 is its last release.
> It is pinned here and works for a demo. For production, use a maintained
> registry such as Harbor. duod doesn't care which registry you use.

## Before the talk

```bash
cd demo
./demo.sh prepare     # day before, on good Wi-Fi: pulls and builds everything
./demo.sh up          # ~3 min first time: Portus bootstrap, 1.0.0 of all services
```

`up` prints the URLs and the Portus login (user `duod`, generated password).
Do a full rehearsal, then reset to a clean 1.0.0 state about 30 minutes before you
go on stage:

```bash
./demo.sh reset && ./demo.sh up
```

**Screen layout**
- **Left (projector):** browser with three tabs: `dashboard.localhost`, `shop.localhost`, Portus.
- **Right:** two terminals, one running `./demo.sh watch` and one for commands.
- **Optional:** a third terminal with `./demo.sh traffic shop` (a request every 200 ms, printed in the color that answered).

## Run-book (~15 minutes)

### 0. Setting the scene (2 min)
- **Portus** → *Namespaces → apps*: three repositories, each tagged `1.0.0`. "This
  is our private registry, with users, teams and token auth."
- **Terminal:** `cat .state/gitops/apps/shop/compose.yaml`. "Each service is a
  folder in Git, and the image tag is the release."
- **Dashboard:** three services, all *Healthy*, all served by **blue**. The
  traffic strips keep filling with blue.

### 1. A release with a manual go (4 min)
```bash
./demo.sh release shop 1.1.0
```
1. Narrate the output: image built, **pushed to Portus** (the new tag appears in
   the Portus UI), tag bumped in Git, push, webhook delivered.
2. **Dashboard:** the green slot goes *preview · analysing*, then *preview · healthy*,
   and the card becomes **PAUSED**. The traffic strip stays blue.
3. Open **http://preview.shop.localhost**: 1.1.0 on green, with a new accent
   color. **http://shop.localhost** still shows 1.0.0 on blue. "Customers don't
   see the new version yet. We could smoke-test it here."
4. Optionally show the **Traefik dashboard**: the router `duod-shop-web` points
   at `duod-shop-web-blue`.
5. Hit the **Promote** button, or run `./demo.sh promote shop`. The shop tab turns
   green with 1.1.0 **without a reload**. The strip switches from blue to green
   with **0 failed**.

### 2. Rollback in seconds (1 min)
```bash
./demo.sh rollback shop
```
Blue is still around (`scaleDownDelay`), so traffic is back on 1.0.0 in about a
second, with no rebuild and no Git change. duod marks 1.1.0 as failed, so it
won't redeploy it by itself. Show `./demo.sh history shop` as the audit trail.

### 3. A broken release never reaches users (3 min)
```bash
./demo.sh release api 1.1.0 --broken
```
api promotes automatically, but this build fails its `/health`. **Dashboard:**
the green preview analyses, then the card turns **DEGRADED** with the reason in
plain text (`status 503`). The traffic strip **stays blue with 0 failed**. "Auto
promotion only happens after the analysis passes." Then ship the fix:
```bash
./demo.sh release api 1.1.1
```
It is analysed and promoted on its own. Both replicas now serve 1.1.1, which
you can see in the hostname line on the card (`api-green-api-1/2`).

### 4. Independent services, in parallel (3 min)
```bash
./demo.sh release-all 2.0.0
```
One commit touches all three folders. **Dashboard:** two services start right
away, and the third shows *queued: waiting for a free rollout slot*
(`maxConcurrentRollouts: 2`). api and docs promote themselves, and shop pauses
for your go. Each one moves at its own pace, and a failure in one wouldn't touch
the others. Finish with **Promote** on shop.

### 5. Wrap-up (1 min)
- **Portus:** every tag that was ever released.
- **Git log** (`git -C .state/gitops log --oneline`): the full release history.
- `./demo.sh watch`: everything *Synced* and *Healthy*.

## Commands

| Command | |
|---|---|
| `./demo.sh up` / `down` / `reset` | start or resume / stop / remove everything (asks first) |
| `./demo.sh release <app> <ver> [--broken]` | build, push to Portus and release through Git |
| `./demo.sh release-all <ver>` | one commit releasing all services |
| `./demo.sh promote\|abort\|rollback\|retry <app>` | duod actions (also on the dashboard) |
| `./demo.sh watch` / `history <app>` / `get <app>` | state in the terminal |
| `./demo.sh traffic <app>` | request loop that prints the answering color and failures |
| `./demo.sh open` / `urls` / `hosts` / `logs [service]` | helpers |
| `./demo.sh prepare` | pre-pull and pre-build for an offline venue |

## Troubleshooting

| Symptom | Fix |
|---|---|
| Port 80 is taken | `./demo.sh reset`, then `DEMO_HTTP_PORT=8088 ./demo.sh up`. URLs become `shop.localhost:8088`. |
| `waiting for Portus` takes ages | First start runs DB migrations. It's slower on Apple Silicon (emulated). Check `./demo.sh logs portus`. |
| `push … denied` / `unauthorized` | `./demo.sh up` again. It logs in again and refreshes duod's credentials. |
| A service stays *Degraded* after a fix | It has a failed revision. Push a new version or run `./demo.sh retry <app>`. |
| Browser can't open `shop.localhost` | Use Chrome or Firefox, or add `./demo.sh hosts` to `/etc/hosts`. |
| Nothing happens after `release` | The hook prints whether the webhook arrived. duod also polls every 15 s. Check `./demo.sh logs duod`. |

## How the pieces fit

- **Token auth:** for a push or pull, the registry answers `401` and points to
  Portus (`http://localhost:3000/v2/token`). Portus checks the user's rights on
  the namespace `apps` and signs a JWT with `.state/secrets/portus.key`. The
  registry verifies it with `portus.crt`.
- **Two names for one registry:** Portus reaches the registry as `registry:5000`
  (inside Docker), while clients use `localhost:5050`. Portus is set up with
  that hostname and *external hostname* through its REST API (`demo.sh`
  bootstraps the admin user, registry, team and namespace). `localhost` is an
  insecure registry by default, so no Docker daemon settings are needed.
- **Portus background worker** runs in Portus' network namespace, so the token
  realm `localhost:3000` points to Portus from there too.
- **duod** runs as a container with the Docker socket. It pulls with credentials
  written into its own Docker config. It writes Traefik's dynamic files into a
  shared volume and probes the slots through Traefik's private `duod` entrypoint.
- **Push hook:** the bare repo's `post-receive` hook calls
  `POST /api/v1/webhook`. Only services whose folder changed roll out.

## Files

```
demo/
  demo.sh                 setup, talk commands, cleanup
  compose.yaml            Portus, MariaDB, registry, Traefik, duod, dashboard
  duod/duod.yaml          duod config: shop (manual), api (auto, 2 replicas), docs (auto)
  apps/demo-app/          the demo service (Go): web page + JSON, version/accent/health baked in
  gitops-seed/            initial content of the GitOps repo (one folder per service)
  dashboard/              live dashboard (static HTML, served by nginx behind Traefik)
  .state/                 generated: secrets, Portus key, Git repo, tokens (git-ignored)
```
