-- Run by test-migrate.sh after repeated migrations. All fixtures roll back.
BEGIN;
DO $$
DECLARE
  first_user uuid := gen_random_uuid();
  other_user uuid := gen_random_uuid();
  first_account uuid;
  session_hash bytea := sha256(gen_random_uuid()::text::bytea);
  old_email text := gen_random_uuid()::text || '@example.test';
  new_email text := gen_random_uuid()::text || '@example.test';
BEGIN
  IF NOT has_function_privilege('web_app', 'auth.set_my_profile(bytea,text,text,date,text,text)', 'EXECUTE')
     OR NOT has_table_privilege('web_app', 'auth.native_handoffs', 'SELECT,INSERT,DELETE') THEN
    RAISE EXCEPTION 'native account grants did not survive repeated migrations';
  END IF;
  INSERT INTO public.users(id, name) VALUES (first_user, 'Before'), (other_user, 'Untouched');
  INSERT INTO auth.accounts(user_id, email, password_hash) VALUES (first_user, old_email, 'test-hash') RETURNING id INTO first_account;
  INSERT INTO auth.sessions(id, account_id, expires_at, purpose) VALUES (session_hash, first_account, now() + interval '1 day', 'personal');
  PERFORM auth.set_my_profile(session_hash, 'After', old_email, '2000-02-29', 'female', NULL);
  IF (SELECT name FROM public.users WHERE id = first_user) <> 'After'
     OR (SELECT dob FROM public.users WHERE id = first_user) <> '2000-02-29'::date
     OR (SELECT name FROM public.users WHERE id = other_user) <> 'Untouched' THEN
    RAISE EXCEPTION 'profile update did not preserve account isolation/date';
  END IF;
  BEGIN
    PERFORM auth.set_my_profile(session_hash, 'Wrong', new_email, NULL, NULL, NULL);
    RAISE EXCEPTION 'email changed without current password';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  INSERT INTO auth.password_resets(token_hash, account_id, password_hash, expires_at)
    VALUES (sha256('reset-fixture'::bytea), first_account, 'test-hash', now() + interval '1 hour');
  PERFORM auth.set_my_profile(session_hash, NULL, new_email, NULL, NULL, 'test-hash');
  IF (SELECT email FROM auth.accounts WHERE id = first_account) <> new_email
     OR EXISTS (SELECT 1 FROM auth.password_resets WHERE account_id = first_account)
     OR EXISTS (SELECT 1 FROM public.users WHERE id = first_user AND (name IS NOT NULL OR dob IS NOT NULL OR biological_sex IS NOT NULL)) THEN
    RAISE EXCEPTION 'account email/clears/recovery invalidation did not persist';
  END IF;
  BEGIN
    PERFORM auth.set_my_profile(sha256('unknown'::bytea), 'Wrong', new_email, NULL, NULL, NULL);
    RAISE EXCEPTION 'unknown session changed profile';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  INSERT INTO auth.native_handoffs(code_hash, challenge, account_id, source_session)
    VALUES (sha256('handoff-fixture'::bytea), repeat('a', 43), first_account, session_hash);
  UPDATE auth.sessions SET expires_at = now() - interval '1 second' WHERE id = session_hash;
  BEGIN
    PERFORM auth.set_my_profile(session_hash, 'Wrong', new_email, NULL, NULL, NULL);
    RAISE EXCEPTION 'expired session changed profile';
  EXCEPTION WHEN insufficient_privilege THEN NULL; END;
  DELETE FROM auth.sessions WHERE id = session_hash;
  IF EXISTS (SELECT 1 FROM auth.native_handoffs WHERE account_id = first_account) THEN
    RAISE EXCEPTION 'revoked source session left a usable account handoff';
  END IF;
END $$;
ROLLBACK;
