CREATE SCHEMA api;
REVOKE ALL ON SCHEMA api FROM PUBLIC;

CREATE FUNCTION public.puls_api_time_bound(p text, round_up boolean) RETURNS timestamptz
LANGUAGE plpgsql IMMUTABLE
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
  fraction numeric;
  micros bigint;
BEGIN
  IF public.puls_utc_nanoseconds(p) IS NULL THEN RETURN NULL; END IF;
  fraction := rpad(coalesce(substring(p FROM '\.([0-9]+)Z$'), ''), 9, '0')::numeric;
  micros := CASE WHEN round_up THEN ceil(fraction / 1000) ELSE floor(fraction / 1000) END;
  RETURN (substring(p, 1, 19) || 'Z')::timestamptz + (micros::text || ' microseconds')::interval;
END
$$;

CREATE FUNCTION api.read_scope()
RETURNS TABLE(member_id uuid, record_types text[], from_ns numeric, until_ns numeric,
              from_ts timestamptz, until_start_ts timestamptz, until_end_ts timestamptz)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
  token_hash text := current_setting('puls.api_credential_hash', true);
  p jsonb;
BEGIN
  IF token_hash IS NULL OR token_hash !~ '^[0-9a-f]{64}$' THEN RETURN; END IF;
  SELECT r.principal INTO p FROM auth.resolve_api_credential(decode(token_hash, 'hex')) r;
  IF NOT FOUND THEN RETURN; END IF;
  RETURN QUERY SELECT (p->>'member_id')::uuid,
    ARRAY(SELECT jsonb_array_elements_text(p->'allowed_types')),
    public.puls_utc_nanoseconds(p->'allowed_time_range'->>'from'),
    public.puls_utc_nanoseconds(p->'allowed_time_range'->>'until'),
    public.puls_api_time_bound(p->'allowed_time_range'->>'from', true),
    public.puls_api_time_bound(p->'allowed_time_range'->>'until', true),
    public.puls_api_time_bound(p->'allowed_time_range'->>'until', false);
END
$$;
REVOKE ALL ON FUNCTION api.read_scope() FROM PUBLIC;

-- Keep one calendar/deduplication query for public reference reads and scoped reads.
CREATE FUNCTION public.puls_metric_daily_query(relation_schema text) RETURNS text
LANGUAGE sql IMMUTABLE
SET search_path = pg_catalog, pg_temp
AS $body$
  SELECT format($query$
WITH type_semantics AS (
  SELECT type_id,
         CASE WHEN bool_or(agg_func = 'sum') THEN 'cumulative'
              WHEN bool_or(agg_func = 'average') THEN 'discrete' END AS semantic
  FROM %1$I.aggregate_series
  GROUP BY type_id
  HAVING bool_or(agg_func IN ('sum', 'average'))
),
candidates AS (
  SELECT t.identifier, s.type_id, b.user_id,
         calendar.local_start::date AS day,
         b.value,
         1 AS tier, b.updated_at, b.bucket_start
  FROM %1$I.aggregate_samples b
  JOIN %1$I.aggregate_series s USING (series_id)
  JOIN type_semantics ts USING (type_id)
  JOIN %1$I.sample_types t ON t.type_id = s.type_id
  JOIN %1$I.users u ON u.id = b.user_id
  LEFT JOIN public.temporal_contexts tc ON tc.temporal_context_id = b.bucket_start_temporal_context_id
  LEFT JOIN public.temporal_contexts ec ON ec.temporal_context_id = b.bucket_end_temporal_context_id
  CROSS JOIN LATERAL (
    SELECT
      CASE WHEN tc.temporal_context_id IS NOT NULL
           THEN (b.bucket_start AT TIME ZONE 'UTC') + make_interval(secs => tc.utc_offset_seconds)
           ELSE b.bucket_start AT TIME ZONE coalesce(u.time_zone, public.puls_time_zone()) END AS local_start,
      CASE WHEN ec.temporal_context_id IS NOT NULL
           THEN (b.bucket_end AT TIME ZONE 'UTC') + make_interval(secs => ec.utc_offset_seconds)
           WHEN tc.temporal_context_id IS NOT NULL THEN
             CASE WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_timezone_names WHERE name = tc.time_zone_id)
                  THEN b.bucket_end AT TIME ZONE tc.time_zone_id
                  ELSE (b.bucket_end AT TIME ZONE 'UTC') + make_interval(secs => tc.utc_offset_seconds) END
           ELSE b.bucket_end AT TIME ZONE coalesce(u.time_zone, public.puls_time_zone()) END AS local_end
  ) calendar
  WHERE b.value IS NOT NULL
    AND s.interval_value = 1
    AND s.interval_unit = 'day'
    AND s.device_filter = 'all'
    -- Preserve deployed 023's recording-quality rule: a one-day series may
    -- retain clipped/misaligned buckets from an older anchor. Only complete
    -- local midnight-to-midnight days enter the headline, using recorded
    -- phone context for travel and the account zone for legacy buckets.
    -- Separate boundary offsets preserve 23/25-hour (and half-hour) DST days.
    AND calendar.local_start = calendar.local_start::date::timestamp
    AND calendar.local_end = (calendar.local_start::date + 1)::timestamp
    AND (tc.temporal_context_id IS NULL OR ec.temporal_context_id IS NULL
         OR tc.time_zone_id = ec.time_zone_id)
    AND ((ts.semantic = 'cumulative' AND s.agg_func = 'sum')
      OR (ts.semantic = 'discrete' AND s.agg_func = 'average'))
  UNION ALL
  SELECT t.identifier, ts.type_id, d.user_id, d.day, d.value,
         2, NULL::timestamptz, NULL::timestamptz
  FROM type_semantics ts
  JOIN %1$I.sample_types t ON t.type_id = ts.type_id
  CROSS JOIN LATERAL (
    SELECT per_source.user_id, per_source.day,
           CASE WHEN ts.semantic = 'cumulative'
                THEN (array_agg(per_source.value ORDER BY per_source.value DESC))[1]
                ELSE sum(per_source.total) / nullif(sum(per_source.n), 0) END AS value
    FROM (
      SELECT r.user_id, r.source_id, parts.day,
             CASE WHEN ts.semantic = 'cumulative' THEN sum(parts.total)
                  ELSE sum(parts.total) / nullif(sum(parts.n), 0) END AS value,
             sum(parts.total) AS total, sum(parts.n) AS n
      FROM %1$I.quantity_rollups r
      JOIN %1$I.users u ON u.id = r.user_id
      CROSS JOIN LATERAL (
        SELECT coalesce(u.time_zone, public.puls_time_zone()) AS zone
      ) z
      CROSS JOIN LATERAL (
        SELECT (r.bucket AT TIME ZONE z.zone)::date AS first_day,
               ((r.bucket + interval '1 hour' - interval '1 microsecond') AT TIME ZONE z.zone)::date AS last_day
      ) bounds
      CROSS JOIN LATERAL (
        -- Most hourly rollups lie within one account day. Retain that cheap
        -- path. Fractional-offset midnight (e.g. Kathmandu at UTC18:15) can
        -- split an hour; only that hour is read from raw rows to avoid moving
        -- samples across days or inventing a proportional split.
        SELECT bounds.first_day AS day,
               CASE WHEN ts.semantic = 'cumulative' THEN r.sum_value ELSE r.avg_value * r.n END AS total, r.n
         WHERE bounds.first_day = bounds.last_day
        UNION ALL
        SELECT (q.start_ts AT TIME ZONE z.zone)::date AS day,
               CASE WHEN ts.semantic = 'cumulative' THEN sum(q.value) ELSE avg(q.value) * count(*) END AS total, count(*) AS n
          FROM %1$I.quantity_samples q
         WHERE bounds.first_day <> bounds.last_day
           AND q.user_id = r.user_id AND q.type_id = r.type_id AND q.source_id IS NOT DISTINCT FROM r.source_id
           AND q.start_ts >= r.bucket AND q.start_ts < r.bucket + interval '1 hour'
         GROUP BY day
      ) parts
      WHERE r.type_id = ts.type_id
      GROUP BY r.user_id, r.source_id, parts.day
    ) per_source
    GROUP BY per_source.user_id, per_source.day
  ) d
),
ranked AS (
  SELECT *, row_number() OVER (
    PARTITION BY identifier, type_id, user_id, day
    ORDER BY tier, updated_at DESC, bucket_start DESC
  ) AS preference FROM candidates
)
SELECT identifier, type_id, user_id, day, value,
       CASE WHEN tier = 1 THEN 'aggregate' ELSE 'rollup' END AS source
FROM ranked WHERE preference = 1
$query$, relation_schema)
$body$;
REVOKE ALL ON FUNCTION public.puls_metric_daily_query(text) FROM PUBLIC;

CREATE OR REPLACE FUNCTION public.puls_create_metric_daily() RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
  EXECUTE 'CREATE OR REPLACE VIEW public.metric_daily AS ' || public.puls_metric_daily_query('public');
END
$$;
REVOKE ALL ON FUNCTION public.puls_create_metric_daily() FROM PUBLIC;

CREATE FUNCTION public.puls_create_api_views() RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $factory$
DECLARE
  relation record;
  type_expression text;
  end_expression text;
BEGIN
  DROP VIEW IF EXISTS api.metric_daily, api.quantity_rollups, api.sources,
    api.aggregate_series, api.aggregate_samples, api.sample_types, api.category_labels,
    api.users, api.activity_summaries, api.quantity_samples, api.category_samples,
    api.workouts, api.heartbeat_series, api.ecg_samples, api.state_of_mind,
    api.medication_dose_events, api.workout_route_points, api.workout_series_points;

  FOR relation IN SELECT * FROM (VALUES
    ('quantity_samples', NULL::text), ('category_samples', NULL::text),
    ('workouts', 'HKWorkoutTypeIdentifier'),
    ('heartbeat_series', 'HKDataTypeIdentifierHeartbeatSeries'),
    ('ecg_samples', 'HKDataTypeIdentifierElectrocardiogram'),
    ('state_of_mind', 'HKDataTypeIdentifierStateOfMind'),
    ('medication_dose_events', 'HKMedicationDoseEventTypeIdentifierMedicationDoseEvent')
  ) AS configured(table_name, identifier)
  LOOP
    type_expression := CASE WHEN relation.identifier IS NULL
      THEN '(SELECT identifier FROM public.sample_types WHERE type_id=q.type_id)'
      ELSE quote_literal(relation.identifier) END;
    end_expression := CASE WHEN EXISTS (
      SELECT 1 FROM pg_attribute WHERE attrelid=format('public.%I', relation.table_name)::regclass
        AND attname='end_ts' AND attnum>0 AND NOT attisdropped
    ) THEN 'q.end_ts' ELSE 'q.start_ts' END;
    EXECUTE format($view$
      CREATE VIEW api.%1$I WITH (security_barrier) AS
      WITH scope AS MATERIALIZED (SELECT * FROM api.read_scope())
      SELECT q.* FROM public.%1$I q CROSS JOIN scope s
      LEFT JOIN public.family_record_identity f ON f.user_id=q.user_id
        AND f.physical_table=%1$L AND f.physical_uuid=q.uuid
      WHERE q.user_id=s.member_id AND %2$s=ANY(s.record_types)
        AND q.start_ts>=s.from_ts AND q.start_ts<s.until_start_ts AND %3$s<=s.until_end_ts
        AND (f.user_id IS NULL OR (f.record_type=%2$s
          AND public.puls_utc_nanoseconds(f.source_facts->>'start_at')>=s.from_ns
          AND public.puls_utc_nanoseconds(f.source_facts->>'start_at')<s.until_ns
          AND public.puls_utc_nanoseconds(f.source_facts->>'end_at')<=s.until_ns))
    $view$, relation.table_name, type_expression, end_expression);
  END LOOP;

  FOR relation IN SELECT * FROM (VALUES
    ('workout_route_points','puls.v1.route'), ('workout_series_points','puls.v1.series')
  ) AS configured(table_name, identifier)
  LOOP
    type_expression := CASE WHEN relation.table_name='workout_series_points'
      THEN '(SELECT identifier FROM public.sample_types WHERE type_id=q.type_id)'
      ELSE 'NULL::text' END;
    EXECUTE format($view$
      CREATE VIEW api.%1$I WITH (security_barrier) AS
      WITH scope AS MATERIALIZED (SELECT * FROM api.read_scope())
      SELECT q.* FROM public.%1$I q CROSS JOIN scope s
      LEFT JOIN public.family_record_identity f ON f.user_id=q.user_id AND f.physical_table=%1$L
        AND f.physical_parent_uuid=q.workout_uuid AND f.physical_start=q.ts
        AND f.physical_type IS NOT DISTINCT FROM %3$s
      WHERE q.user_id=s.member_id AND %2$L=ANY(s.record_types)
        AND q.ts>=s.from_ts AND q.ts<s.until_start_ts
        AND (f.user_id IS NULL OR (f.record_type=%2$L
          AND public.puls_utc_nanoseconds(f.source_facts->>'start_at')>=s.from_ns
          AND public.puls_utc_nanoseconds(f.source_facts->>'start_at')<s.until_ns))
    $view$, relation.table_name, relation.identifier, type_expression);
  END LOOP;

  CREATE VIEW api.sample_types WITH (security_barrier) AS
    WITH scope AS MATERIALIZED (SELECT * FROM api.read_scope())
    SELECT t.* FROM public.sample_types t CROSS JOIN scope s
     WHERE t.identifier=ANY(s.record_types)
        OR ('puls.v1.series'=ANY(s.record_types) AND EXISTS (
          SELECT 1 FROM api.workout_series_points p WHERE p.type_id=t.type_id));

  CREATE VIEW api.category_labels WITH (security_barrier) AS
    SELECT c.* FROM public.category_labels c
     WHERE EXISTS (SELECT 1 FROM api.sample_types t WHERE t.identifier=c.type_identifier);

  CREATE VIEW api.sources WITH (security_barrier) AS
    SELECT s.* FROM public.sources s
     WHERE EXISTS (SELECT 1 FROM api.quantity_samples q WHERE q.source_id=s.source_id)
        OR EXISTS (SELECT 1 FROM api.category_samples c WHERE c.source_id=s.source_id);

  CREATE VIEW api.aggregate_series WITH (security_barrier) AS
    SELECT g.* FROM public.aggregate_series g
     WHERE EXISTS (SELECT 1 FROM api.sample_types t WHERE t.type_id=g.type_id);

  CREATE VIEW api.aggregate_samples WITH (security_barrier) AS
    WITH scope AS MATERIALIZED (SELECT * FROM api.read_scope())
    SELECT q.* FROM public.aggregate_samples q CROSS JOIN scope s
    JOIN public.aggregate_series g ON g.series_id=q.series_id
    JOIN public.sample_types t ON t.type_id=g.type_id
    LEFT JOIN public.family_snapshot_identity f ON f.user_id=q.user_id
      AND f.source_connection_id=q.source_connection_id AND f.record_type='puls.v1.aggregate'
      AND f.physical_series_id=q.series_id AND f.physical_bucket_start=q.bucket_start
    WHERE q.user_id=s.member_id AND t.identifier=ANY(s.record_types)
      AND q.bucket_start>=s.from_ts AND q.bucket_start<s.until_start_ts AND q.bucket_end<=s.until_end_ts
      AND (f.user_id IS NULL OR (
        public.puls_utc_nanoseconds(f.source_facts->>'start_at')>=s.from_ns
        AND public.puls_utc_nanoseconds(f.source_facts->>'end_at')<=s.until_ns));

  CREATE VIEW api.activity_summaries WITH (security_barrier) AS
    WITH scope AS MATERIALIZED (SELECT * FROM api.read_scope())
    SELECT q.* FROM public.activity_summaries q CROSS JOIN scope s
    JOIN public.family_snapshot_identity f ON f.user_id=q.user_id
      AND f.source_connection_id=q.source_connection_id AND f.record_type='HKActivitySummaryTypeIdentifier'
      AND f.physical_date=q.date
    WHERE q.user_id=s.member_id AND 'HKActivitySummaryTypeIdentifier'=ANY(s.record_types)
      AND f.source_facts#>>'{source_local_date,status}'='provided'
      AND f.source_facts#>>'{source_local_date,confidence}'='explicit'
      AND f.source_facts#>>'{source_time_zone,status}'='provided'
      AND f.source_facts#>>'{source_time_zone,confidence}'='explicit'
      AND CASE WHEN EXISTS (SELECT 1 FROM pg_timezone_names z
                            WHERE z.name=f.source_facts#>>'{source_time_zone,value}') THEN
        (q.date::timestamp AT TIME ZONE (f.source_facts#>>'{source_time_zone,value}'))>=s.from_ts
        AND ((q.date+1)::timestamp AT TIME ZONE (f.source_facts#>>'{source_time_zone,value}'))<=s.until_end_ts
      ELSE false END;

  CREATE VIEW api.users WITH (security_barrier) AS
    WITH scope AS MATERIALIZED (SELECT * FROM api.read_scope())
    SELECT u.id, u.time_zone, profile.name, profile.email,
      (to_timestamp(profile.dob_epoch_ms/1000.0) AT TIME ZONE 'UTC')::date AS dob,
      profile.biological_sex
    FROM public.users u CROSS JOIN scope s
    LEFT JOIN LATERAL (
      SELECT fp.* FROM public.family_source_profiles fp
      JOIN public.family_snapshot_identity f ON f.user_id=fp.user_id
        AND f.source_connection_id=fp.source_connection_id AND f.record_type='puls.v1.profile'
        AND f.physical_profile_source=fp.source_connection_id
      WHERE fp.user_id=u.id AND 'puls.v1.profile'=ANY(s.record_types)
        AND public.puls_utc_nanoseconds(f.source_facts->>'start_at')>=s.from_ns
        AND public.puls_utc_nanoseconds(f.source_facts->>'start_at')<s.until_ns
      ORDER BY public.puls_utc_nanoseconds(f.source_facts->>'start_at') DESC, fp.source_connection_id
      LIMIT 1
    ) profile ON true
    WHERE u.id=s.member_id;

  CREATE VIEW api.quantity_rollups WITH (security_barrier) AS
    SELECT type_id, source_id, user_id, public.time_bucket('1 hour',start_ts) AS bucket,
      sum(value) AS sum_value, avg(value) AS avg_value, min(value) AS min_value,
      max(value) AS max_value, count(*) AS n
    FROM api.quantity_samples GROUP BY type_id, source_id, user_id, bucket;

  EXECUTE 'CREATE VIEW api.metric_daily WITH (security_barrier) AS ' || public.puls_metric_daily_query('api');
END
$factory$;
REVOKE ALL ON FUNCTION public.puls_create_api_views() FROM PUBLIC;
SELECT public.puls_create_api_views();
