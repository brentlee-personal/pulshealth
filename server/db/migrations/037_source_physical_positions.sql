ALTER TABLE family_record_identity DROP CONSTRAINT family_record_identity_physical_table_check;
ALTER TABLE family_record_identity ADD CHECK (physical_table IN (
  'quantity_samples','category_samples','workouts','heartbeat_series','ecg_samples',
  'state_of_mind','medication_dose_events','workout_route_points','workout_series_points'));
ALTER TABLE family_record_identity ALTER COLUMN physical_uuid DROP NOT NULL;
ALTER TABLE family_record_identity ADD COLUMN physical_parent_uuid uuid;
ALTER TABLE family_record_identity ADD COLUMN physical_type text;
ALTER TABLE family_record_identity ADD CHECK (
  (physical_table IN ('workout_route_points','workout_series_points') AND physical_uuid IS NULL AND physical_parent_uuid IS NOT NULL)
  OR (physical_table NOT IN ('workout_route_points','workout_series_points') AND physical_uuid IS NOT NULL AND physical_parent_uuid IS NULL)
);
CREATE UNIQUE INDEX family_route_record_position ON family_record_identity
  (user_id, source_connection_id, physical_parent_uuid, physical_start)
  WHERE physical_table='workout_route_points';
CREATE UNIQUE INDEX family_series_record_position ON family_record_identity
  (user_id, source_connection_id, physical_parent_uuid, physical_start, physical_type)
  WHERE physical_table='workout_series_points';

CREATE TABLE family_workout_references (
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  source_connection_id uuid NOT NULL,
  source_workout_uuid uuid NOT NULL,
  physical_uuid uuid NOT NULL DEFAULT gen_random_uuid(),
  PRIMARY KEY (user_id, source_connection_id, source_workout_uuid),
  UNIQUE (user_id, source_connection_id, physical_uuid),
  FOREIGN KEY (user_id, source_connection_id)
    REFERENCES family_source_connections(user_id,id) ON DELETE CASCADE
);
INSERT INTO family_workout_references(user_id,source_connection_id,source_workout_uuid,physical_uuid)
SELECT user_id,source_connection_id,source_record_id::uuid,physical_uuid
FROM family_record_identity WHERE physical_table='workouts';

ALTER TABLE family_record_identity ADD FOREIGN KEY (user_id,source_connection_id,physical_parent_uuid)
  REFERENCES family_workout_references(user_id,source_connection_id,physical_uuid);

CREATE TABLE family_source_tombstones (
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  source_connection_id uuid NOT NULL,
  source_record_uuid uuid NOT NULL,
  first_batch_id uuid NOT NULL,
  deleted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (user_id, source_connection_id, source_record_uuid),
  FOREIGN KEY (user_id,source_connection_id)
    REFERENCES family_source_connections(user_id,id) ON DELETE CASCADE
);
