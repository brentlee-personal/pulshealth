ALTER TABLE device_tokens ADD COLUMN credential_id uuid NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE device_tokens ADD UNIQUE (user_id, credential_id);
ALTER TABLE device_tokens ADD UNIQUE (user_id, id, device_generation);

-- A valid token id alone cannot bind a batch to a different member or generation.
ALTER TABLE batches ADD CONSTRAINT batches_credential_owner_fkey
  FOREIGN KEY (user_id, device_token_id, device_generation)
  REFERENCES device_tokens(user_id, id, device_generation);
