ALTER TABLE aggregate_samples ADD COLUMN source_connection_id uuid
  CHECK (source_connection_id <> '00000000-0000-0000-0000-000000000000');
ALTER TABLE aggregate_samples ADD COLUMN source_scope uuid
  GENERATED ALWAYS AS (coalesce(source_connection_id,'00000000-0000-0000-0000-000000000000'::uuid)) STORED;
ALTER TABLE aggregate_samples DROP CONSTRAINT aggregate_samples_pkey;
ALTER TABLE aggregate_samples ADD PRIMARY KEY(user_id,source_scope,series_id,bucket_start);
ALTER TABLE aggregate_samples ADD FOREIGN KEY(user_id,source_connection_id)
  REFERENCES family_source_connections(user_id,id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE activity_summaries ADD COLUMN source_connection_id uuid
  CHECK (source_connection_id <> '00000000-0000-0000-0000-000000000000');
ALTER TABLE activity_summaries ADD COLUMN source_scope uuid
  GENERATED ALWAYS AS (coalesce(source_connection_id,'00000000-0000-0000-0000-000000000000'::uuid)) STORED;
ALTER TABLE activity_summaries DROP CONSTRAINT activity_summaries_pkey;
ALTER TABLE activity_summaries ADD PRIMARY KEY(user_id,source_scope,date);
ALTER TABLE activity_summaries ADD FOREIGN KEY(user_id,source_connection_id)
  REFERENCES family_source_connections(user_id,id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE family_source_profiles (
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  source_connection_id uuid NOT NULL,
  name text,
  email text,
  dob_epoch_ms double precision,
  biological_sex text CHECK(biological_sex IN ('female','male','other')),
  PRIMARY KEY(user_id,source_connection_id),
  FOREIGN KEY(user_id,source_connection_id)
    REFERENCES family_source_connections(user_id,id) ON DELETE CASCADE
);

CREATE TABLE family_snapshot_identity (
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  source_connection_id uuid NOT NULL,
  record_type text NOT NULL CHECK(record_type IN ('puls.v1.aggregate','HKActivitySummaryTypeIdentifier','puls.v1.profile')),
  source_record_id text NOT NULL CHECK(length(source_record_id) BETWEEN 1 AND 2048),
  payload_digest bytea NOT NULL CHECK(octet_length(payload_digest)=32),
  query_key text NOT NULL,
  observation_id uuid NOT NULL,
  observation_sequence bigint NOT NULL CHECK(observation_sequence>0),
  source_facts jsonb NOT NULL CHECK(jsonb_typeof(source_facts)='object'),
  physical_series_id smallint,
  physical_bucket_start timestamptz,
  physical_date date,
  physical_profile_source uuid,
  first_credential_id uuid NOT NULL,
  first_batch_id uuid NOT NULL,
  last_credential_id uuid NOT NULL,
  last_batch_id uuid NOT NULL,
  first_received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  last_received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY(user_id,source_connection_id,record_type,source_record_id),
  FOREIGN KEY(user_id,source_connection_id)
    REFERENCES family_source_connections(user_id,id) ON DELETE CASCADE,
  FOREIGN KEY(user_id,first_credential_id,source_connection_id)
    REFERENCES device_tokens(user_id,credential_id,source_connection_id) DEFERRABLE INITIALLY DEFERRED,
  FOREIGN KEY(user_id,last_credential_id,source_connection_id)
    REFERENCES device_tokens(user_id,credential_id,source_connection_id) DEFERRABLE INITIALLY DEFERRED,
  FOREIGN KEY(user_id,source_connection_id,query_key)
    REFERENCES family_source_observations(user_id,source_connection_id,query_key) DEFERRABLE INITIALLY DEFERRED,
  FOREIGN KEY(user_id,source_connection_id,physical_series_id,physical_bucket_start)
    REFERENCES aggregate_samples(user_id,source_scope,series_id,bucket_start) DEFERRABLE INITIALLY DEFERRED,
  FOREIGN KEY(user_id,source_connection_id,physical_date)
    REFERENCES activity_summaries(user_id,source_scope,date) DEFERRABLE INITIALLY DEFERRED,
  FOREIGN KEY(user_id,physical_profile_source)
    REFERENCES family_source_profiles(user_id,source_connection_id) DEFERRABLE INITIALLY DEFERRED,
  CHECK (
    (record_type='puls.v1.aggregate' AND physical_series_id IS NOT NULL AND physical_bucket_start IS NOT NULL
      AND physical_date IS NULL AND physical_profile_source IS NULL)
    OR (record_type='HKActivitySummaryTypeIdentifier' AND physical_date IS NOT NULL
      AND physical_series_id IS NULL AND physical_bucket_start IS NULL AND physical_profile_source IS NULL)
    OR (record_type='puls.v1.profile' AND source_record_id='profile' AND physical_profile_source IS NOT NULL AND physical_profile_source=source_connection_id
      AND physical_series_id IS NULL AND physical_bucket_start IS NULL AND physical_date IS NULL)
  )
);
CREATE UNIQUE INDEX family_snapshot_aggregate_position ON family_snapshot_identity
  (user_id,source_connection_id,physical_series_id,physical_bucket_start) WHERE record_type='puls.v1.aggregate';
CREATE UNIQUE INDEX family_snapshot_activity_position ON family_snapshot_identity
  (user_id,source_connection_id,physical_date) WHERE record_type='HKActivitySummaryTypeIdentifier';
