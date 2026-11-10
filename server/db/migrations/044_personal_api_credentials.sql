CREATE FUNCTION public.puls_utc_nanoseconds(p text) RETURNS numeric
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
  IF p IS NULL OR p !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$'
     OR to_char(substring(p, 1, 19)::timestamp, 'YYYY-MM-DD"T"HH24:MI:SS') <> substring(p, 1, 19) THEN RETURN NULL; END IF;
  RETURN extract(epoch FROM (substring(p, 1, 19) || 'Z')::timestamptz) * 1000000000
    + rpad(coalesce(substring(p FROM '\.([0-9]+)Z$'), ''), 9, '0')::numeric;
EXCEPTION WHEN OTHERS THEN RETURN NULL;
END
$$;

CREATE FUNCTION public.puls_valid_permissions(p jsonb, allowed_operations text[]) RETURNS boolean
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
  ops jsonb;
  types jsonb;
  bounds jsonb;
BEGIN
  IF allowed_operations IS NULL OR cardinality(allowed_operations) = 0 OR array_position(allowed_operations, NULL) IS NOT NULL
     OR jsonb_typeof(p) IS DISTINCT FROM 'object'
     OR (SELECT array_agg(k ORDER BY k) FROM jsonb_object_keys(p) k)
        IS DISTINCT FROM ARRAY['operations', 'record_types', 'time_range']::text[] THEN RETURN false; END IF;
  ops := p->'operations'; types := p->'record_types'; bounds := p->'time_range';
  IF jsonb_typeof(ops) IS DISTINCT FROM 'array' OR jsonb_typeof(types) IS DISTINCT FROM 'array'
     OR jsonb_typeof(bounds) IS DISTINCT FROM 'object' THEN RETURN false; END IF;
  IF jsonb_array_length(ops) < 1 OR jsonb_array_length(types) < 1
     OR EXISTS (SELECT 1 FROM jsonb_array_elements(ops) op
                 WHERE jsonb_typeof(op) <> 'string' OR NOT ((op #>> '{}') = ANY(allowed_operations)))
     OR (SELECT count(DISTINCT op) FROM jsonb_array_elements(ops) op) <> jsonb_array_length(ops)
     OR EXISTS (SELECT 1 FROM jsonb_array_elements(types) t
                 WHERE jsonb_typeof(t) <> 'string' OR char_length(t #>> '{}') NOT BETWEEN 1 AND 256)
     OR (SELECT count(DISTINCT t) FROM jsonb_array_elements(types) t) <> jsonb_array_length(types)
     OR (SELECT array_agg(k ORDER BY k) FROM jsonb_object_keys(bounds) k)
        IS DISTINCT FROM ARRAY['from', 'until']::text[]
     OR jsonb_typeof(bounds->'from') IS DISTINCT FROM 'string'
     OR jsonb_typeof(bounds->'until') IS DISTINCT FROM 'string' THEN RETURN false; END IF;
  RETURN coalesce(public.puls_utc_nanoseconds(bounds->>'from') < public.puls_utc_nanoseconds(bounds->>'until'), false);
EXCEPTION WHEN OTHERS THEN RETURN false;
END
$$;

CREATE OR REPLACE FUNCTION public.puls_valid_read_permissions(p jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
AS $$ SELECT public.puls_valid_permissions(p, ARRAY['metrics:read', 'samples:read']) $$;

CREATE FUNCTION public.puls_api_permissions_within(child jsonb, parent jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
AS $$
  SELECT public.puls_valid_permissions(child, ARRAY['metrics:read', 'samples:read', 'data:export'])
     AND public.puls_valid_read_permissions(parent)
     AND parent->'operations' @> child->'operations'
     AND parent->'record_types' @> child->'record_types'
     AND public.puls_utc_nanoseconds(child->'time_range'->>'from') >= public.puls_utc_nanoseconds(parent->'time_range'->>'from')
     AND public.puls_utc_nanoseconds(child->'time_range'->>'until') <= public.puls_utc_nanoseconds(parent->'time_range'->>'until')
$$;

ALTER TABLE auth.sessions ADD CONSTRAINT sessions_account_identity UNIQUE (account_id, id);
ALTER TABLE auth.oauth_grants ADD CONSTRAINT oauth_grants_account_identity UNIQUE (account_id, id);

CREATE TABLE auth.api_credentials (
  id uuid PRIMARY KEY,
  token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  account_id uuid NOT NULL REFERENCES auth.accounts(id) ON DELETE CASCADE,
  session_id bytea,
  grant_id uuid,
  parent_resource text,
  permissions jsonb NOT NULL CHECK (public.puls_valid_permissions(permissions, ARRAY['metrics:read', 'samples:read', 'data:export'])),
  authorization_version bigint NOT NULL CHECK (authorization_version BETWEEN 1 AND 9007199254740991),
  assertion text NOT NULL CHECK (octet_length(assertion) <= 16384 AND assertion ~ '^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$'),
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  CHECK ((session_id IS NULL) <> (grant_id IS NULL)),
  CHECK ((grant_id IS NULL) = (parent_resource IS NULL)),
  CHECK (expires_at > created_at AND expires_at <= created_at + CASE WHEN grant_id IS NULL THEN interval '30 minutes' ELSE interval '60 seconds' END),
  FOREIGN KEY (account_id, session_id) REFERENCES auth.sessions(account_id, id) ON DELETE CASCADE,
  FOREIGN KEY (account_id, grant_id) REFERENCES auth.oauth_grants(account_id, id) ON DELETE CASCADE
);
CREATE INDEX api_credentials_expiry ON auth.api_credentials(expires_at);

CREATE FUNCTION auth.resolve_api_credential(p_token_hash bytea)
RETURNS TABLE (assertion text, principal jsonb, grant_id uuid, client_id text, expires_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
  SELECT c.assertion,
         jsonb_build_object('member_id', a.user_id, 'credential_id', c.id,
           'credential_kind', CASE WHEN c.grant_id IS NULL THEN 'session' ELSE 'exchange' END,
           'allowed_operations', c.permissions->'operations', 'allowed_types', c.permissions->'record_types',
           'allowed_time_range', c.permissions->'time_range', 'authorization_version', c.authorization_version),
         c.grant_id, CASE WHEN c.grant_id IS NULL THEN 'family-health-web' ELSE g.client_id END, c.expires_at
    FROM auth.api_credentials c JOIN auth.accounts a ON a.id = c.account_id
    LEFT JOIN auth.sessions s ON s.account_id = c.account_id AND s.id = c.session_id
    LEFT JOIN auth.oauth_grants g ON g.account_id = c.account_id AND g.id = c.grant_id
   WHERE c.token_hash = p_token_hash AND c.created_at <= statement_timestamp()
     AND c.expires_at > statement_timestamp() AND c.revoked_at IS NULL
     AND a.disabled_at IS NULL AND a.password_hash IS NOT NULL
     AND ((c.session_id IS NOT NULL AND c.authorization_version = 1
            AND s.expires_at > statement_timestamp() AND s.created_at > statement_timestamp() - interval '90 days')
       OR (c.grant_id IS NOT NULL AND g.revoked_at IS NULL AND g.refresh_expires_at > statement_timestamp()
            AND c.authorization_version = g.authorization_version AND c.parent_resource = g.resource
            AND public.puls_api_permissions_within(c.permissions, g.permissions)))
$$;
REVOKE ALL ON FUNCTION auth.resolve_api_credential(bytea) FROM PUBLIC;

UPDATE auth.oauth_grants SET revoked_at = now() WHERE revoked_at IS NULL AND resource IS NULL;

CREATE OR REPLACE PROCEDURE auth.prune_oauth(job_id integer, config jsonb)
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
  DELETE FROM auth.api_credentials WHERE expires_at < statement_timestamp() OR revoked_at IS NOT NULL;
  DELETE FROM auth.oauth_codes WHERE expires_at < now();
  DELETE FROM auth.oauth_grants WHERE revoked_at IS NOT NULL OR refresh_expires_at < now();
  DELETE FROM auth.oauth_clients c
   WHERE c.last_used_at < now() - interval '30 days'
     AND NOT EXISTS (SELECT 1 FROM auth.oauth_grants g
                      WHERE g.client_id = c.id AND g.revoked_at IS NULL AND g.refresh_expires_at > now());
END
$$;
REVOKE ALL ON PROCEDURE auth.prune_oauth(integer, jsonb) FROM PUBLIC;
