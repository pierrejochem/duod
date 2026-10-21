#!/usr/bin/env bash
# duod live demo - one script for setup, the talk itself and cleanup.
#
#   ./demo.sh up                      start everything, push v1.0.0 of all apps
#   ./demo.sh release shop 1.1.0      build + push to Portus, bump the tag in Git
#   ./demo.sh release api 1.1.0 --broken     a release whose health check fails
#   ./demo.sh release-all 2.0.0       one commit, three services, parallel rollouts
#   ./demo.sh promote|abort|rollback|retry|history|get <app>
#   ./demo.sh watch | traffic <app> | open | logs | urls
#   ./demo.sh prepare                 pre-pull/pre-build (do this before the talk)
#   ./demo.sh down | reset
set -euo pipefail

cd "$(dirname "$0")"
DEMO=$(pwd)
STATE="$DEMO/.state"
REG=localhost:5050
NS=apps
APPS=(shop api docs)
APPS_USAGE=$(IFS='|'; echo "${APPS[*]}")
PORTUS=http://localhost:3000
DUOD_API=http://127.0.0.1:9800

c_blue=$'\e[38;5;33m'; c_green=$'\e[38;5;42m'; c_red=$'\e[38;5;203m'; c_dim=$'\e[2m'; c_b=$'\e[1m'; c_0=$'\e[0m'
say()  { printf '%s▸%s %s\n' "$c_blue" "$c_0" "$*"; }
ok()   { printf '%s✓%s %s\n' "$c_green" "$c_0" "$*"; }
die()  { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_0" >&2; exit 1; }

compose() { docker compose -f "$DEMO/compose.yaml" --env-file "$STATE/.env" "$@"; }
envval()  { sed -n "s/^$1=//p" "$STATE/.env"; }
duodctl() { compose exec -T duod duod "$@"; }

need() { for b in "$@"; do command -v "$b" >/dev/null || die "missing dependency: $b"; done; }

is_app() { local a; for a in "${APPS[@]}"; do [[ $a == "$1" ]] && return 0; done; return 1; }

http_port() { local p; p=$(envval DEMO_HTTP_PORT); echo "${p:-80}"; }
app_url()   { local p; p=$(http_port); [[ $p == 80 ]] && echo "http://$1.localhost" || echo "http://$1.localhost:$p"; }

# Version -> accent color, so every release looks different on stage:
# consecutive versions (1.0 -> 1.1 -> 2.0) always get different colors.
# (Blue and green are reserved for the slots.)
accent_for() {
  local palette=("#FF8A3D" "#A78BFA" "#F472B6" "#FACC15" "#22D3EE" "#FB7185" "#F5F5F4" "#C084FC")
  local major minor
  IFS=. read -r major minor _ <<<"$1"
  [[ $major =~ ^[0-9]+$ && ${minor:-0} =~ ^[0-9]+$ ]] || { major=$(printf '%s' "$1" | cksum | cut -d' ' -f1); minor=0; }
  echo "${palette[$(( (major * 3 + ${minor:-0}) % ${#palette[@]} ))]}"
}

# ------------------------------------------------------------------ init ----
init() {
  mkdir -p "$STATE/secrets"
  if [[ ! -f $STATE/.env ]]; then
    say "generating secrets"
    {
      echo "SECRET_KEY_BASE=$(openssl rand -hex 64)"
      echo "DATABASE_PASSWORD=$(openssl rand -hex 12)"
      echo "PORTUS_PASSWORD=$(openssl rand -hex 12)"
      echo "DUOD_TOKEN=$(openssl rand -hex 24)"
      echo "DEMO_USER=duod"
      echo "DEMO_PASSWORD=$(openssl rand -hex 8)"
      echo "DEMO_HTTP_PORT=${DEMO_HTTP_PORT:-80}"
    } > "$STATE/.env"
  fi
  if [[ ! -f $STATE/secrets/portus.key ]]; then
    say "creating the key Portus signs registry tokens with"
    openssl req -x509 -newkey rsa:4096 -nodes -sha256 -days 825 -subj "/CN=localhost" \
      -keyout "$STATE/secrets/portus.key" -out "$STATE/secrets/portus.crt" 2>/dev/null
    chmod 644 "$STATE/secrets/portus.key" "$STATE/secrets/portus.crt"
  fi
  if [[ ! -d $STATE/gitops.git ]]; then
    say "creating the GitOps repository (bare repo = the demo's Git server)"
    git init -q --bare -b main "$STATE/gitops.git"
    cat > "$STATE/gitops.git/hooks/post-receive" <<EOF
#!/bin/sh
# Like a GitHub/Gitea push webhook: tell duod to fetch now (all apps;
# only the services whose folder changed roll out).
curl -fsS -m 3 -X POST -H "Authorization: Bearer $(envval DUOD_TOKEN)" $DUOD_API/api/v1/webhook >/dev/null 2>&1 \
  && echo "duod: webhook delivered" || echo "duod: webhook not delivered (duod polls every 15s anyway)"
EOF
    chmod +x "$STATE/gitops.git/hooks/post-receive"
    git clone -q "$STATE/gitops.git" "$STATE/gitops" 2>/dev/null
    cp -R "$DEMO/gitops-seed/." "$STATE/gitops/"
    git -C "$STATE/gitops" add -A
    git -C "$STATE/gitops" -c user.name="duod demo" -c user.email=demo@duod.local commit -qm "Initial: shop, api, docs at 1.0.0"
    git -C "$STATE/gitops" push -q origin main >/dev/null 2>&1
  fi
  printf 'window.DUOD = {token: "%s", httpPort: "%s"};\n' "$(envval DUOD_TOKEN)" "$(http_port)" > "$STATE/dashboard-config.js"
}

# -------------------------------------------------------------- portus -----
wait_for() { # url, what, seconds
  local i=0
  printf '%s▸%s waiting for %s ' "$c_blue" "$c_0" "$2"
  until curl -fsS -m 2 -o /dev/null "$1" 2>/dev/null; do
    i=$((i + 2)); [[ $i -gt $3 ]] && { echo; die "$2 did not come up - check: ./demo.sh logs"; }
    printf '.'; sleep 2
  done
  echo " up"
}

portus_api() { # method path json
  curl -sS -X "$1" "$PORTUS/api/v1/$2" -H "Content-Type: application/json" \
    -H "Portus-Auth: $(envval DEMO_USER):$(cat "$STATE/portus-token")" -d "$3"
}

bootstrap_portus() {
  [[ -s $STATE/portus-token ]] && return 0
  say "bootstrapping Portus: admin user, registry, namespace '$NS'"
  local resp token
  resp=$(curl -sS -X POST "$PORTUS/api/v1/users/bootstrap" -H "Content-Type: application/json" \
    -d "{\"user\":{\"username\":\"$(envval DEMO_USER)\",\"email\":\"demo@duod.local\",\"password\":\"$(envval DEMO_PASSWORD)\"}}")
  token=$(printf '%s' "$resp" | sed -n 's/.*"plain_token":"\([^"]*\)".*/\1/p')
  [[ -n $token ]] || die "Portus bootstrap failed: $resp"
  printf '%s' "$token" > "$STATE/portus-token"
  # hostname: how Portus reaches the registry; external_hostname: what Docker clients use
  portus_api POST registries "{\"registry\":{\"name\":\"duod-demo\",\"hostname\":\"registry:5000\",\"external_hostname\":\"$REG\",\"use_ssl\":false}}" >/dev/null
  portus_api POST teams "{\"name\":\"$NS\",\"description\":\"duod demo services\"}" >/dev/null
  portus_api POST namespaces "{\"name\":\"$NS\",\"team\":\"$NS\",\"description\":\"Images deployed by duod\"}" >/dev/null
  ok "Portus ready - user $(envval DEMO_USER)"
}

registry_login() {
  envval DEMO_PASSWORD | docker login "$REG" -u "$(envval DEMO_USER)" --password-stdin >/dev/null
  ok "host docker logged in to $REG"
}

# duod pulls through the host daemon with the credentials of the docker CLI
# inside its container. Write them into its volume BEFORE duod starts, so the
# very first rollout can already pull from Portus.
duod_credentials() {
  local auth
  auth=$(printf '%s:%s' "$(envval DEMO_USER)" "$(envval DEMO_PASSWORD)" | base64 | tr -d '\n')
  printf '{"auths":{"%s":{"auth":"%s"}}}\n' "$REG" "$auth" |
    compose run --rm -T --no-deps --entrypoint sh duod -c 'mkdir -p /root/.docker && cat > /root/.docker/config.json'
}

# ------------------------------------------------------------- images ------
build_push() { # app version healthy
  local app=$1 ver=$2 healthy=${3:-true} img="$REG/$NS/$1:$2"
  say "building $img ${c_dim}(accent $(accent_for "$ver"), healthy=$healthy)${c_0}"
  docker build -q --build-arg "VERSION=$ver" --build-arg "ACCENT=$(accent_for "$ver")" --build-arg "HEALTHY=$healthy" \
    -t "$img" "$DEMO/apps/demo-app" >/dev/null
  docker push -q "$img" >/dev/null
  ok "pushed $img to Portus"
}

set_tag() { # app version
  sed -i.bak -E "s#(image: $REG/$NS/$1:).*#\1$2#" "$STATE/gitops/apps/$1/compose.yaml"
  rm -f "$STATE/gitops/apps/$1/compose.yaml.bak"
}

git_push() { # message
  git -C "$STATE/gitops" add -A
  git -C "$STATE/gitops" -c user.name="duod demo" -c user.email=demo@duod.local commit -qm "$1"
  say "git push: $1"
  git -C "$STATE/gitops" push -q origin main 2>&1 | sed "s/^/  ${c_dim}/;s/[[:space:]]*$/${c_0}/"
}

# ------------------------------------------------------------- commands ----
cmd_up() {
  need docker git openssl curl cksum
  docker compose version >/dev/null 2>&1 || die "docker compose v2 is required"
  init
  say "starting registry stack (Portus, MariaDB, registry), Traefik and dashboard"
  compose up -d db portus background registry traefik dashboard
  wait_for "$PORTUS/api/v1/_ping" "Portus (first start runs migrations, ~1-2 min)" 300
  bootstrap_portus
  registry_login
  if [[ ! -f $STATE/seeded ]]; then
    local a; for a in "${APPS[@]}"; do build_push "$a" 1.0.0; done
    touch "$STATE/seeded"
  fi
  say "building and starting duod"
  compose build duod
  duod_credentials
  compose up -d duod
  wait_for "$DUOD_API/api/v1/apps" "duod" 60
  curl -fsS -m 3 -X POST -H "Authorization: Bearer $(envval DUOD_TOKEN)" "$DUOD_API/api/v1/webhook" >/dev/null || true
  say "waiting until all services are Healthy"
  local i=0
  until [[ $(curl -fsS "$DUOD_API/api/v1/apps" | grep -o '"phase": "Healthy"' | wc -l | tr -d ' ') -eq ${#APPS[@]} ]]; do
    i=$((i + 2)); [[ $i -gt 240 ]] && { duodctl list; die "services not healthy - ./demo.sh logs"; }
    sleep 2
  done
  duodctl list
  cmd_urls
}

cmd_urls() {
  cat <<EOF

${c_b}Live demo is ready${c_0}
  Dashboard  $(app_url dashboard)          ${c_dim}(put this on the projector)${c_0}
  Services   $(app_url shop)   $(app_url api)/api/info   $(app_url docs)
  Preview    $(app_url preview.shop)  ${c_dim}(only while a shop rollout is paused)${c_0}
  Portus     $PORTUS   user ${c_b}$(envval DEMO_USER)${c_0} / ${c_b}$(envval DEMO_PASSWORD)${c_0}
  Traefik    http://localhost:8081/dashboard/
  duod CLI   ./demo.sh watch | promote shop | rollback shop | history shop

  ${c_dim}*.localhost resolves in Chrome, Edge and Firefox. Safari: ./demo.sh hosts${c_0}
EOF
}

cmd_release() { # app version [--broken]
  local app=${1:-} ver=${2:-} healthy=true
  is_app "$app" && [[ -n $ver ]] || die "usage: ./demo.sh release <$APPS_USAGE> <version> [--broken]"
  [[ ${3:-} == --broken ]] && healthy=false
  build_push "$app" "$ver" "$healthy"
  set_tag "$app" "$ver"
  git_push "release $app $ver$([[ $healthy == false ]] && echo ' (broken build)')"
  ok "duod picks it up now - watch the dashboard"
}

cmd_release_all() { # version
  local ver=${1:-} a
  [[ -n $ver ]] || die "usage: ./demo.sh release-all <version>"
  for a in "${APPS[@]}"; do build_push "$a" "$ver"; done
  for a in "${APPS[@]}"; do set_tag "$a" "$ver"; done
  git_push "release ${APPS[*]} $ver"
  ok "one commit, ${#APPS[@]} independent rollouts (maxConcurrentRollouts=2: one waits in the queue)"
}

cmd_watch() {
  while true; do
    clear
    printf '%sduod%s  %s\n\n' "$c_b" "$c_0" "$(date +%T)"
    duodctl list || true
    sleep 1
  done
}

cmd_traffic() { # app
  local app=${1:-shop} n=0 fails=0 line slot ver
  is_app "$app" || die "usage: ./demo.sh traffic <$APPS_USAGE>"
  say "requests to $app through Traefik (Ctrl-C to stop)"
  while true; do
    n=$((n + 1))
    if line=$(curl -fsS -m 2 -H "Host: $app.localhost" "http://127.0.0.1:$(http_port)/api/info" 2>/dev/null); then
      slot=$(printf '%s' "$line" | sed -n 's/.*"slot":"\([a-z]*\)".*/\1/p')
      ver=$(printf '%s' "$line" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
      [[ $slot == blue ]] && c=$c_blue || c=$c_green
      printf '%s%5d  %-5s  %s%s   failed so far: %d\n' "$c" "$n" "$slot" "$ver" "$c_0" "$fails"
    else
      fails=$((fails + 1))
      printf '%s%5d  FAILED%s   failed so far: %d\n' "$c_red" "$n" "$c_0" "$fails"
    fi
    sleep 0.2
  done
}

cmd_open() {
  local opener=xdg-open; command -v open >/dev/null && opener=open
  "$opener" "$(app_url dashboard)"; "$opener" "$(app_url shop)"; "$opener" "$PORTUS"
}

cmd_hosts() {
  echo "# add to /etc/hosts (only needed for Safari or tools that don't resolve *.localhost)"
  echo "127.0.0.1 dashboard.localhost shop.localhost preview.shop.localhost api.localhost docs.localhost preview.docs.localhost"
}

cmd_prepare() {
  need docker
  init
  say "pulling infrastructure images"
  compose pull db portus background registry traefik dashboard
  say "building duod and the demo app (warms the build cache)"
  compose build duod
  docker build -q "$DEMO/apps/demo-app" >/dev/null
  ok "ready for an offline talk"
}

stop_slots() { # verb
  local a c
  for a in "${APPS[@]}"; do for c in blue green; do
    docker compose -p "$a-$c" "$@" >/dev/null 2>&1 || true
  done; done
}

cmd_down()  { stop_slots stop; compose stop; ok "stopped (./demo.sh up resumes)"; }
cmd_reset() {
  read -r -p "Remove ALL demo containers, volumes, images in the registry and $STATE? [y/N] " yn
  [[ $yn == [yY] ]] || exit 0
  stop_slots down --remove-orphans
  compose down -v --remove-orphans || true
  docker logout "$REG" >/dev/null 2>&1 || true
  rm -rf "$STATE"
  ok "reset done"
}

case "${1:-help}" in
  up)            cmd_up ;;
  release)       shift; cmd_release "$@" ;;
  release-all)   shift; cmd_release_all "$@" ;;
  promote|abort|rollback|retry|history|get)
                 is_app "${2:-}" || die "usage: ./demo.sh $1 <$APPS_USAGE>"; duodctl "$1" "$2" ;;
  status|list)   duodctl list ;;
  watch)         cmd_watch ;;
  traffic)       shift; cmd_traffic "$@" ;;
  open)          cmd_open ;;
  urls)          cmd_urls ;;
  hosts)         cmd_hosts ;;
  logs)          shift; compose logs -f --tail=50 "${@:-duod}" ;;
  prepare)       cmd_prepare ;;
  down)          cmd_down ;;
  reset)         cmd_reset ;;
  *)             sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//' ;;
esac
