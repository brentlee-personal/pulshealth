-- Scope existing physical identities to their owner. Source/receipt identities
-- are separate contracts; this migration does not assert that they are complete.
-- Apply with this project's writers stopped, after preserving a scoped snapshot.

SET LOCAL lock_timeout = '10s';

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM batches WHERE user_id IS NULL) THEN
    RAISE EXCEPTION 'batch owner is unknown; reconcile the preserved snapshot before migrating';
  END IF;
END
$$;

CREATE TEMP TABLE family_member_keys_compressed (chunk regclass PRIMARY KEY)
ON COMMIT DROP;

INSERT INTO family_member_keys_compressed
SELECT format('%I.%I', chunk_schema, chunk_name)::regclass
FROM timescaledb_information.chunks
WHERE hypertable_schema = 'public'
  AND hypertable_name IN ('quantity_samples', 'workout_route_points', 'workout_series_points')
  AND is_compressed;

DO $$
DECLARE
  compressed record;
  target record;
  previous_key name;
BEGIN
  FOR compressed IN SELECT chunk FROM family_member_keys_compressed ORDER BY chunk::text LOOP
    PERFORM decompress_chunk(compressed.chunk);
  END LOOP;

  FOR target IN SELECT * FROM (VALUES
    ('quantity_samples',     'user_id, uuid, start_ts'),
    ('category_samples',     'user_id, uuid'),
    ('workouts',             'user_id, uuid'),
    ('heartbeat_series',     'user_id, uuid'),
    ('ecg_samples',           'user_id, uuid'),
    ('state_of_mind',         'user_id, uuid'),
    ('medication_dose_events','user_id, uuid'),
    ('workout_route_points', 'user_id, workout_uuid, ts'),
    ('workout_series_points','user_id, workout_uuid, type_id, ts'),
    ('deleted_samples',      'user_id, uuid'),
    ('batches',              'user_id, batch_id')
  ) AS keys(table_name, columns) LOOP
    SELECT conname INTO previous_key FROM pg_constraint
    WHERE conrelid = format('public.%I', target.table_name)::regclass AND contype = 'p';
    IF previous_key IS NULL THEN
      RAISE EXCEPTION 'expected primary key is absent on %', target.table_name;
    END IF;
    EXECUTE format('ALTER TABLE public.%I DROP CONSTRAINT %I', target.table_name, previous_key);
    EXECUTE format('ALTER TABLE public.%I ADD PRIMARY KEY (%s)', target.table_name, target.columns);
  END LOOP;

  FOR compressed IN SELECT chunk FROM family_member_keys_compressed ORDER BY chunk::text LOOP
    PERFORM compress_chunk(compressed.chunk, true);
  END LOOP;
END
$$;

ALTER TABLE batches ADD CONSTRAINT batches_user_id_fkey
  FOREIGN KEY (user_id) REFERENCES users(id);
