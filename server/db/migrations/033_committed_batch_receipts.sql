-- A device enrollment has a server-assigned generation. Credential rotation
-- may preserve it only through a verified pairing flow, never an HTTP header.
ALTER TABLE device_tokens ADD COLUMN device_generation bigint;
UPDATE device_tokens SET device_generation = id;
ALTER TABLE device_tokens ALTER COLUMN device_generation SET NOT NULL;
ALTER TABLE device_tokens ADD CHECK (device_generation > 0);

CREATE FUNCTION assign_device_generation() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
BEGIN
  IF NEW.device_generation IS NULL THEN
    NEW.device_generation := NEW.id;
  END IF;
  RETURN NEW;
END
$$;
REVOKE ALL ON FUNCTION assign_device_generation() FROM PUBLIC;
CREATE TRIGGER assign_device_generation BEFORE INSERT ON device_tokens
FOR EACH ROW EXECUTE FUNCTION assign_device_generation();

-- Zero identifies the existing shared-token mode, not a personal credential.
ALTER TABLE batches ADD COLUMN device_generation bigint NOT NULL DEFAULT 0
  CHECK (device_generation >= 0);
UPDATE batches b SET device_generation = d.device_generation
FROM device_tokens d WHERE b.device_token_id = d.id AND b.user_id = d.user_id;
ALTER TABLE batches DROP CONSTRAINT batches_pkey;
ALTER TABLE batches ADD PRIMARY KEY (user_id, device_generation, batch_id);

CREATE TABLE family_batch_receipts (
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  device_generation bigint NOT NULL CHECK (device_generation >= 0),
  batch_id uuid NOT NULL,
  body_digest bytea NOT NULL CHECK (octet_length(body_digest) = 32),
  transaction_id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
  committed_at timestamptz,
  result jsonb CHECK (result IS NULL OR (
    jsonb_typeof(result) = 'object' AND result ?& ARRAY[
      'accepted', 'deleted', 'duplicates', 'routePoints', 'seriesPoints',
      'aggregateSamples', 'activitySummaries'
    ]
  )),
  PRIMARY KEY (user_id, device_generation, batch_id),
  FOREIGN KEY (user_id, device_generation, batch_id)
    REFERENCES batches(user_id, device_generation, batch_id) ON DELETE CASCADE
    DEFERRABLE INITIALLY DEFERRED
);

-- A reservation may be incomplete inside its transaction, but cannot commit
-- before the data and original result are ready. Read the final row, not NEW.
CREATE FUNCTION require_committed_batch_receipt() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM family_batch_receipts
    WHERE user_id = NEW.user_id AND device_generation = NEW.device_generation
      AND batch_id = NEW.batch_id AND (result IS NULL OR committed_at IS NULL)
  ) THEN
    RAISE EXCEPTION 'incomplete batch receipt cannot commit' USING ERRCODE = '23514';
  END IF;
  RETURN NULL;
END
$$;
REVOKE ALL ON FUNCTION require_committed_batch_receipt() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER require_committed_batch_receipt
AFTER INSERT OR UPDATE ON family_batch_receipts
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
EXECUTE FUNCTION require_committed_batch_receipt();
