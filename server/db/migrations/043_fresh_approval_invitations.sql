-- Approval and resend must not attach an invitation to existing health data.
UPDATE auth.invites SET expires_at = now()
 WHERE accepted_at IS NULL AND expires_at > now();

CREATE OR REPLACE FUNCTION auth.approve_signup(p_session bytea, p_request uuid)
RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
  caller record;
  req record;
  new_user uuid := gen_random_uuid();
BEGIN
  SELECT * INTO caller FROM auth.session_owner(p_session);
  IF NOT FOUND OR NOT caller.is_admin THEN
    RAISE EXCEPTION 'only an administrator may approve requests' USING ERRCODE = '42501';
  END IF;
  SELECT * INTO req FROM auth.signup_requests WHERE id = p_request FOR UPDATE;
  IF NOT FOUND OR req.status <> 'pending' THEN
    RAISE EXCEPTION 'no pending request' USING ERRCODE = 'P0002';
  END IF;
  IF EXISTS (SELECT 1 FROM auth.accounts WHERE email = req.email) THEN
    RAISE EXCEPTION 'an account already signs in with that address' USING ERRCODE = '23505';
  END IF;
  UPDATE auth.invites SET expires_at = now()
   WHERE email = req.email AND accepted_at IS NULL AND expires_at > now();
  INSERT INTO public.users (id, time_zone_auto_initialize) VALUES (new_user, true);
  INSERT INTO auth.self_service_users (user_id, request_id) VALUES (new_user, p_request);
  UPDATE auth.signup_requests
     SET status = 'approved', user_id = new_user, decided_at = now(), decided_by = caller.account_id
   WHERE id = p_request;
  RETURN new_user;
END
$$;
REVOKE ALL ON FUNCTION auth.approve_signup(bytea, uuid) FROM PUBLIC;

CREATE FUNCTION auth.reinvite_new_member(p_session bytea, p_user uuid, p_token_hash bytea, p_hours integer)
RETURNS TABLE (user_id uuid, email text, name text)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
  caller record;
  previous record;
  new_user uuid;
BEGIN
  SELECT * INTO caller FROM auth.session_owner(p_session);
  IF NOT FOUND OR NOT caller.is_admin THEN
    RAISE EXCEPTION 'only an administrator may resend invitations' USING ERRCODE = '42501';
  END IF;
  SELECT i.email, coalesce((SELECT r.name FROM auth.signup_requests r WHERE r.user_id = i.user_id
                             ORDER BY r.decided_at DESC LIMIT 1), '') AS name
    INTO previous
    FROM auth.invites i JOIN auth.self_service_users ss ON ss.user_id = i.user_id
   WHERE i.user_id = p_user AND NOT EXISTS (SELECT 1 FROM auth.accounts a WHERE a.user_id = i.user_id)
   ORDER BY i.created_at DESC, i.id DESC LIMIT 1 FOR UPDATE OF i;
  IF NOT FOUND THEN RETURN; END IF;
  UPDATE auth.invites i SET expires_at = now()
   WHERE i.email = previous.email AND i.accepted_at IS NULL AND i.expires_at > now();
  SELECT n.user_id INTO new_user
    FROM auth.invite_new_member(p_token_hash, previous.email, false, p_hours) n;
  UPDATE auth.invites i SET created_by = caller.account_id
   WHERE i.user_id = new_user AND i.token_hash = p_token_hash;
  RETURN QUERY SELECT new_user, previous.email, previous.name;
END
$$;
REVOKE ALL ON FUNCTION auth.reinvite_new_member(bytea, uuid, bytea, integer) FROM PUBLIC;
