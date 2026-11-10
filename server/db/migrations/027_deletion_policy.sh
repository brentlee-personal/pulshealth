#!/usr/bin/env bash
# Only the operator may classify pre-existing invited users as personal.
set -euo pipefail
if [[ -n "${PULS_USER_ID:-}" ]]; then
  echo '027_deletion_policy: PULS_USER_ID is retired; stored legacy protection is retained' >&2
  exit 1
fi
personal_users="${WEB_PERSONAL_USERS:-}"
uuid_pattern='^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
IFS=',' read -r -a requested_users <<< "$personal_users"
for personal_user in "${requested_users[@]}"; do
  if [[ ! "$personal_user" =~ $uuid_pattern ]]; then
    echo '027_deletion_policy: WEB_PERSONAL_USERS must be comma-separated UUIDs' >&2
    exit 1
  fi
done
psql -X -q -v ON_ERROR_STOP=1 -v personal_users="$personal_users" \
  --username "${POSTGRES_USER:-postgres}" --dbname "${POSTGRES_DB:-postgres}" <<'EOSQL'
BEGIN;
DELETE FROM auth.personal_users;
INSERT INTO auth.personal_users (user_id)
  SELECT u.id FROM public.users u
  WHERE u.id::text = ANY (string_to_array(lower(:'personal_users'), ','))
    AND NOT EXISTS (SELECT 1 FROM auth.deletion_policy WHERE default_user_id = u.id)
    AND NOT EXISTS (SELECT 1 FROM auth.device_pairing_policy WHERE demo_user_id = u.id)
    AND NOT EXISTS (SELECT 1 FROM auth.accounts WHERE user_id = u.id AND is_admin);
COMMIT;
EOSQL
