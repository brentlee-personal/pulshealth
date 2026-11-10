-- Legacy consents remain sealed until a person explicitly chooses a scope.
CREATE FUNCTION public.puls_valid_read_permissions(p jsonb) RETURNS boolean
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
  ops jsonb;
  types jsonb;
  bounds jsonb;
  first_at text;
  last_at text;
  first_ns numeric;
  last_ns numeric;
BEGIN
  IF jsonb_typeof(p) IS DISTINCT FROM 'object'
     OR (SELECT array_agg(k ORDER BY k) FROM jsonb_object_keys(p) k)
        IS DISTINCT FROM ARRAY['operations', 'record_types', 'time_range']::text[] THEN
    RETURN false;
  END IF;
  ops := p->'operations';
  types := p->'record_types';
  bounds := p->'time_range';
  IF jsonb_typeof(ops) IS DISTINCT FROM 'array' OR jsonb_typeof(types) IS DISTINCT FROM 'array'
     OR jsonb_typeof(bounds) IS DISTINCT FROM 'object' THEN RETURN false; END IF;
  IF jsonb_array_length(ops) < 1 OR jsonb_array_length(types) < 1
     OR EXISTS (SELECT 1 FROM jsonb_array_elements(ops) op WHERE op NOT IN ('"metrics:read"'::jsonb, '"samples:read"'::jsonb))
     OR (SELECT count(DISTINCT op) FROM jsonb_array_elements(ops) op) <> jsonb_array_length(ops)
     OR EXISTS (SELECT 1 FROM jsonb_array_elements(types) t
                 WHERE jsonb_typeof(t) <> 'string' OR char_length(t #>> '{}') NOT BETWEEN 1 AND 256)
     OR (SELECT count(DISTINCT t) FROM jsonb_array_elements(types) t) <> jsonb_array_length(types)
     OR (SELECT array_agg(k ORDER BY k) FROM jsonb_object_keys(bounds) k)
        IS DISTINCT FROM ARRAY['from', 'until']::text[] THEN RETURN false; END IF;
  first_at := bounds->>'from';
  last_at := bounds->>'until';
  IF jsonb_typeof(bounds->'from') IS DISTINCT FROM 'string' OR jsonb_typeof(bounds->'until') IS DISTINCT FROM 'string'
     OR first_at !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$'
     OR last_at !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$' THEN RETURN false; END IF;
  IF to_char(substring(first_at, 1, 19)::timestamp, 'YYYY-MM-DD"T"HH24:MI:SS') <> substring(first_at, 1, 19)
     OR to_char(substring(last_at, 1, 19)::timestamp, 'YYYY-MM-DD"T"HH24:MI:SS') <> substring(last_at, 1, 19) THEN RETURN false; END IF;
  first_ns := extract(epoch FROM (substring(first_at, 1, 19) || 'Z')::timestamptz) * 1000000000
    + rpad(coalesce(substring(first_at FROM '\.([0-9]+)Z$'), ''), 9, '0')::numeric;
  last_ns := extract(epoch FROM (substring(last_at, 1, 19) || 'Z')::timestamptz) * 1000000000
    + rpad(coalesce(substring(last_at FROM '\.([0-9]+)Z$'), ''), 9, '0')::numeric;
  RETURN first_ns < last_ns;
EXCEPTION WHEN OTHERS THEN RETURN false;
END
$$;

ALTER TABLE auth.oauth_codes ADD COLUMN permissions jsonb
  CHECK (permissions IS NULL OR public.puls_valid_read_permissions(permissions));
ALTER TABLE auth.oauth_grants ADD COLUMN permissions jsonb
  CHECK (permissions IS NULL OR public.puls_valid_read_permissions(permissions));
ALTER TABLE auth.oauth_grants ADD COLUMN authorization_version bigint NOT NULL DEFAULT 1
  CHECK (authorization_version >= 1);

UPDATE auth.oauth_grants SET revoked_at = now() WHERE revoked_at IS NULL;
DELETE FROM auth.oauth_codes WHERE used_at IS NULL;
