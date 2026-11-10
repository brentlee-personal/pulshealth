-- Old rows cannot distinguish a personal sign-in from the retired shared demo.
ALTER TABLE auth.sessions ADD COLUMN purpose text NOT NULL DEFAULT 'legacy'
  CHECK (purpose IN ('legacy', 'personal'));

UPDATE auth.sessions SET expires_at = least(expires_at, statement_timestamp());
UPDATE auth.api_credentials SET revoked_at = coalesce(revoked_at, statement_timestamp());
UPDATE auth.oauth_grants SET revoked_at = coalesce(revoked_at, statement_timestamp());
UPDATE auth.oauth_codes SET expires_at = least(expires_at, statement_timestamp());

CREATE OR REPLACE FUNCTION auth.resolve_api_credential(p_token_hash bytea)
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
     AND ((c.session_id IS NOT NULL AND c.authorization_version = 1 AND s.purpose = 'personal'
            AND s.expires_at > statement_timestamp() AND s.created_at > statement_timestamp() - interval '90 days')
       OR (c.grant_id IS NOT NULL AND g.revoked_at IS NULL AND g.refresh_expires_at > statement_timestamp()
            AND c.authorization_version = g.authorization_version AND c.parent_resource = g.resource
            AND public.puls_api_permissions_within(c.permissions, g.permissions)))
$$;
REVOKE ALL ON FUNCTION auth.resolve_api_credential(bytea) FROM PUBLIC;
