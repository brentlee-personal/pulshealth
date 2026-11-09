CREATE TABLE family_record_identity (
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  source_connection_id uuid NOT NULL,
  record_type text NOT NULL CHECK (length(record_type) BETWEEN 1 AND 256),
  source_record_id text NOT NULL CHECK (length(source_record_id) BETWEEN 1 AND 2048),
  raw_source_record_id text NOT NULL,
  payload_digest bytea NOT NULL CHECK (octet_length(payload_digest) = 32),
  physical_table text NOT NULL CHECK (physical_table IN (
    'quantity_samples','category_samples','workouts','heartbeat_series',
    'ecg_samples','state_of_mind','medication_dose_events')),
  physical_uuid uuid NOT NULL,
  physical_start timestamptz NOT NULL,
  physical_end timestamptz NOT NULL CHECK (physical_end >= physical_start),
  source_facts jsonb NOT NULL CHECK (jsonb_typeof(source_facts) = 'object'),
  first_credential_id uuid NOT NULL,
  first_batch_id uuid NOT NULL,
  first_received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (user_id, source_connection_id, record_type, source_record_id),
  UNIQUE (user_id, physical_table, physical_uuid),
  FOREIGN KEY (user_id, source_connection_id)
    REFERENCES family_source_connections(user_id, id) ON DELETE CASCADE,
  FOREIGN KEY (user_id, first_credential_id)
    REFERENCES device_tokens(user_id, credential_id)
);

