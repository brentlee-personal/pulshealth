#!/usr/bin/env bash
set -euo pipefail

for name in POSTGRES_PASSWORD GRAFANA_DB_PASSWORD API_DB_PASSWORD INGEST_DB_PASSWORD WEB_DB_PASSWORD; do
  file_name="${name}_FILE"
  path="${!file_name:-}"
  if [[ -n "${!name:-}" || ! "$path" = /* || ! -f "$path" || -L "$path" ]]; then
    echo "migrate: $file_name must name a protected file; plaintext is not accepted" >&2
    exit 1
  fi
  mode=$(stat -c '%a' "$path")
  size=$(stat -c '%s' "$path")
  if (( (8#$mode & 077) != 0 || (8#$mode & 0400) == 0 || size < 64 || size > 65 )) || [[ ! -r "$path" ]]; then
    echo "migrate: $file_name must be owner-readable only" >&2
    exit 1
  fi
  mapfile -t lines < "$path"
  if [[ ${#lines[@]} != 1 || ! "${lines[0]}" =~ ^[a-f0-9]{64}$ ]]; then
    echo "migrate: $file_name is not a generated family-stack credential" >&2
    exit 1
  fi
  printf -v "$name" '%s' "${lines[0]}"
  export "$name"
done
export PGPASSWORD="$POSTGRES_PASSWORD"
exec bash /puls/migrate.sh "$@"
