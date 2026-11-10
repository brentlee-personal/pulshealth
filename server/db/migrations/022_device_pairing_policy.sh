#!/usr/bin/env bash
# Preserve any legacy shared-member restriction; never create shared access.
set -euo pipefail

if [[ -n "${WEB_DEMO_USER:-}" ]]; then
  echo "022_device_pairing_policy: WEB_DEMO_USER is retired; remove this setting" >&2
  exit 1
fi
psql -X -q -v ON_ERROR_STOP=1 \
  --username "${POSTGRES_USER:-postgres}" --dbname "${POSTGRES_DB:-postgres}" <<'EOSQL'
INSERT INTO auth.device_pairing_policy (singleton, demo_user_id)
VALUES (true, NULL)
ON CONFLICT (singleton) DO NOTHING;
EOSQL
