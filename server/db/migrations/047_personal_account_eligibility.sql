-- Retired shared accounts stay sealed after their environment flag is removed.
CREATE FUNCTION auth.personal_account_allowed(p_account uuid)
RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
  SELECT EXISTS (
    SELECT 1 FROM auth.accounts a
     WHERE a.id = p_account AND a.disabled_at IS NULL AND a.password_hash IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM auth.device_pairing_policy p WHERE p.demo_user_id = a.user_id)
  )
$$;
REVOKE ALL ON FUNCTION auth.personal_account_allowed(uuid) FROM PUBLIC;

CREATE OR REPLACE FUNCTION auth.session_owner(p_session bytea)
RETURNS TABLE (account_id uuid, user_id uuid, is_admin boolean, self_service boolean)
LANGUAGE sql STABLE
SET search_path = pg_catalog, pg_temp
AS $$
  SELECT a.id, a.user_id, a.is_admin,
         EXISTS (SELECT 1 FROM auth.self_service_users ss WHERE ss.user_id = a.user_id)
    FROM auth.sessions s JOIN auth.accounts a ON a.id = s.account_id
   WHERE octet_length(p_session) = 32 AND s.id = p_session AND s.purpose = 'personal'
     AND s.expires_at > statement_timestamp()
     AND s.created_at > statement_timestamp() - interval '90 days'
     AND auth.personal_account_allowed(a.id)
$$;
REVOKE ALL ON FUNCTION auth.session_owner(bytea) FROM PUBLIC;

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
     AND auth.personal_account_allowed(a.id)
     AND ((c.session_id IS NOT NULL AND c.authorization_version = 1 AND s.purpose = 'personal'
            AND s.expires_at > statement_timestamp() AND s.created_at > statement_timestamp() - interval '90 days')
       OR (c.grant_id IS NOT NULL AND g.revoked_at IS NULL AND g.refresh_expires_at > statement_timestamp()
            AND c.authorization_version = g.authorization_version AND c.parent_resource = g.resource
            AND public.puls_api_permissions_within(c.permissions, g.permissions)))
$$;
REVOKE ALL ON FUNCTION auth.resolve_api_credential(bytea) FROM PUBLIC;
