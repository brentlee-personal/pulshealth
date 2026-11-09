# PulsHealth Data And Schema Guide

Use this reference to query a PulsHealth database without double-counting data
or mixing people and calendars. To install a server, start with the
[setup guide](setup.md). For most apps and AI tools, use the read-only
[product API](api.md) or [MCP connection](ai.md); direct SQL is optional.

Start with [Which Table Should I Use?](#which-table-should-i-use), then read the
relevant table and [gotchas](#important-gotchas). Raw samples preserve HealthKit
records; aggregates store HealthKit's computed buckets; `metric_daily` selects
a daily value with a documented fallback.

**SQL examples use psql variables.** In your psql session, set the intended
person and a bounded range before running them. Replace the example dates and
use the user ID from your pairing configuration; the UUID below is the default
single-user install. These filters select data; they are not an authorization
boundary for an administrator or the shared read-only role.

```sql
\set user_id '5ea4d000-0000-4000-8000-000000000001'
\set start_ts '2026-10-01T00:00:00Z'
\set end_ts '2026-11-01T00:00:00Z'
\set start_day '2026-10-01'
\set end_day '2026-11-01'
```

Timestamp ranges use `[start_ts, end_ts)`; daily queries use `[start_day,
end_day)` in the calendar described below. In another SQL client, substitute
bound parameters for psql's `:'name'` syntax.

## Mental Model

HealthKit data arrives as batches from the iOS app. Each batch contains one or
more HealthKit objects plus metadata about the device wake that produced the
upload. The ingest server normalizes shared dimensions, writes raw facts, and
records batch-level observability.

```mermaid
flowchart LR
  HK["HealthKit objects\nsamples, workouts, routes,\naggregates, activity rings"]
  Batch["Upload batch\none or more HealthKit types\nplus user/source/time context"]
  Lookups["Dimensions\nusers\nsample_types\nsources\ntemporal_contexts"]
  Facts["Raw fact tables\nquantity_samples\ncategory_samples\nworkouts\nspecialty tables"]
  Aggregates["Aggregate fact tables\naggregate_samples\nactivity_summaries"]
  Derived["Analysis surfaces\nquantity_rollups\nmetric_daily\ncategory_labels joins"]
  Ops["Ingest log\nbatches\ningest_rejections\ndeleted_samples"]

  HK --> Batch
  Batch --> Lookups
  Batch --> Facts
  Batch --> Aggregates
  Batch --> Ops
  Facts --> Derived
  Aggregates --> Derived
```

There are five kinds of data:

| Kind | What it means | Main tables |
|---|---|---|
| Dimensions | Shared labels and identity used by facts | `users`, `sample_types`, `sources`, `temporal_contexts`, `category_labels` |
| Raw samples | Individual HealthKit observations with UUIDs | `quantity_samples`, `category_samples`, `workouts`, `heartbeat_series`, `ecg_samples`, `state_of_mind`, `medication_dose_events` |
| Workout series | Points that belong to a workout | `workout_route_points`, `workout_series_points` |
| Aggregates | HealthKit-computed buckets and daily summaries | `aggregate_series`, `aggregate_samples`, `activity_summaries` |
| Operations | Sync history, rejected uploads, deletion tombstones, ingest timing | `batches`, `ingest_rejections`, `deleted_samples` |

## Schema Map

The public schema is centered on `users`, `sample_types`, and time-series fact
tables. Most rows belong to a user. Most HealthKit data rows point to
`sample_types` so you can filter by HealthKit identifier instead of local
numeric IDs.

```mermaid
erDiagram
  users ||--o{ quantity_samples : owns
  users ||--o{ category_samples : owns
  users ||--o{ workouts : owns
  users ||--o{ heartbeat_series : owns
  users ||--o{ ecg_samples : owns
  users ||--o{ state_of_mind : owns
  users ||--o{ medication_dose_events : owns
  users ||--o{ aggregate_samples : owns
  users ||--o{ activity_summaries : owns
  users ||--o{ batches : uploaded

  sample_types ||--o{ quantity_samples : classifies
  sample_types ||--o{ category_samples : classifies
  sample_types ||--o{ aggregate_series : defines
  sample_types ||--o{ workout_series_points : classifies

  sources ||--o{ quantity_samples : source
  sources ||--o{ category_samples : source
  sources ||--o{ workouts : source
  sources ||--o{ heartbeat_series : source
  sources ||--o{ ecg_samples : source
  sources ||--o{ state_of_mind : source
  sources ||--o{ medication_dose_events : source

  workouts ||--o{ workout_route_points : route
  workouts ||--o{ workout_series_points : series

  aggregate_series ||--o{ aggregate_samples : buckets
  category_labels ||--o{ category_samples : decodes

  temporal_contexts ||--o{ quantity_samples : local_time
  temporal_contexts ||--o{ category_samples : local_time
  temporal_contexts ||--o{ workouts : local_time
  temporal_contexts ||--o{ workout_route_points : local_time
  temporal_contexts ||--o{ workout_series_points : local_time
  temporal_contexts ||--o{ aggregate_samples : local_time
  temporal_contexts ||--o{ activity_summaries : local_day
```

## Core Concepts

### User

Every data row carries a `user_id`. The schema seeds one default user; ingest
adds any other user the first time a phone uploads with its id. For analysis,
always include `user_id` when comparing counts or building persistent
summaries.

Table: `users`

Grain: one row per person.

Primary key: `id`.

Important columns:

| Column | Meaning |
|---|---|
| `id` | UUID used as the foreign key on data tables |
| `name`, `email` | Optional profile identity from the app |
| `dob`, `biological_sex` | Profile attributes used by derived metrics such as heart-rate zones |
| `time_zone` | Account reporting calendar; NULL falls back to the stack setting |

### HealthKit Type

HealthKit identifiers such as `HKQuantityTypeIdentifierStepCount` are stored in
`sample_types`. Numeric `type_id` values are local database IDs. They are not
stable across databases.

Table: `sample_types`

Grain: one row per HealthKit identifier seen by ingest.

Primary key: `type_id`.

Natural key: `identifier`.

Important columns:

| Column | Meaning |
|---|---|
| `identifier` | Full HealthKit identifier string |
| `kind` | Broad kind such as `quantity`, `category`, or `workout` |
| `unit` | Canonical unit used by the app for quantity samples |

Rule: join through `sample_types.identifier`; do not hardcode `type_id`.

### Source

Sources describe where a HealthKit object came from, for example Apple Watch,
iPhone, or another app.

Table: `sources`

Grain: one unique `(name, bundle_id, version)` tuple.

Primary key: `source_id`.

Use this table when you need to separate Watch and iPhone data or understand
overlapping sources.

### Time

Event times are stored as `timestamptz` UTC instants. Some rows also carry
temporal-context IDs so the original local wall time can be reconstructed.

Table: `temporal_contexts`

Grain: one unique `(time_zone_id, utc_offset_seconds, source, confidence,
tzdb_version)` combination.

Important columns:

| Column | Meaning |
|---|---|
| `time_zone_id` | IANA zone such as `Europe/Berlin` |
| `utc_offset_seconds` | Offset at the event instant |
| `source` | How the context was obtained |
| `confidence` | How trustworthy the context is |

Use UTC timestamps for ordering. Use temporal context only when local wall-clock
meaning matters, such as sleep nights or local-day reconstruction.

### UUIDs And Idempotency

Most raw HealthKit objects have UUIDs. Retried uploads are expected, so raw
sample tables use UUID-based keys and ignore duplicates. Aggregate tables are
different: they are recomputed buckets, so they upsert by bucket identity.

## Table Families

### 1. Raw Quantity Samples

Table: `quantity_samples`

What it stores: numeric HealthKit samples: heart rate, body mass, steps, active
energy, HRV, distance, VO2 max, respiratory rate, and other
`HKQuantityTypeIdentifier*` data.

Grain: one row per HealthKit quantity sample, per user.

Primary key: `(user_id, uuid, start_ts)`.

TimescaleDB: hypertable on `start_ts`, one-month chunks, old chunks compressed
with columnstore.

Important columns:

| Column | Meaning |
|---|---|
| `uuid` | HealthKit object UUID |
| `type_id` | FK to `sample_types` |
| `start_ts`, `end_ts` | Sample interval |
| `value` | Numeric value in the app's canonical unit |
| `source_id` | FK to `sources` |
| `metadata` | HealthKit metadata as JSON |
| `user_id` | FK to `users` |
| `start_temporal_context_id`, `end_temporal_context_id` | Optional local-time reconstruction context |

Use this table when you need raw samples, latest readings, high-resolution
series, source-specific analysis, or debug-level inspection.

Example:

```sql
SELECT q.start_ts, q.end_ts, q.value, st.unit, s.name AS source
FROM quantity_samples q
JOIN sample_types st ON st.type_id = q.type_id
LEFT JOIN sources s ON s.source_id = q.source_id
WHERE q.user_id = :'user_id'::uuid
  AND q.start_ts >= :'start_ts'::timestamptz
  AND q.start_ts < :'end_ts'::timestamptz
  AND st.identifier = 'HKQuantityTypeIdentifierHeartRate'
ORDER BY q.start_ts DESC
LIMIT 25;
```

Gotcha: cumulative metrics such as steps can overlap across sources. For daily
truth, prefer `metric_daily` or HealthKit aggregate tables over naively summing
raw samples across all sources.

### 2. Raw Category Samples

Table: `category_samples`

What it stores: enum-like HealthKit intervals and events: sleep analysis,
symptoms, mindful sessions, stand hours, menstrual flow, and other
`HKCategoryTypeIdentifier*` data.

Grain: one row per HealthKit category sample, per user.

Primary key: `(user_id, uuid)`.

Important columns:

| Column | Meaning |
|---|---|
| `uuid` | HealthKit object UUID |
| `type_id` | FK to `sample_types` |
| `start_ts`, `end_ts` | Event or interval time |
| `value` | Integer enum value from HealthKit |
| `source_id` | FK to `sources` |
| `metadata` | HealthKit metadata as JSON |
| `user_id` | FK to `users` |

Join to `category_labels` to decode `value`.

```sql
SELECT c.start_ts, c.end_ts, c.value, cl.label, cl.enum_name
FROM category_samples c
JOIN sample_types st ON st.type_id = c.type_id
LEFT JOIN category_labels cl
  ON cl.type_identifier = st.identifier
 AND cl.value = c.value
WHERE c.user_id = :'user_id'::uuid
  AND c.start_ts >= :'start_ts'::timestamptz
  AND c.start_ts < :'end_ts'::timestamptz
  AND st.identifier = 'HKCategoryTypeIdentifierSleepAnalysis'
ORDER BY c.start_ts DESC
LIMIT 50;
```

These examples select intervals by their start. To find every interval that
overlaps a window, use `start_ts < end` and `end_ts > start`; merge overlapping
sleep intervals before totaling time.

Gotcha: a raw integer category value is not meaningful without its HealthKit
type. The same integer can mean different things for different category types.

### 3. Workouts

Table: `workouts`

What it stores: one HealthKit workout session with summary fields and rich JSON
details.

Grain: one row per workout UUID, per user.

Primary key: `(user_id, uuid)`.

Important columns:

| Column | Meaning |
|---|---|
| `activity_type` | HealthKit workout activity type |
| `start_ts`, `end_ts` | Workout interval |
| `duration_s` | Duration in seconds |
| `energy_kcal`, `distance_m` | Common workout totals |
| `stats` | Legacy/basic statistics JSON |
| `stats_detail` | Per-type min/avg/max/sum details |
| `events` | Workout events such as pauses or laps |
| `activities` | Multi-sport sub-activities |
| `source_id`, `metadata`, `user_id` | Shared dimensions |

Related tables:

| Table | Relationship |
|---|---|
| `workout_route_points` | GPS points keyed by `workout_uuid` |
| `workout_series_points` | Intra-workout quantity curves keyed by `workout_uuid` and `type_id` |

Example:

```sql
SELECT start_ts, end_ts, activity_type, duration_s, energy_kcal, distance_m,
       stats_detail
FROM workouts
WHERE user_id = :'user_id'::uuid
  AND start_ts >= :'start_ts'::timestamptz
  AND start_ts < :'end_ts'::timestamptz
ORDER BY start_ts DESC
LIMIT 20;
```

### 4. Workout Route Points

Table: `workout_route_points`

What it stores: GPS route points from `HKWorkoutRoute`.

Grain: one point per workout timestamp.

Primary key: `(user_id, workout_uuid, ts)`.

TimescaleDB: hypertable on `ts`.

Important columns:

| Column | Meaning |
|---|---|
| `workout_uuid` | Workout this point belongs to |
| `ts` | Point timestamp |
| `lat`, `lon` | Coordinates |
| `altitude_m`, `h_acc_m`, `v_acc_m`, `speed_mps`, `course_deg` | Optional GPS attributes |
| `user_id` | FK to `users` |

Use with `workouts` to draw routes or inspect route quality.

### 5. Workout Series Points

Table: `workout_series_points`

What it stores: intra-workout quantity streams such as heart rate, cycling
power, speed, or cadence.

Grain: one point per `(workout_uuid, type_id, ts)`.

Primary key: `(user_id, workout_uuid, type_id, ts)`.

TimescaleDB: hypertable on `ts`, compressed by workout for older chunks.

Use this table when the question is about what happened inside a workout, not
just the workout summary.

### 6. Specialty HealthKit Objects

These tables store HealthKit objects that are neither simple quantity samples
nor category samples.

| Table | Grain | What it stores |
|---|---|---|
| `heartbeat_series` | One row per heartbeat-series sample | Beat-to-beat offsets stored as JSON |
| `ecg_samples` | One row per ECG | Classification, average HR, sampling rate, symptoms status, voltage trace |
| `state_of_mind` | One row per state-of-mind entry | Momentary emotion or daily mood, valence, labels, associations |
| `medication_dose_events` | One row per medication dose event | Medication name, optional device-local `medication_id`, status, scheduled time, dose quantity and unit |

These tables follow the same general pattern: HealthKit UUID, start/end time,
optional source, metadata, user, and optional temporal context.

### 7. HealthKit Aggregate Series

Tables: `aggregate_series`, `aggregate_samples`

What they store: values computed on device by HealthKit statistics queries.
These are not raw samples. They are bucketed summaries such as daily step sum,
hourly heart-rate average, or weekly active-energy sum.

`aggregate_series` grain: one aggregate definition.

Natural key:

```text
(type_id, agg_func, interval_value, interval_unit, device_filter)
```

Important `aggregate_series` columns:

| Column | Meaning |
|---|---|
| `type_id` | HealthKit metric being aggregated |
| `agg_func` | `sum`, `average`, `min`, `max`, `mostRecent`, or `duration` |
| `interval_value`, `interval_unit` | Bucket size, such as `1 day` |
| `device_filter` | `all`, `watch`, or `iphone` |
| `unit` | Unit for bucket values |

`aggregate_samples` grain: one value for one aggregate series, bucket start,
and user.

Primary key: `(series_id, bucket_start, user_id)`.

Important `aggregate_samples` columns:

| Column | Meaning |
|---|---|
| `series_id` | FK to `aggregate_series` |
| `bucket_start`, `bucket_end` | Aggregate bucket interval |
| `value` | Bucket value; nullable when the bucket is explicitly empty |
| `user_id` | FK to `users` |
| `updated_at` | Last server update time |

Aggregates upsert. If HealthKit recomputes a bucket, the new upload overwrites
the old value.

Use aggregates when you want HealthKit's own source-deduped view of cumulative
metrics.

### 8. Activity Summaries

Table: `activity_summaries`

What it stores: daily `HKActivitySummary` rings: Move, Exercise, Stand, goals,
and move mode.

Grain: one local calendar day per user.

Primary key: `(user_id, date)`.

Important columns:

| Column | Meaning |
|---|---|
| `date` | Local activity-ring date, not a UTC timestamp |
| `move_kcal`, `move_goal_kcal` | Active-energy ring value and goal |
| `exercise_min`, `exercise_goal_min` | Exercise ring value and goal |
| `stand_hours`, `stand_goal_hours` | Stand ring value and goal |
| `move_mode` | Active-energy mode or Apple Move Time mode |
| `move_time_min`, `move_time_goal_min` | Move Time fields for that mode |

Activity summaries are mutable during the current day, so ingest upserts them.
Do not reconstruct rings from raw quantity samples unless this table is missing.

### 9. Category Labels

Table: `category_labels`

What it stores: a lookup from `(type_identifier, value)` to a readable label and
HealthKit enum name.

Grain: one enum value per category type.

Primary key: `(type_identifier, value)`.

Use this table whenever you query `category_samples.value`.

### 10. Quantity Rollups

View: `quantity_rollups`

What it stores: Timescale continuous aggregate over `quantity_samples`.

Grain: one hour per `(type_id, source_id, user_id)`.

Columns:

| Column | Meaning |
|---|---|
| `bucket` | One-hour bucket |
| `sum_value`, `avg_value`, `min_value`, `max_value` | Hourly stats |
| `n` | Number of raw samples in the bucket |

Use this for broad exploration across long time ranges. For cumulative metrics,
remember it is grouped by source so consumers can avoid double-counting
overlapping Watch and iPhone samples. Real-time aggregation is enabled, so reads
also combine the materialized history with raw rows from the current,
not-yet-materialized hour.

### 11. Metric Daily

View: `metric_daily`

What it stores: one daily value per metric and user. This is the best general
surface for daily analytics.

Grain: one row per `(identifier, user_id, day)`.

Columns:

| Column | Meaning |
|---|---|
| `identifier` | HealthKit identifier |
| `type_id` | Local sample type ID |
| `user_id` | User |
| `day` | Recorded phone day for aggregates with temporal context; account reporting day for raw fallback and legacy contextless aggregates |
| `value` | Daily value |
| `source` | `aggregate` if from HealthKit aggregate buckets, otherwise `rollup` |

Resolution order:

1. Prefer only the canonical HealthKit series: `sum` for an explicitly
   cumulative type or `average` for an explicitly discrete type, with
   `interval_value = 1`, `interval_unit = 'day'`, and `device_filter = 'all'`.
   HealthKit handles cross-source deduplication for that series. Other
   functions, intervals, and device-specific series never compete for the
   daily value. Both boundaries must be consecutive local midnights in the recorded
   phone context, or the account reporting zone when context is absent.
   Shifted or partial buckets remain stored but do not compete, even when
   uploaded more recently; DST days need not last 24 hours.
2. Fall back to `quantity_rollups` when no non-null canonical daily bucket exists.
   Cumulative types use the highest single-source daily sum to avoid overlap;
   discrete types use a sample-count-weighted cross-source average. Raw days
   use `puls_user_time_zone(user_id)` (`users.time_zone`, falling back to
   `PULS_TIME_ZONE`). This is an approximation of daily activity, not a
   recreation of HealthKit's source-resolution algorithm.

Only types with a configured `sum` or `average` series are included. That
explicitly establishes cumulative versus discrete semantics; a type configured
only with `min`, `max`, `mostRecent`, or `duration` is omitted rather than
assigned a plausible but incorrect daily value.

Example:

```sql
SELECT day, value, source
FROM metric_daily
WHERE user_id = :'user_id'::uuid
  AND day >= :'start_day'::date
  AND day < :'end_day'::date
  AND identifier = 'HKQuantityTypeIdentifierStepCount'
ORDER BY day DESC
LIMIT 30;
```

### 12. Batches

Table: `batches`

What it stores: one row per upload batch received by the ingest server. This is
operational data, but it is essential for understanding whether the health data
is current.

Grain: one row per member, server-assigned device generation and client batch UUID.

Primary key: `(user_id, device_generation, batch_id)`.

Migration 032 also scopes heartbeat, ECG, State of Mind, medication and deletion
identities by `user_id`. Raw UUIDs are source identifiers, not authorization.
It refuses batches without a known owner, temporarily decompresses affected
columnstore chunks, and restores their compression in the same migration
transaction. Keep a scoped snapshot and stop this project's writers before an
upgrade; an error rolls back the migration. This member-key migration alone
does not implement source-connection identities. Migration 033 adds private
`family_batch_receipts`: the decompressed-body SHA-256, original count result,
logical transaction UUID and transaction-final commit-record time. The result
and metadata become visible only after the real transaction commits; the
timestamp is not a claim about a PostgreSQL WAL commit timestamp. Deferred
constraints prevent an incomplete reservation from committing. Device generation
is trusted server enrollment state; zero is reserved for the existing legacy
shared-token mode, never a personal credential.

Important columns:

| Column | Meaning |
|---|---|
| `device_id` | Sending device identifier from the app |
| `type_identifier` | A label: the type that contributed the most samples (an incremental batch can carry several types) |
| `reason` | Why the client produced the batch |
| `sample_count`, `deletion_count`, `aggregate_count`, `activity_summary_count` | Payload counts |
| `bytes` | Compressed upload size |
| `exported_at` | Client export time |
| `received_at` | Server receipt time |
| `wake_id`, `trigger` | iOS wake correlation fields |
| `parse_ms`, `insert_ms` | Server timing |

Use this table first when debugging data freshness.

### 13. Ingest Rejections

Table: `ingest_rejections`

What it stores: metadata for authenticated batch requests that failed before a
successful row could be committed to `batches`. It deliberately stores no
HealthKit request-body data.

Important columns:

| Column | Meaning |
|---|---|
| `received_at` | Server time of the failed request |
| `batch_id`, `wake_id`, `trigger` | Client correlation headers, when present |
| `status` | HTTP response status |
| `stage` | Failure phase: `gzip`, `encoding`, `parse`, `identity`, `wake`, or `insert` |
| `error_message` | Bounded diagnostic message |
| `bytes` | Compressed bytes read before failure |

Use this with `batches` when deciding whether apparent silence means no wake or
failed uploads. Rows are kept for 90 days: a daily TimescaleDB job
(`017_prune_ingest_rejections.sql`) deletes older ones.

### 14. Deleted Samples

Table: `deleted_samples`

What it stores: HealthKit deletion tombstones reported by anchored queries.

Grain: one row per deleted UUID (HealthKit UUIDs are globally unique).

Important columns:

| Column | Meaning |
|---|---|
| `uuid` | Deleted HealthKit object UUID |
| `type_identifier` | HealthKit type of the deleted object |
| `deleted_at` | Server time when the tombstone was recorded |

The server also applies deletes to the raw tables when deletion information
arrives.

## Query Patterns

### List Metrics Present In The Database

This shared vocabulary lists types seen anywhere on the server, not proof that
the selected person has samples. Use `/v1/catalog/types` in the
[product API](api.md) for a user-scoped inventory.

```sql
SELECT identifier, kind, unit
FROM sample_types
ORDER BY identifier;
```

### Latest Value For A Quantity Type

```sql
SELECT q.start_ts, q.value, st.unit, src.name AS source_name
FROM quantity_samples q
JOIN sample_types st ON st.type_id = q.type_id
LEFT JOIN sources src ON src.source_id = q.source_id
WHERE q.user_id = :'user_id'::uuid
  AND q.start_ts >= :'start_ts'::timestamptz
  AND q.start_ts < :'end_ts'::timestamptz
  AND st.identifier = 'HKQuantityTypeIdentifierBodyMass'
ORDER BY q.start_ts DESC
LIMIT 10;
```

### Daily Metric Trend

```sql
SELECT day, value, source
FROM metric_daily
WHERE user_id = :'user_id'::uuid
  AND day >= :'start_day'::date
  AND day < :'end_day'::date
  AND identifier = 'HKQuantityTypeIdentifierStepCount'
ORDER BY day;
```

### Sleep By Stage

```sql
SELECT c.start_ts,
       c.end_ts,
       extract(epoch FROM c.end_ts - c.start_ts) / 3600.0 AS hours,
       cl.label AS stage
FROM category_samples c
JOIN sample_types st ON st.type_id = c.type_id
LEFT JOIN category_labels cl
  ON cl.type_identifier = st.identifier
 AND cl.value = c.value
WHERE c.user_id = :'user_id'::uuid
  AND c.start_ts >= :'start_ts'::timestamptz
  AND c.start_ts < :'end_ts'::timestamptz
  AND st.identifier = 'HKCategoryTypeIdentifierSleepAnalysis'
ORDER BY c.start_ts DESC;
```

### Recent Workouts With Routes

```sql
SELECT w.uuid,
       w.start_ts,
       w.activity_type,
       w.duration_s,
       w.distance_m,
       count(r.*) AS route_points
FROM workouts w
LEFT JOIN workout_route_points r
  ON r.workout_uuid = w.uuid AND r.user_id = w.user_id
WHERE w.user_id = :'user_id'::uuid
  AND w.start_ts >= :'start_ts'::timestamptz
  AND w.start_ts < :'end_ts'::timestamptz
GROUP BY w.uuid
ORDER BY w.start_ts DESC
LIMIT 20;
```

### Ingest Freshness

```sql
SELECT type_identifier,
       max(received_at) AS last_received_at,
       sum(sample_count) AS samples_received
FROM batches
WHERE user_id = :'user_id'::uuid
  AND received_at >= :'start_ts'::timestamptz
  AND received_at < :'end_ts'::timestamptz
GROUP BY type_identifier
ORDER BY last_received_at DESC NULLS LAST;
```

`type_identifier` names only a batch's biggest contributor, so a type that
rides along in other types' batches looks older here than it is.
`sum(sample_count)` counts uploaded payload samples, including repeats, not
unique stored rows. For the newest recorded event of one type, use
`max(start_ts)` from its user-scoped sample table; that timestamp does not say
when the server received it.

### One Row Per Device Wake

```sql
SELECT min(received_at) AS wake_started_at,
       trigger,
       count(*) AS batches,
       sum(sample_count) AS samples,
       sum(bytes) AS bytes,
       max(parse_ms) AS max_parse_ms,
       max(insert_ms) AS max_insert_ms
FROM batches
WHERE user_id = :'user_id'::uuid
  AND received_at >= :'start_ts'::timestamptz
  AND received_at < :'end_ts'::timestamptz
  AND wake_id IS NOT NULL
GROUP BY wake_id, trigger
ORDER BY wake_started_at DESC;
```

## Which Table Should I Use?

| Question | Use |
|---|---|
| What was my daily step count? | `metric_daily` |
| What is the latest body weight sample? | `quantity_samples` joined to `sample_types` |
| What sleep stages were recorded last night? | `category_samples` joined to `category_labels` |
| What workouts happened recently? | `workouts` |
| What was my route for a workout? | `workout_route_points` joined to `workouts` |
| What was heart rate during a workout? | `workout_series_points` joined to `sample_types` |
| What does HealthKit say the daily aggregate was? | `aggregate_samples` joined through `aggregate_series` |
| What are today's Activity rings? | `activity_summaries` |
| Did the phone upload recently? | `batches` |
| Why are category values numeric? | Decode with `category_labels` |

## Important Gotchas

- Do not hardcode `type_id`. Always filter on `sample_types.identifier`.
- Raw cumulative samples can double-count across sources. Prefer
  `metric_daily` or `aggregate_samples` for daily cumulative truth.
- `activity_summaries.date` is a local calendar date, not a UTC instant.
- Raw server-side day boundaries follow `puls_user_time_zone(user_id)`:
  `users.time_zone`, or the stack's `PULS_TIME_ZONE` when NULL. Existing users
  retain that fallback; newly approved hosted accounts initialize once from
  the first syncing phone and can change it explicitly in Account.
- Daily phone aggregates use their recorded temporal-context offset to retain
  the phone's calendar date, including travel; legacy contextless aggregates
  use the account reporting zone. Rings retain their stored local date.
  Thus a raw reporting day and a phone's recorded day can differ while traveling.
- `metric_daily` normally uses hourly quantity rollups for its raw fallback.
  Where fractional-offset midnight splits an hour (for example Kathmandu),
  it reads that hour's raw samples to assign each sample to the correct day.
- Raw samples are immutable facts; aggregate buckets and activity summaries are
  recomputed and upserted.
- `quantity_samples` and workout point tables are Timescale hypertables. Old
  chunks may be compressed.
- Category integer values only make sense with both `type_identifier` and
  `category_labels`.
- `batches.received_at` is server time; sample `start_ts` and `end_ts` are
  HealthKit event times.
- Deletion tombstones are best-effort HealthKit data. If HealthKit purges a
  tombstone before sync sees it, reconciliation is needed.

## Recording Behavior and Historical Metadata

Use the [recording guide](recording-behavior.md) for cadence, coverage, source
identity and context. Record counts describe the stored representation, not a
sensor sampling rate or a guarantee of complete monitoring.

Migration `023_recording_quality.sql` adds a read-time decoder for historical
heart-rate motion metadata. Older client code could bridge numeric 0/1 to JSON
booleans; the current Swift mapper preserves numeric enums. The decoder is
narrowly scoped to `HKMetadataKeyHeartRateMotionContext`: false/true decode as
0/1, numeric 0/1/2 retain their meanings, and missing or invalid values return
NULL. Other boolean metadata is never converted. Code 0 means *context not
set*, not sedentary; code 2 includes movement **or working out** and is not a
workout identifier. Raw records remain unchanged so their provenance survives.

```sql
-- psql variables: user_id, start_ts and end_ts. The range is [start, end).
SELECT q.start_ts,
       q.metadata -> 'HKMetadataKeyHeartRateMotionContext' AS stored_context,
       jsonb_typeof(q.metadata -> 'HKMetadataKeyHeartRateMotionContext') AS stored_kind,
       puls_heart_rate_motion_context(q.metadata) AS motion_context
FROM quantity_samples q
WHERE q.user_id = :'user_id'::uuid
  AND q.type_id = (SELECT type_id FROM sample_types
                  WHERE identifier = 'HKQuantityTypeIdentifierHeartRate')
  AND q.start_ts >= :'start_ts'::timestamptz
  AND q.start_ts < :'end_ts'::timestamptz
ORDER BY q.start_ts;
```

Use this decoder explicitly in SQL analyses that consume this metadata. It does
not rewrite exports or claim that arbitrary third-party boolean fields are enums.
The product API currently does not expose quantity-sample metadata.

Daily analytics now reject buckets whose actual boundaries do not fit the
configured local calendar. Do not delete all non-midnight buckets: they can
represent a different historical timezone or anchor. The stored bucket remains
evidence; `metric_daily` uses an aligned canonical bucket or its existing raw
rollup fallback. If neither exists, there is no daily row. The fallback remains
an approximation, not a recreation of HealthKit's source-resolution algorithm.

## Appendix: Minimal Connection Notes

Admin access on the Docker host running the stack, from the `server/`
directory (or wherever the compose file lives):

```bash
docker compose exec db psql -U postgres -d postgres
```

Read-only access from another machine. Postgres is not meant to be reachable
beyond the host, so open an SSH tunnel and connect through it as the read-only
`grafana` role:

```bash
ssh -N -L 15432:127.0.0.1:5432 <user>@<host>
psql -h 127.0.0.1 -p 15432 -U grafana -d postgres -W
```

Any GUI client (TablePlus, DBeaver, pgAdmin, DataGrip, ...) works the same way:
SSH tunnel to `<host>`, then host `127.0.0.1`, port `15432`, user `grafana`,
database `postgres`. The passwords live in the stack's ignored `.env`
(`POSTGRES_PASSWORD`, `GRAFANA_DB_PASSWORD`), never in this guide.

Enter `GRAFANA_DB_PASSWORD` at the password prompt; do not put it in shell
history. The shared `grafana` role can read all users, so only trusted operators
should have it. For account-scoped application access, use the product API or
the viewer's accounts mode.

For a fresh local database, use the [setup guide](setup.md), which generates
real credentials. Copying `.env.example` alone leaves placeholder passwords
that the migrator refuses. To start only the database and schema after valid
configuration exists, run `docker compose up -d migrate` from `server/`.
Migrations run through that service; see
[Schema migrations](../server/README.md#schema-migrations).
