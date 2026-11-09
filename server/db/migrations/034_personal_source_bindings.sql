CREATE TABLE family_source_connections (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider text NOT NULL CHECK (provider IN ('apple_health', 'oura')),
  binding_method text NOT NULL CHECK (binding_method = 'local_member_pairing'),
  account_fingerprint text GENERATED ALWAYS AS
    (encode(sha256(uuid_send(id)), 'hex')) STORED,
  status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, id)
);
CREATE INDEX family_source_connections_owner ON family_source_connections(user_id, provider);

-- Hold the revocation lock without giving the writer UPDATE on source bindings.
CREATE FUNCTION family_apple_source_active(member_id uuid, source_id uuid) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  PERFORM 1 FROM public.family_source_connections
  WHERE user_id = member_id AND id = source_id AND provider = 'apple_health' AND status = 'active'
  FOR SHARE;
  RETURN FOUND;
END
$$;
REVOKE ALL ON FUNCTION family_apple_source_active(uuid, uuid) FROM PUBLIC;

CREATE TABLE family_device_generations (
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  device_generation bigint NOT NULL CHECK (device_generation > 0),
  source_connection_id uuid,
  PRIMARY KEY (user_id, device_generation),
  UNIQUE (user_id, device_generation, source_connection_id),
  FOREIGN KEY (user_id, source_connection_id)
    REFERENCES family_source_connections(user_id, id) ON DELETE CASCADE
);

-- Existing enrollments have no verified source binding. Preserve them as
-- unbound; personal writes require a new verified enrollment, not a guess.
INSERT INTO family_device_generations(user_id, device_generation)
SELECT DISTINCT user_id, device_generation FROM device_tokens;
ALTER TABLE device_tokens ADD COLUMN source_connection_id uuid;
ALTER TABLE device_tokens ADD FOREIGN KEY (user_id, device_generation, source_connection_id)
  REFERENCES family_device_generations(user_id, device_generation, source_connection_id);

CREATE FUNCTION bind_personal_device_source() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
DECLARE
  candidates uuid[];
BEGIN
  -- Serialize pairing only within one member. Device names and install IDs
  -- never select a source, and a second possible source requires a choice.
  PERFORM pg_advisory_xact_lock(hashtextextended(NEW.user_id::text, 342044));
  IF NEW.source_connection_id IS NULL THEN
    SELECT array_agg(id ORDER BY id) INTO candidates
    FROM family_source_connections
    WHERE user_id = NEW.user_id AND provider = 'apple_health' AND status = 'active';
    IF coalesce(cardinality(candidates), 0) = 0 THEN
      INSERT INTO family_source_connections(user_id, provider, binding_method)
      VALUES (NEW.user_id, 'apple_health', 'local_member_pairing')
      RETURNING id INTO NEW.source_connection_id;
    ELSIF cardinality(candidates) = 1 THEN
      NEW.source_connection_id := candidates[1];
    ELSE
      RAISE EXCEPTION 'source connection is ambiguous' USING ERRCODE = '23514';
    END IF;
  END IF;
  IF NOT family_apple_source_active(NEW.user_id, NEW.source_connection_id) THEN
    RAISE EXCEPTION 'source connection is not active for this member' USING ERRCODE = '23514';
  END IF;
  INSERT INTO family_device_generations(user_id, device_generation, source_connection_id)
  VALUES (NEW.user_id, NEW.device_generation, NEW.source_connection_id)
  ON CONFLICT (user_id, device_generation) DO NOTHING;
  PERFORM 1 FROM family_device_generations
  WHERE user_id = NEW.user_id AND device_generation = NEW.device_generation
    AND source_connection_id = NEW.source_connection_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'device generation cannot change its source' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END
$$;
REVOKE ALL ON FUNCTION bind_personal_device_source() FROM PUBLIC;
CREATE TRIGGER bind_personal_device_source
BEFORE INSERT OR UPDATE OF user_id, device_generation, source_connection_id ON device_tokens
FOR EACH ROW EXECUTE FUNCTION bind_personal_device_source();
