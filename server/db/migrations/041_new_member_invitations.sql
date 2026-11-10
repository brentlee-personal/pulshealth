-- Invitations create identities; they cannot claim a pre-existing user's data.
-- Old unaccepted links may carry an operator-selected member and must expire.
UPDATE auth.invites SET expires_at = now()
 WHERE accepted_at IS NULL AND expires_at > now();

CREATE FUNCTION auth.invite_new_member(p_token_hash bytea, p_email text, p_admin boolean, p_hours integer)
RETURNS TABLE (user_id uuid, expires_at timestamptz)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
  new_user uuid := gen_random_uuid();
  expiry timestamptz;
BEGIN
  IF p_token_hash IS NULL OR octet_length(p_token_hash) <> 32
     OR p_email IS NULL OR p_email <> lower(btrim(p_email))
     OR char_length(p_email) NOT BETWEEN 3 AND 254
     OR p_email !~ '^[^[:space:]@]+@[^[:space:]@]+$'
     OR p_admin IS NULL OR p_hours IS NULL OR p_hours NOT BETWEEN 1 AND 336 THEN
    RAISE EXCEPTION 'invalid new-member invitation' USING ERRCODE = '22023';
  END IF;
  IF EXISTS (SELECT 1 FROM auth.accounts a WHERE a.email = p_email) THEN
    RAISE EXCEPTION 'an account already uses this address; owner verification is required'
      USING ERRCODE = '23505';
  END IF;
  expiry := now() + make_interval(hours => p_hours);
  INSERT INTO public.users (id, time_zone_auto_initialize) VALUES (new_user, true);
  INSERT INTO auth.self_service_users (user_id) VALUES (new_user);
  INSERT INTO auth.invites (token_hash, user_id, email, is_admin, expires_at)
    VALUES (p_token_hash, new_user, p_email, p_admin, expiry);
  RETURN QUERY SELECT new_user, expiry;
END
$$;
REVOKE ALL ON FUNCTION auth.invite_new_member(bytea, text, boolean, integer) FROM PUBLIC;
