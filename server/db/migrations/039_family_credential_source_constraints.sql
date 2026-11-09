ALTER TABLE device_tokens ADD CONSTRAINT family_device_credential_source_unique
  UNIQUE (user_id, credential_id, source_connection_id);

-- Check credential/source equality at commit while user deletion completes its cascade.
ALTER TABLE family_record_identity
  DROP CONSTRAINT family_record_identity_user_id_first_credential_id_fkey;
ALTER TABLE family_record_identity ADD CONSTRAINT family_record_identity_credential_binding_fkey
  FOREIGN KEY (user_id, first_credential_id, source_connection_id)
  REFERENCES device_tokens(user_id, credential_id, source_connection_id)
  DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE family_source_observations
  DROP CONSTRAINT family_source_observations_user_id_credential_id_fkey;
ALTER TABLE family_source_observations ADD CONSTRAINT family_source_observations_credential_binding_fkey
  FOREIGN KEY (user_id, credential_id, source_connection_id)
  REFERENCES device_tokens(user_id, credential_id, source_connection_id)
  DEFERRABLE INITIALLY DEFERRED;
