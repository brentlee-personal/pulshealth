CREATE TABLE family_source_observations (
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  source_connection_id uuid NOT NULL CHECK (source_connection_id <> '00000000-0000-0000-0000-000000000000'),
  query_key text NOT NULL CHECK (length(query_key) BETWEEN 1 AND 2048),
  observation_id uuid NOT NULL DEFAULT gen_random_uuid(),
  credential_id uuid NOT NULL,
  sequence_number bigint NOT NULL DEFAULT 1 CHECK (sequence_number > 0),
  issued_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL CHECK (expires_at > issued_at),
  PRIMARY KEY (user_id, source_connection_id, query_key),
  UNIQUE (user_id, source_connection_id, observation_id),
  FOREIGN KEY (user_id, source_connection_id)
    REFERENCES family_source_connections(user_id,id) ON DELETE CASCADE,
  FOREIGN KEY (user_id, credential_id)
    REFERENCES device_tokens(user_id,credential_id)
);
