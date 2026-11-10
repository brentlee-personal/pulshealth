#!/usr/bin/env bash
set -euo pipefail
umask 077

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
settings="$root/server/.env"
url=""
zone="UTC"
configure_only=false

die() { printf 'bootstrap: %s\n' "$*" >&2; exit 1; }
usage() {
  cat <<'EOF'
usage: scripts/bootstrap.sh --url https://<private-viewer-origin> [--time-zone UTC] [--configure-only]
Creates non-secret settings only when absent, builds this checkout and starts
the internal personal-account stack. Existing settings and keys are kept.
--configure-only writes settings without starting containers.
--build is accepted; this branch always builds its own images.
Shared pairing, default members, --lan and public tunnels are retired.
Invite each new member with make web-invite. Pair phones in that account.
EOF
}
while [[ $# -gt 0 ]]; do
  case "$1" in
    --url) [[ $# -ge 2 ]] || die '--url needs an HTTPS origin'; url="${2%/}"; shift 2 ;;
    --time-zone) [[ $# -ge 2 ]] || die '--time-zone needs an IANA zone'; zone="$2"; shift 2 ;;
    --configure-only) configure_only=true; shift ;;
    --build) shift ;;
    -h|--help) usage; exit 0 ;;
    *) die 'unknown or retired option; see --help' ;;
  esac
done

command -v docker >/dev/null 2>&1 || die 'Docker with Compose is required'
if [[ -e "$settings" ]]; then
  [[ -f "$settings" && ! -L "$settings" ]] || die 'server/.env must be a regular settings file'
  [[ "$configure_only" == false ]] || die 'existing settings were preserved; no validation or startup was performed'
  [[ -z "$url" && "$zone" == UTC ]] || die 'existing settings are preserved; edit their non-secret URL or time zone explicitly'
else
  [[ "$url" =~ ^https://[a-zA-Z0-9.:-]+$ ]] || die '--url must be a private HTTPS origin without a path'
  [[ "$zone" =~ ^[a-zA-Z0-9_+/-]+$ ]] || die 'invalid time-zone spelling'
  [[ "$zone" != *..* ]] || die 'invalid time-zone spelling'
  # Noclobber also protects a file created by another bootstrap process.
  (set -o noclobber; printf 'WEB_PUBLIC_URL=%s\nPULS_PUBLIC_URL=%s\nPULS_TIME_ZONE=%s\nTRUST_PROXY_HEADERS=false\n' "$url" "$url" "$zone" > "$settings") \
    || die 'settings appeared during setup; nothing was replaced'
fi
if [[ "$configure_only" == true ]]; then
  printf 'Non-secret settings are ready. No containers were started.\n'
  exit 0
fi

export DEPLOY_COMMIT
DEPLOY_COMMIT="$(git -C "$root" rev-parse HEAD)"
compose=(docker compose --project-directory "$root/server" -f "$root/server/docker-compose.yml" -f "$root/server/compose.build.yml")
"${compose[@]}" config --quiet
"${compose[@]}" up -d --build --wait --wait-timeout 180 db migrate ingest api web
printf 'Internal personal-account services started. No member or phone was created.\n'
printf 'Private HTTPS ingress and backup acceptance remain separate gates.\n'
