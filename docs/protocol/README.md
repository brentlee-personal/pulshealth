# Puls Sync Protocol v1

The wire protocol the PulsHealth iOS app uses to push Apple Health data to a
backend. This document is written for someone implementing a **receiver** in
any language; the reference receiver is the Go ingest server in
[`server/ingest/`](../../server/ingest/), and a deliberately minimal one is
[`examples/receivers/python-sqlite/`](../../examples/receivers/python-sqlite/).

To run the existing backend, follow [Server setup](../setup.md). You do not
need to implement this protocol to use the app. To build a receiver, start with
[the checklist](#11-minimal-conformant-receiver-checklist), then use the schemas
and fixtures below to validate it. Reading synced data is a separate interface:
the [product API](../api.md).

| Artifact | Where |
|---|---|
| This specification | `docs/protocol/README.md` |
| JSON Schema (draft 2020-12) for the header and every line type | [`schema/`](schema/) |
| Type vocabulary: every syncable type with its identifier, kind, canonical unit, aggregation style and minimum iOS, rendered from the Swift catalog | [`catalog.json`](catalog.json), explained in [`catalog.md`](catalog.md) |
| Fixture corpus: known-good batches with the counts a reference server returns | [`fixtures/`](fixtures/) |
| Checker: validates any batch against the schemas and the framing rules | [`tools/protocol-check/`](../../tools/protocol-check/) |
| Reference receiver (Python, SQLite, standard library) plus a smoke test that runs the corpus against any receiver URL | [`examples/receivers/python-sqlite/`](../../examples/receivers/python-sqlite/) |

**Status.** Version 1 describes what the app sends today. The normative text
is derived from the Swift models
([`PulsHealthSync/Sources/PulsHealthSync/Models/SyncModels.swift`](../../PulsHealthSync/Sources/PulsHealthSync/Models/SyncModels.swift)),
the NDJSON encoder and HTTP transport, and the Go parser
([`server/ingest/parse.go`](../../server/ingest/parse.go)) and store. Where
the two sides disagree, [section 12](#12-known-discrepancies-between-the-swift-models-and-the-go-parser)
says so rather than guessing. Gaps you hit while implementing a receiver
belong in a "Backend implementer question" issue; the answers become spec
text.

"MUST", "SHOULD" and "MAY" are used in the RFC 2119 sense. "The app" and
"the sender" mean the PulsHealth client; "the reference server" means
`server/ingest`.

## Contents

1. [Overview](#1-overview)
2. [Transport](#2-transport)
3. [The batch](#3-the-batch)
4. [Line types](#4-line-types)
5. [Type vocabulary and canonical units](#5-type-vocabulary-and-canonical-units)
6. [Idempotency and storage semantics](#6-idempotency-and-storage-semantics)
7. [Responses](#7-responses)
8. [Sender behaviour a receiver can rely on](#8-sender-behaviour-a-receiver-can-rely-on)
9. [Optional read endpoints](#9-optional-read-endpoints)
10. [Versioning](#10-versioning)
11. [Minimal conformant receiver checklist](#11-minimal-conformant-receiver-checklist)
12. [Known discrepancies between the Swift models and the Go parser](#12-known-discrepancies-between-the-swift-models-and-the-go-parser)

## 1. Overview

The app reads HealthKit and uploads **batches** to one HTTP endpoint,
`POST {base}/v1/batches`. A batch is a gzip-compressed NDJSON document: a
header line declaring how many lines of each type follow, then those lines in
a fixed order. The receiver stores what it wants and answers with any 2xx
status. That 2xx is the whole contract: only after receiving it does the app
persist the HealthKit cursor (anchor or watermark) that produced the batch,
so a crash or a failed upload re-sends the same data, and every write the
protocol defines is idempotent so a re-send is harmless.

There are four categories of data on the wire:

| Category | Identity | Receiver rule |
|---|---|---|
| **Samples** (quantity, category, workout, heartbeat series, ECG, State of Mind, medication dose) and their **deletions** | HealthKit UUID | insert if unknown, never overwrite; delete by UUID |
| **Workout route and series points** | (workout UUID, timestamp[, type]) | insert if unknown |
| **Aggregate buckets** and **activity summaries** | a composite key, no UUID | upsert; each line is the whole row, so a `null` (or, on an activity summary, an omitted field) clears |
| **Profile** | the batch's user | replace the whole snapshot |

A receiver that implements only `POST /v1/batches` is conformant. The read
endpoints in [section 9](#9-optional-read-endpoints) back optional app
screens (server-side counts, reconciliation, connection testing).

## 2. Transport

### 2.1 Endpoint

`POST {base}/v1/batches`, where `{base}` is the URL the user enters in the
app. `v1/batches` is appended to the base URL's path, so a base of
`https://health.example.net/puls` yields
`https://health.example.net/puls/v1/batches`. The app requires `https` except
for hosts on the local network, where plain `http` is allowed.

### 2.2 Request headers

| Header | Value | Required | Notes |
|---|---|---|---|
| `Authorization` | `Bearer <token>` | yes | A bearer token. A receiver MAY accept one shared token, per-device tokens it issues itself, or both (the reference server does both — `server/README.md`, "Tokens"). Compare in constant time, or by hash lookup. Wrong, missing or revoked: **401**. |
| `Content-Type` | `application/x-ndjson` | sent always | Receivers SHOULD NOT reject other values. |
| `Content-Encoding` | `gzip` | sent always | Receivers MUST accept `gzip`; the reference server also accepts an absent or `identity` encoding (a plain body) and rejects anything else with 400. |
| `X-Puls-Protocol` | `1` | sent by versioned clients | Absent on clients that predate versioning: treat as `1`. Any other value, including a non-integer, is refused with the fixed body in [7.3](#73-unsupported-protocol-version) **before the body is read**. Sent on every request, reads included. |
| `X-Batch-ID` | the header line's `batchID` | sent always | Convenience copy for logging and dedup before parsing. The reference server only logs a mismatch with the body. |
| `X-User-ID` | a UUID | sent always | The user every row in the batch belongs to. Absent: the default user `5ea4d000-0000-4000-8000-000000000001`. Malformed: **400**. See [2.4](#24-user-identity). |
| `X-Wake-ID` | a UUID | optional | The iOS wake (background delivery, scheduled task, foreground, manual) that produced the upload; joins server rows to the app's Background Activity export. Malformed: 400. |
| `X-Wake-Trigger` | `observer` \| `backgroundProcessing` \| `backgroundContinued` \| `foreground` \| `manual` | optional | What woke the app. Stored verbatim, never validated. |

`Content-Length` is always set; the app does not use chunked transfer
encoding.

### 2.3 Body framing

The body is one gzip member (RFC 1952) containing UTF-8 NDJSON: JSON objects
separated by `\n`, one object per line, no byte-order mark, no pretty
printing. The final line is newline-terminated. Receivers MUST tolerate blank
lines between objects (the reference server skips them) and MUST decode
incrementally or bound memory some other way: a batch can be tens of
megabytes decompressed. Receivers SHOULD also cap the **decompressed** size,
not only the request body, and answer **413** past it ([2.5](#25-size-limits)):
DEFLATE inflates up to about 1,000×, so a request well under any body limit
can expand to gigabytes. The cap applies to a plain (`identity`) body too.

The app produces gzip with its own framing (10-byte header, raw DEFLATE,
CRC-32 and length trailer) so any standard gzip decoder reads it, and gzip
typically shrinks health-sample JSON about 10×.

### 2.4 User identity

Authentication proves access to the receiver; `X-User-ID` selects the
dataset. A receiver MUST scope every write and read by it, MUST create the
user on first sight, and MUST NOT let one user's deletions touch another's
rows. The app's default user ID is a fixed UUID so a reinstall keeps its
identity; households set distinct IDs per phone. A receiver MAY bind a
token to a user, in which case `X-User-ID` MUST be absent or equal to that
user and any other value is **403** ([7.2](#72-errors)); the reference server
does this for the per-device tokens it issues and not for its shared token.
How tokens are issued and bound is the receiver's business, not the
protocol's, so it does not move `schemaVersion`.

### 2.5 Size limits

What the reference server enforces; a receiver MAY choose different limits
but SHOULD answer over-limit input with **413**, which the app does not retry.

| Limit | Value |
|---|---|
| Compressed request body | 8 MiB |
| Decompressed NDJSON | 64 MiB (enforced while inflating, so a larger body is never held in memory) |
| One NDJSON line | 4 MiB (real maxima: ECG voltage arrays and 4,000-point route lines, about 400 KB) |
| Any single header count (`sampleCount`, …) | 5,000 |
| All header counts combined | 5,000 business lines, excluding the header |
| Points per route or series line | 4,000 (the sender chunks longer streams) |
| Route points per batch, series points per batch | 100,000 each |

Personal device credentials bind the member, Apple Health source connection,
and device generation in the database. Client device names, install IDs and
body fields cannot choose that binding. Re-pairing to the sole active personal
source preserves its identity and creates a new device generation. Multiple
possible sources require an explicit trusted pairing choice; they are not
merged. The protected local source fingerprint identifies this local binding,
not a verified Apple account ID. Pre-migration credentials remain unbound and
must be re-enrolled before personal uploads can commit.
| Time to accept one batch | the sender gives up after 60 s ([8.1](#81-retry-contract)); the reference server bounds its own database work at 5 minutes and finishes the commit even if the client disconnects |

## 3. The batch

### 3.1 Line order

```
1        header                          (always)
2…       sample lines                    sampleCount
         deletion lines                  deletionCount
         route lines                     routeCount
         series lines                    seriesCount
         aggregate lines                 aggregateCount
         activity-summary lines          activitySummaryCount
         profile line                    profileCount (0 or 1)
```

The counts are exact. Fewer lines than declared, more lines than declared, or
a line of the wrong type in a slot is malformed input (**400**). A count that
is absent from the header is 0.

### 3.2 The header

Schema: [`schema/header.schema.json`](schema/header.schema.json).

| Field | Type | Required | Meaning |
|---|---|---|---|
| `schemaVersion` | integer | v1 clients | Protocol version, `1`. Absent (or `null` on the reference server) means a client that predates versioning; read it as 1. An unsupported value is refused with the fixed body in [7.3](#73-unsupported-protocol-version), checked before any other header field. When both `X-Puls-Protocol` and `schemaVersion` are present they MUST agree. |
| `clientVersion` | string | v1 clients | Free text identifying the sender, e.g. `0.1.0 (57)`. Diagnostic only; log it, do not parse it. |
| `timeZoneID` | string | optional | Current device IANA zone (for example `America/Los_Angeles`), at most 100 characters. Absent or empty preserves legacy behavior. The reference server validates nonempty names and refuses `Local`; it initializes the calendar zone only for newly approved hosted accounts, never overwriting an established zone on travel or later uploads. Receivers may ignore it. |
| `batchID` | UUID | yes | Stable across retries of the same upload. A receiver MAY key replay detection on it ([6.1](#61-batch-replay)). |
| `deviceID` | string | yes | Opaque, stable per app install. |
| `type` | string | yes | A label: the type identifier that contributed the most samples, or, for the connection probe ([8.3](#83-connection-probe)), the literal string `probe`, which is not a type identifier. Any non-empty string is valid. Receivers MUST NOT derive the types of the lines from it; every line names its own type. |
| `reason` | `backfill` \| `incremental` \| `manual` \| `reconciliation` | yes | What kind of sync produced the batch. `backfill` is the initial history pass; `incremental` follows a HealthKit change notification or a scheduled catch-up; `manual` is a user tap or the connection probe; `reconciliation` re-sends samples the digest comparison found missing. |
| `exportedAt` | epoch ms | yes | When the sender serialized the batch. |
| `sampleCount`, `deletionCount` | integer ≥ 0 | yes | Line counts. |
| `routeCount`, `seriesCount`, `aggregateCount`, `activitySummaryCount` | integer ≥ 0 | no (0) | Line counts. Absent on older clients. |
| `profileCount` | 0 or 1 | no (0) | Whether a profile line ends the batch. |

Fields a receiver does not recognise MUST be ignored. That is what lets a
newer app talk to an older receiver within a version.

### 3.3 Telling lines apart

Every line after the header is a JSON object. Six line types are **wrapped**:
the object has exactly one top-level key naming the type (`deleted`, `route`,
`series`, `aggregate`, `activitySummary`, `profile`) whose value is the
payload object. A **sample** line is bare: its fields sit at the top level and
it carries none of the wrapper keys. A line whose wrapper key the receiver
does not recognise is malformed input (400), which is how the reference
server behaves; see [section 10](#10-versioning) for what that implies.

Within a line, unknown fields MUST be ignored.

### 3.4 Conventions

- **Timestamps are epoch milliseconds**: JSON numbers, never ISO 8601. The
  app's are usually **fractional** (`1718000000123.456`: HealthKit dates carry
  sub-millisecond precision and the app encodes them as a `Double`), so a
  receiver MUST accept a fraction wherever a timestamp appears — `start`,
  `end`, `t`, `bucketStart`, `exportedAt`, metadata dates — and MAY round it to
  its storage precision (the reference server keeps microseconds). An integer
  is equally valid. The accepted range is `-62135596800000` (0001-01-01) to
  `253402300799999` (9999-12-31T23:59:59.999Z); anything outside is a 400 on
  the reference server rather than a garbage-but-valid timestamp. Where a
  timestamp is required, `0` counts as absent (`start`, `t`, `bucketStart`,
  `date`). Instants are UTC; the optional `…Context` objects say what zone
  the phone was in.
- **Temporal context** (`startContext`, `endContext`, `temporalContext`,
  `bucketStartContext`, `bucketEndContext`, `scheduledAtContext`): an object
  `{"timeZoneID": "Europe/Berlin", "utcOffsetSeconds": 7200, "source":
  "device_current", "confidence": "inferred", "tzdbVersion": "2024a"}`. The
  first four are required when the object is present; `tzdbVersion` may be
  empty. It never changes the instant; it lets a receiver reconstruct local
  wall time. Older clients omit it.
- **UUIDs** are the canonical 36-character `8-4-4-4-12` form. The app sends
  them upper-case (Swift's `UUID` encoding); other senders may not, and a
  receiver's own storage may hand them back lower-case. Receivers MUST compare case-insensitively — sample identity, deletions,
  `workoutUUID`, `batchID` — and SHOULD normalise to one case before storing or
  keying on a UUID. Fixture 01 stores a sample under an upper-case UUID that
  fixtures 04 and 05 name in lower and mixed case.
- **Optional fields** may be absent or `null` with the same meaning, except
  where a field's presence is itself meaningful ([4.5](#45-aggregate),
  [4.7](#47-profile)). The app omits absent optionals; older builds sent
  explicit `null`s (`"category":null,"workout":null` on a quantity sample),
  and the fixtures keep one such line. On the lines a receiver upserts
  ([4.6](#46-activity-summary)) "the same meaning" includes overwriting: an
  omitted field clears the stored value exactly as `null` does.
- **Metadata** (`metadata` on samples and workout events) is an object of
  JSON scalars: strings, numbers, booleans. HealthKit dates become epoch-ms
  numbers and `HKQuantity` values become plain numbers, so a receiver cannot
  tell them from other numbers.
- **Units.** Every quantity is converted to one canonical unit per type before
  it leaves the phone ([section 5](#5-type-vocabulary-and-canonical-units)).
  The `unit` string on a line is informational; receivers MUST NOT expect
  device units.
- **Type identifiers** are HealthKit's (`HKQuantityTypeIdentifierHeartRate`,
  `HKCategoryTypeIdentifierSleepAnalysis`, …) plus a few synthetic ones the
  protocol defines: `HKWorkoutTypeIdentifier`, `HKActivitySummaryTypeIdentifier`
  (never on a sample line; used in `/v1/stats` and as a batch label),
  `HKDataTypeIdentifierHeartbeatSeries`, `HKDataTypeIdentifierElectrocardiogram`,
  `HKDataTypeIdentifierStateOfMind`,
  `HKMedicationDoseEventTypeIdentifierMedicationDoseEvent`.

## 4. Line types

### 4.1 Sample

Schema: [`schema/sample.schema.json`](schema/sample.schema.json).

```json
{"uuid":"11111111-1111-4111-8111-111111111111","type":"HKQuantityTypeIdentifierHeartRate","kind":"quantity","start":1718000000000,"end":1718000005000,"value":62.5,"unit":"count/min","sourceName":"Apple Watch","sourceBundleID":"com.apple.health","sourceVersion":"10.0","device":"Apple Watch","metadata":{"HKMetadataKeyHeartRateMotionContext":1}}
```

Common fields:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `uuid` | UUID | yes | `HKObject.uuid`; the sample's identity. |
| `type` | string | yes | Type identifier. |
| `kind` | enum | yes | `quantity`, `category`, `workout`, `heartbeatSeries`, `ecg`, `stateOfMind`, `medicationDose`. Selects the detail object below. Any other value is a 400. |
| `start`, `end` | epoch ms | `start` | `end` absent or 0 means equal to `start`. |
| `startContext`, `endContext` | temporal context | no | |
| `value`, `unit` | number, string | no | Quantity samples: the value in the canonical unit and that unit's string. |
| `category` | integer | for `category` | Raw `HKCategorySample.value`, e.g. sleep stage. The reference server stores 16 bits. |
| `sourceName`, `sourceBundleID`, `sourceVersion` | string | no | `HKSource` name, bundle identifier, and version of the app that wrote the sample. |
| `device` | string | no | `HKDevice.name`, e.g. `Apple Watch`, `iPhone`. |
| `metadata` | object | no | See [3.4](#34-conventions). |
| `workout`, `heartbeats`, `ecg`, `stateOfMind`, `medicationDose` | object / array | per kind | Exactly the one matching `kind` MUST be present and non-null; a missing detail object is a 400. |

Per-kind detail:

**`quantity`** carries `value` and `unit`. The app always sets both; the
reference server tolerates their absence and stores NULL.

**`category`** carries `category`. The meaning of the integer is HealthKit's
per-type enumeration (for `HKCategoryTypeIdentifierSleepAnalysis`: 0 in bed,
1 asleep unspecified, 2 awake, 3 core, 4 deep, 5 REM).

**`workout`** carries a `workout` object:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `activityType` | string | yes | A stable snake_case name: `running`, `walking`, `cycling`, `hiking`, `swimming`, `strength_training`, `functional_strength_training`, `hiit`, `yoga`, `pilates`, `rowing`, `elliptical`, `stair_climbing`, `core_training`, `cross_training`, `flexibility`, `mixed_cardio`, `dance`, `tennis`, `basketball`, `soccer`, `golf`, `skating`, `snow_sports`, `surfing`, `paddle_sports`, `climbing`, and others following the same pattern. Receivers MUST accept unknown names. |
| `duration` | number | yes | Active seconds (pauses excluded). |
| `totalEnergyKcal`, `totalDistanceMeters` | number | no | Totals. |
| `statistics` | object | no | `{typeIdentifier: number}`: one representative value per quantity type, kept for older receivers. |
| `statisticsDetail` | object | no | `{typeIdentifier: {min, avg, max, sum}}` in canonical units; cumulative types carry `sum`, discrete types carry `min`/`avg`/`max`. |
| `events` | array | no | `[{type, start, end?, startContext?, endContext?, metadata?}]` in order. `type` is `pause`, `resume`, `lap`, `marker`, `motionPaused`, `motionResumed`, `segment`, or another HealthKit event name; `end` is set for spans (segments, laps). |
| `activities` | array | no | Sub-activities of a multi-sport or interval workout: `[{activityType, start, end?, startContext?, endContext?, duration, statistics?}]` where `statistics` has the `statisticsDetail` shape. |

GPS routes and intra-workout curves for a workout do not ride inside the
sample; they follow as [route](#43-route) and [series](#44-series) lines that
reference the workout's UUID, possibly in later batches.

**`heartbeatSeries`** carries `heartbeats`: an array (possibly empty) of
two-element arrays `[secondsSinceSeriesStart, precededByGap]`, a number and a
boolean. Any other arity is a 400.

**`ecg`** carries an `ecg` object: `classification` (string:
`sinusRhythm`, `atrialFibrillation`, `inconclusiveLowHeartRate`,
`inconclusiveHighHeartRate`, `inconclusivePoorReading`, `inconclusiveOther`,
`unrecognized`, `notSet`, or `classification_<n>` for values newer than the
sender), `averageHeartRateBpm` and `samplingFrequencyHz` (numbers, optional),
`symptomsStatus` (`notSet` \| `none` \| `present`), and `voltagesUV` (array of
numbers: the lead I trace in microvolts, one entry per measurement, typically
15,000 for a 30-second recording at 512 Hz).

**`stateOfMind`** (iOS 18) carries a `stateOfMind` object: `kind`
(`momentaryEmotion` \| `dailyMood`), `valence` (number, −1 very unpleasant to
+1 very pleasant), `valenceClassification` (`veryUnpleasant`, `unpleasant`,
`slightlyUnpleasant`, `neutral`, `slightlyPleasant`, `pleasant`,
`veryPleasant`), `labels` and `associations` (arrays of strings naming
HealthKit's label and association cases).

**`medicationDose`** (iOS 26) carries a `medicationDose` object:
`medication` (string or null: the display name when resolvable),
`medicationID` (optional UUID string or null), `status`
(`taken`, `skipped`, `snoozed`, `notLogged`, or another HealthKit log status
name), `scheduledAt` (epoch ms or null), `scheduledAtContext`,
`doseQuantity` (number or null) and `doseUnit` (string or null, the dose's own
unit, not a catalog unit).

`medication` is a display label, **not an identity key**. Senders resolve it by
matching the dose's opaque HealthKit concept identifier against authorized
medication concepts (including archived medications). If unavailable, send null;
an Objective-C object description or memory address is not a medication name.
Different concepts may share a name, and names may change. `medicationID` is a
random UUID assigned by the sender by comparing opaque HealthKit concept
identifiers for equality. It groups doses only while that device's local identity
map survives; it is not a cross-device or reinstall-stable ID and cannot be
reconstructed from a display name. Older senders omit it. This does not provide
strength conversion or a per-tablet mass. A dose in
`count` must not be treated as grams or milligrams.


### 4.2 Deletion

Schema: [`schema/deletion.schema.json`](schema/deletion.schema.json).

```json
{"deleted":{"uuid":"44444444-4444-4444-8444-444444444444","type":"HKQuantityTypeIdentifierHeartRate"}}
```

A HealthKit deletion tombstone, reported by the same anchored query that
delivers new samples. Both fields are required. Semantics in
[6.3](#63-deletions).

### 4.3 Route

Schema: [`schema/route.schema.json`](schema/route.schema.json).

```json
{"route":{"workoutUUID":"33333333-3333-4333-8333-333333333333","points":[{"t":1718000001000,"lat":37.3349,"lon":-122.009,"alt":12.5,"hAcc":3.2,"vAcc":4.1,"speed":2.8,"course":181.0}]}}
```

GPS fixes for one workout. `workoutUUID` (required) names the workout sample;
`points` (required, at most 4,000 per line) carry `t` (epoch ms, required),
`lat` and `lon` (degrees, required), and optional `alt` (metres), `hAcc`,
`vAcc` (metres), `speed` (m/s), `course` (degrees) and `temporalContext`.
Optional fields are absent or null when Core Location reported them invalid.
A route longer than 4,000 points is split across several lines, and the
lines of one workout MAY arrive in different batches (the app back-fills
workouts in phases: the workout summary in the raw phase, routes and series
in the last two phases — see §8.2). The app sends the workout sample before
its route lines today, but a receiver SHOULD NOT depend on the workout row
existing when its points arrive.

### 4.4 Series

Schema: [`schema/series.schema.json`](schema/series.schema.json).

```json
{"series":{"workoutUUID":"33333333-3333-4333-8333-333333333333","type":"HKQuantityTypeIdentifierHeartRate","unit":"count/min","points":[{"t":1718000001000,"value":120.0},{"t":1718000002000,"value":135.5}]}}
```

An intra-workout time series for one quantity type (heart rate, running
power, cadence, speed, altitude and similar), in that type's canonical unit.
`workoutUUID`, `type` and `points` are required; `unit` is informational;
each point carries `t` (epoch ms, required), `value` (number, required) and an
optional `temporalContext`. At most 4,000 points per line; longer streams are
split, possibly across batches.

### 4.5 Aggregate

Schema: [`schema/aggregate.schema.json`](schema/aggregate.schema.json).

```json
{"aggregate":{"type":"HKQuantityTypeIdentifierHeartRate","func":"average","intervalValue":1,"intervalUnit":"hour","deviceFilter":"watch","bucketStart":1718000000000,"bucketEnd":1718003600000,"value":62.4,"unit":"count/min"}}
```

One bucket of an on-device statistics series (`HKStatisticsCollectionQuery`),
which the user configures per quantity type. Buckets have no UUID; the
receiver upserts on the identity in [6.4](#64-aggregate-buckets).

| Field | Type | Required | Meaning |
|---|---|---|---|
| `type` | string | yes | Quantity type identifier. |
| `func` | `sum` \| `average` \| `min` \| `max` \| `mostRecent` \| `duration` | yes | The statistic. Anything else is a 400. |
| `intervalValue` | integer ≥ 1 | yes | With `intervalUnit`: the bucket width, e.g. 1 hour, 7 days. |
| `intervalUnit` | `minute` \| `hour` \| `day` \| `week` \| `month` | yes | Buckets are computed in the phone's calendar, so day and month buckets survive daylight-saving changes and are not fixed-length. |
| `deviceFilter` | `all` \| `watch` \| `iphone` | yes | Which devices' samples were counted. |
| `bucketStart`, `bucketEnd` | epoch ms | yes | Half-open `[bucketStart, bucketEnd)`; `bucketEnd` MUST be after `bucketStart`. |
| `bucketStartContext`, `bucketEndContext` | temporal context | no | |
| `value` | number or **null** | yes (key present) | The statistic in the canonical unit, or `null` for "no samples in this bucket". The sender always writes the key; a receiver treats an absent key like `null`. |
| `unit` | string | no | The type's canonical unit, or `s` (seconds) for `func` = `duration`. |

The sender recomputes a trailing window on every run (late Apple Watch data
changes recent buckets) and a full pass roughly monthly, so the same bucket
arrives many times with different values, and an empty recompute arrives as
`null` to clear a stale value.

### 4.6 Activity summary

Schema: [`schema/activity-summary.schema.json`](schema/activity-summary.schema.json).

```json
{"activitySummary":{"date":1718000000000,"localDate":"2024-06-10","temporalContext":{"timeZoneID":"America/Los_Angeles","utcOffsetSeconds":-25200,"source":"device_current","confidence":"inferred","tzdbVersion":""},"moveKcal":420.5,"moveGoalKcal":600,"exerciseMin":25,"exerciseGoalMin":30,"standHours":9,"standGoalHours":12,"moveMode":0}}
```

One day of activity rings (`HKActivitySummary`). Not a sample: no UUID, one
per local calendar day, and the current day changes all day, so it is
re-sent on every run and upserted ([6.5](#65-activity-summaries)).

**The line is the complete row for its day.** The app writes only the fields
it has a value for: in the example, `moveTimeMin` and `moveTimeGoalMin` are
absent because the Move ring is in kilocalories, and a ring HealthKit reports
no value for is absent too, never `null`. (A line with explicit `null`s, as
in fixture 03, means the same.) A receiver MUST store the line as the
whole row: every ring field it stores takes the line's value, and an absent
field sets that column to null. A receiver MUST NOT merge only the keys
present into the stored row: that keeps a value the phone no longer reports.
Fixture [`08-activity-summary-replace`](fixtures/08-activity-summary-replace.ndjson)
re-sends a day fixture 03 stored, with three rings omitted, and its
`expected.json` states the row that must result.

| Field | Type | Required | Meaning |
|---|---|---|---|
| `date` | epoch ms | yes | The start of the local calendar day as an instant. |
| `localDate` | `YYYY-MM-DD` | no, but the app sends it | The calendar day in the phone's zone. **This is the day's identity**; receivers MUST prefer it over `date`. Absent only on older clients. |
| `temporalContext` | temporal context | no | |
| `moveKcal`, `moveGoalKcal` | number or null | no | Move ring and goal in kilocalories (when `moveMode` is 0). |
| `exerciseMin`, `exerciseGoalMin` | number or null | no | Exercise ring and goal in minutes. |
| `standHours`, `standGoalHours` | number or null | no | Stand ring and goal in hours. |
| `moveMode` | 0, 1 or null | no | 0: the Move ring is active energy (`moveKcal`); 1: the Move ring is move minutes (`moveTimeMin`, wheelchair and move-time users). Other values are a 400. |
| `moveTimeMin`, `moveTimeGoalMin` | number or null | no | Populated only for `moveMode` 1. |

### 4.7 Profile

Schema: [`schema/profile.schema.json`](schema/profile.schema.json).

```json
{"profile":{"name":null,"email":null,"dateOfBirth":631152000000,"biologicalSex":"male"}}
```

The batch user's complete identity snapshot: `name` and `email` (strings or
null; whatever the user typed into the app's settings), `dateOfBirth` (epoch
ms or null; the HealthKit date of birth) and `biologicalSex` (`female` \|
`male` \| `other` \| null). The sender writes all four keys explicitly. The
line **replaces** what is stored: a null or absent field clears that value,
`{"profile":{}}` clears all four, and the only way to leave the profile
unchanged is to send no profile line. `{"profile":null}` and a line without
the wrapper are malformed (400). The app attaches a profile line to the next
upload after the user edits these settings, and sends none while it has no
profile to report — an empty one goes out only when the user has just emptied
a filled one, so a reinstall pairing with its old server does not erase what
is stored there. Date of birth and sex are what a receiver needs for
heart-rate zones.

## 5. Type vocabulary and canonical units

Every quantity type has exactly one unit on the wire, converted on the phone.
The machine-readable vocabulary is [`catalog.json`](catalog.json) — one entry
per syncable type with its identifier, kind, canonical unit, HealthKit
aggregation style, the aggregate functions it allows and the first iOS release
that carries it; [`catalog.md`](catalog.md) documents the fields. It is
rendered from `HealthTypeCatalog` in
[`HealthTypeCatalog.swift`](../../PulsHealthSync/Sources/PulsHealthSync/Models/HealthTypeCatalog.swift),
the authoritative definition, and a package test fails whenever the two
disagree, so read the JSON rather than the Swift when you need the list.
Unit strings are HealthKit `HKUnit` strings. Two are easy to misread: `%` is
HealthKit's percent unit, whose scalar is a **fraction** (blood oxygen 0.97,
not 97), and `count/min` is beats or breaths per minute. `s` appears only on
`duration` aggregates — and since a type's first line may be an aggregate
(§8.2), `s` can be the first unit a receiver ever sees for a type whose
canonical unit is `min`. Take the canonical unit from this table, not from
whichever line arrived first. The table below is a reading aid grouped by
unit; `catalog.json` is the copy to trust.

| Type identifier (`HKQuantityTypeIdentifier…`) | Unit |
|---|---|
| `StepCount`, `FlightsClimbed`, `SwimmingStrokeCount`, `NumberOfTimesFallen`, `UVExposure`, `BodyMassIndex` | `count` |
| `DistanceWalkingRunning`, `DistanceCycling`, `DistanceSwimming`, `WalkingStepLength`, `RunningStrideLength`, `Height`, `WaistCircumference` | `m` |
| `ActiveEnergyBurned`, `BasalEnergyBurned`, `DietaryEnergyConsumed` | `kcal` |
| `AppleExerciseTime`, `AppleStandTime`, `AppleMoveTime`, `TimeInDaylight` | `min` |
| `WalkingSpeed`, `RunningSpeed`, `CyclingSpeed` | `m/s` |
| `WalkingDoubleSupportPercentage`, `WalkingAsymmetryPercentage`, `AtrialFibrillationBurden`, `PeripheralPerfusionIndex`, `BodyFatPercentage`, `OxygenSaturation`, `BloodAlcoholContent` | `%` (fraction) |
| `RunningPower`, `CyclingPower` | `W` |
| `RunningGroundContactTime`, `HeartRateVariabilitySDNN`, `HeartRateVariabilityRMSSD` | `ms` |
| `RunningVerticalOscillation` | `cm` |
| `CyclingCadence`, `HeartRate`, `RestingHeartRate`, `WalkingHeartRateAverage`, `HeartRateRecoveryOneMinute`, `RespiratoryRate` | `count/min` |
| `VO2Max` | `ml/kg*min` |
| `PhysicalEffort` | `kcal/hr*kg` |
| `BodyMass`, `LeanBodyMass` | `kg` |
| `BodyTemperature`, `BasalBodyTemperature`, `AppleSleepingWristTemperature` | `degC` |
| `BloodPressureSystolic`, `BloodPressureDiastolic` | `mmHg` |
| `BloodGlucose` | `mg/dL` |
| `EnvironmentalAudioExposure`, `HeadphoneAudioExposure`, `EnvironmentalSoundReduction` | `dBASPL` |
| `DietaryProtein`, `DietaryCarbohydrates`, `DietaryFatTotal`, `DietaryFiber`, `DietarySugar` | `g` |
| `DietarySodium`, `DietaryCaffeine` | `mg` |
| `DietaryWater` | `mL` |

Category types (`HKCategoryTypeIdentifier…`: `SleepAnalysis`,
`AppleStandHour`, `MindfulSession`, `HighHeartRateEvent`, `LowHeartRateEvent`,
`IrregularHeartRhythmEvent`, `LowCardioFitnessEvent`, `HandwashingEvent`,
`ToothbrushingEvent`, `AudioExposureEvent`, `HeadphoneAudioExposureEvent`, and
`SleepApneaEvent` on iOS 18) have no unit; their `category` integer is
HealthKit's per-type enumeration. `AudioExposureEvent` is loud-environment
events: HealthKit renamed the *constant* to
`HKCategoryTypeIdentifierEnvironmentalAudioExposureEvent` in iOS 14 but kept
the original string, so that is the identifier on the wire — one reason to
read [`catalog.json`](catalog.json) rather than an Apple header. Workouts,
heartbeat series, ECGs, State of Mind and medication doses have no unit
either; their detail objects state units per field.

A receiver MUST accept type identifiers it has never seen: the catalog grows
with iOS releases, and a receiver that rejects an unknown identifier stalls
the app for that type.

## 6. Idempotency and storage semantics

The app treats any 2xx as "durably stored" and never sends that page again
unless something failed, so a receiver MUST commit before answering. Because
retries and crashes re-send whole batches, and because reconciliation
deliberately re-sends samples, every rule below makes a re-send a no-op.

### 6.1 Batch replay

`batchID` is stable across retries of one upload. A receiver MAY record it
and short-circuit a replay. The reference server scopes it to the member and
server-assigned device generation, hashes the complete decompressed NDJSON
bytes, and commits the original result atomically with the data. An identical
retry returns that original result; a different body under the same key is
**409** and changes no health data. An old batch without a verifiable receipt
also fails closed rather than inventing one. A receiver that does not track
batch IDs is still conformant provided the per-line rules hold.

### 6.2 Samples

The UUID is the identity. If the UUID is already stored, the line is a
no-op: never an update, never a second row. The reference server's response
counts such lines as `duplicates`. The same UUID can legitimately arrive
under a different batch ID (a retried page after a crash, a reconciliation
re-send). A sample table keyed on UUID with insert-if-absent semantics is
sufficient.

### 6.3 Deletions

Remove the sample with that UUID, scoped to the batch's user. An unknown UUID
is a silent no-op, never an error. A deleted workout takes its route and
series points with it. Recording the tombstone is optional (the reference
server keeps a `deleted_samples` table). The reference server applies
deletions **after** every insert in the same batch, so a batch that both
inserts and deletes one UUID ends with it deleted; receivers SHOULD do the
same. HealthKit may purge tombstones before a sync sees them, which is what
the reconciliation endpoints in [section 9](#9-optional-read-endpoints)
repair.

### 6.4 Aggregate buckets

Identity: `(user, type, func, intervalValue, intervalUnit, deviceFilter,
bucketStart)`. Upsert: a later line overwrites `value`, `bucketEnd` and the
contexts, and a `null` value overwrites with null (the bucket is empty now).
When one batch carries the same bucket twice, the last line wins.

### 6.5 Activity summaries

Identity: `(user, localDate)`, falling back, only when `localDate` is
absent, to the calendar date of `date` shifted by the line's
`temporalContext.utcOffsetSeconds` if it carries one, else its UTC date
(older clients; see
[section 12](#12-known-discrepancies-between-the-swift-models-and-the-go-parser)
for why the fallback is imperfect). **Replace, not merge:** the line is the
whole day ([4.6](#46-activity-summary)), so an upsert overwrites every stored
column, setting each field that is `null` **or absent** to null. Last line
wins within a batch.

### 6.6 Route and series points

Identity: `(workoutUUID, t)` for route points and `(workoutUUID, type, t)`
for series points; insert-if-absent. Chunks of one stream may arrive in
several batches and in any order relative to each other. Resending an edited
series can add new timestamps, but existing values remain unchanged; omitting
previous points does not delete them. Content fingerprints in the client only
suppress unchanged uploads, and do not turn these records into replacements.
Receivers that need edit/removal support require a separate explicit repair
process; a full upload alone does not repair those changes.

### 6.7 Profile and users

The profile line replaces the user's snapshot ([4.7](#47-profile)). Users are
created on first sight of their `X-User-ID`, with an empty profile, before
any row is stored.

## 7. Responses

### 7.1 Success

Any **2xx** acknowledges the batch. The body is optional: the app reads
`accepted` and `duplicates` when they are present, for its own log and
per-type statistics, and treats an empty or unreadable body as no counts —
never as a failure. The reference server returns `200` with

```json
{"accepted":3,"deleted":0,"duplicates":0,"routePoints":0,"seriesPoints":0,"aggregateSamples":0,"activitySummaries":0}
```

| Count | Meaning on the reference server |
|---|---|
| `accepted` | sample lines that created a row |
| `duplicates` | sample lines whose UUID was already stored on the first committed application (`sampleCount − accepted`); an identical batch retry repeats the original counts |
| `deleted` | sample rows actually removed by deletion lines |
| `routePoints`, `seriesPoints` | points that created a row |
| `aggregateSamples`, `activitySummaries` | distinct buckets and days upserted (inserted or overwritten); a bucket or day named twice in one batch counts once |

Receivers that return a body SHOULD use this shape so the fixture corpus and
the smoke test can check them.

Counts describe the first committed application, not new writes made while
returning a cached receipt. The optional Puls v1 JSON count fields are unchanged.

### 7.2 Errors

Error bodies are JSON `{"error": "<message>"}`; the app shows the first 200
characters of the body in its log and on the type's detail screen, so a
specific message (`sample 3 (…): invalid kind "mystery"`) is worth more than
a generic one.

| Status | When | Retried by the app |
|---|---|---|
| 400 | Malformed input: bad JSON, header counts that do not match the lines, unknown `kind` or wrapper key, missing required field, malformed UUID, timestamp out of range, bad enum value, malformed `X-User-ID` or `X-Wake-ID`, bad gzip, unsupported `Content-Encoding`; and the fixed body of [7.3](#73-unsupported-protocol-version) | **no** |
| 401 | Missing, wrong or revoked bearer token | no |
| 403 | `X-User-ID` names a user other than the one the token is bound to ([2.4](#24-user-identity)) | no |
| 413 | Body or line over the receiver's limits | no |
| 429 | Receiver asks the app to back off | yes |
| 5xx | Anything transient: database down, deadlock, timeout, disk full | yes |

A non-429 4xx is terminal for that page: the app keeps its cursor and logs the
error. Current clients put terminal refusals into a cooldown before automatic
retries; a manual sync can retry immediately ([8.1](#81-retry-contract)).
**Answer 4xx only for input that is genuinely
malformed**, and answer transient failures with 5xx or 429 so the app's
backoff does its job. A receiver that cannot store a valid line it does not
understand SHOULD discard it and still return 2xx, not 400.

### 7.3 Unsupported protocol version

When `X-Puls-Protocol` or the header's `schemaVersion` names a version the
receiver does not speak, when either is not an integer, or when the two
disagree, respond **400** with exactly

```json
{"error":"unsupported protocol version","supportedVersions":[1]}
```

before doing any storage work (the reference server checks the request
header before reading the body, and `schemaVersion` before any other header
field). The app turns this body into a "server speaks a different protocol
version" message instead of a stall.

## 8. Sender behaviour a receiver can rely on

Derived from the current `HTTPSyncTransport` and sync engine. Retry scheduling
can change between app releases without changing protocol v1; durable
acknowledgment and idempotent storage remain the receiver contract.

### 8.1 Retry contract

- **Timeout**: 60 seconds per request, no waiting for connectivity. A
  receiver that has not answered by then will see the same batch again.
- **Attempts**: the first try plus up to 4 retries, waiting 2, 4, 8 and 16
  seconds (each multiplied by a random factor in 0.7–1.3; the ladder is
  capped at 30 seconds) before the next. The retry count is a transport parameter
  with default 4. During a background wake triggered by HealthKit
  (`X-Wake-Trigger: observer`) the app spends **one** retry and gives up,
  because the wake's execution budget is short; the next wake re-sends.
- **Server-directed delay**: current clients honor `Retry-After` on 429 and
  503 (seconds or an HTTP date). A delay above 60 seconds, or 10 seconds in an
  observer wake, ends that run instead of retrying early.
- **Retried**: 5xx, 429, and transient network errors such as connection
  refusal, reset or timeout. **Never retried within the run**: other 4xx,
  missing configuration, invalid URLs, ATS refusal or certificate errors
  that need a configuration fix.
- **Cooldown**: current clients pause automatic runs for a type after a
  terminal refusal (4xx except 408/429, incompatible protocol or terminal
  network error). The pause starts at 15 minutes and doubles to six hours.
  A manual sync bypasses it; an acknowledged upload or applied configuration
  change clears it. Older clients may retry on every wake.
- **Cursor**: the HealthKit anchor or watermark behind a batch advances only
  after a 2xx. Retries of an upload keep its payload; a later run can reread
  the unacknowledged page. Receivers must tolerate repeated sample UUIDs even
  when the new upload has a different `batchID`.

### 8.2 Batch shapes

- Backfill batches carry one type and up to 1,000 samples by default (user
  configurable 250–5,000). Incremental batches merge one page from each
  changed type into shared uploads, so a batch routinely mixes types; the
  header `type` is only the biggest contributor. A page is never split across
  batches.
- During backfill four types upload in parallel; the transport allows up to
  8 concurrent connections to one host. Receivers MUST cope with concurrent
  batches from one device and with the same type arriving out of
  chronological order across batches.
- A full run goes in phases, cheapest-useful first: activity summaries, then a
  bounded recent window of aggregates, then the raw samples, then the full
  aggregate pass, then workout routes, then workout series. Within the raw
  phase the heaviest type starts first and the rest run cheapest-first. None
  of this is part of the wire contract — it is described so receiver authors
  know what arrival order to expect — but the two consequences below are
  normative.
- **A type's first line MAY be an aggregate or activity-summary line rather
  than a sample line.** That is true of aggregate-only types (aggregates
  enabled for a type whose raw samples are not) and, because the recent window
  of aggregates runs before the raw samples, of every type with an aggregate
  configured on a first backfill. A receiver MUST therefore be able to
  register a type from an aggregate line, and MUST NOT assume the canonical
  unit from §5 has been established by an earlier sample line — a `duration`
  aggregate carries `s` whatever the type's own unit is, so a receiver that
  records the unit of whichever line arrives first MUST correct it when a
  sample line later supplies the canonical one.
- Workouts back-fill in phases: workout samples first, then route and series
  lines in later batches, chunked by point count. Route and series lines for
  a workout can therefore arrive minutes after the workout, and a receiver
  MUST NOT require the workout row to exist when its points arrive.
- Aggregate batches carry only aggregate lines; activity-summary batches
  carry only activity-summary lines plus, occasionally, a profile line.
  A receiver MUST NOT assume any particular combination.
- The largest single lines are ECGs (about 15,000 voltages, 200–400 KB) and
  4,000-point route or series lines (about 400 KB).

### 8.3 Connection probe

When the app tests a connection it calls `GET /v1/capabilities`
([9.1](#91-get-v1capabilities)). If that returns 404 or 405 it sends a
header-only batch instead: every count 0, `reason` `manual`, a fresh
`batchID`, `type` set to the string `probe` (`PulsProtocol.probeBatchType`),
which is not a type identifier — a receiver that registers a type from the
header label, or records the label per batch, should expect it. Any 2xx means the URL, the token, and the
upload path work; a 401 is a wrong token and a 403 a token bound to a user
other than the one the app is configured with ([2.4](#24-user-identity)).
Receivers MUST accept a header-only batch (fixture
[`06-empty-probe`](fixtures/06-empty-probe.ndjson)).

## 9. Optional read endpoints

All are bearer-authenticated, take the same `X-User-ID` header as uploads
(absent: the default user; malformed: 400), answer JSON, use epoch
milliseconds, and return `{"error": "..."}` on failure. None is required; the
app hides the corresponding screens when a receiver lacks them (a 404 is the
signal, and `features` in the capabilities body is the explicit one).

### 9.1 `GET /v1/capabilities`

```json
{"protocolVersions":[1],"features":["batches","stats","digest","uuids","aggregates","activitySummaries","routes","series","profile"],"server":"puls-ingest","version":"3f9c2a1"}
```

| Field | Meaning |
|---|---|
| `protocolVersions` | integers the receiver accepts in `X-Puls-Protocol` / `schemaVersion`. |
| `features` | what the receiver implements beyond accepting a batch. Names are protocol vocabulary and append-only: `batches` (`POST /v1/batches`; always present), `stats`, `digest`, `uuids` (the endpoints below exist), `aggregates`, `activitySummaries`, `routes`, `series`, `profile` (that line type is **stored**, not merely accepted). A receiver MUST accept every v1 line type regardless of what it advertises; `features` lets the app hide options whose data would be discarded. |
| `server`, `version` | free text naming the implementation and its build. |

The endpoint sits behind bearer auth on purpose: the app's "Test connection"
step validates URL and token together here, and a 401 is the earliest signal
of a mistyped token (a 403, of a user ID that does not match the token's
user). The minimal Python receiver advertises
`["batches","profile"]`.

### 9.2 `GET /v1/stats`

Per-type row counts and batch bookkeeping for the app's per-type sync detail,
so it can compare what it exported with what the receiver holds.

```json
[{"type":"HKQuantityTypeIdentifierHeartRate","rows":1834021,"earliest":1580515200000,"latest":1718000005000,"lastBatchAt":1718000400000,"batches":2103}]
```

`rows` and `batches` are integers; `earliest`, `latest` (sample start times)
and `lastBatchAt` are epoch ms or null. Types with no rows may be omitted.

### 9.3 `GET /v1/digest?type=&from=&to=`

Reconciliation, step one. `type` is a type identifier; `from` and `to` are
epoch-ms integers with `to > from`, selecting samples by start time in
`[from, to)`. Missing or malformed parameters are a 400. The response is one
entry per UTC calendar month that has rows, ascending:

```json
[{"window":1717200000000,"rows":88231,"digest":"3a0f…16 bytes as 32 hex characters"}]
```

`window` is the epoch ms of the first instant of the month (UTC); `digest`
is the byte-wise XOR of the raw 16-byte UUIDs of every sample in that month,
hex-encoded, which is order-independent so any storage can compute it. The
app computes the same digest from HealthKit and, for months that differ,
calls `uuids`. An unknown type returns `[]`.

### 9.4 `GET /v1/uuids?type=&from=&to=`

Reconciliation, step two: the sample UUIDs of one type in `[from, to)`,
with the same parameters as `digest`, limited to a 35-day range (larger: 400
`range exceeds 35 days`).

```json
{"uuids":["11111111-1111-4111-8111-111111111111","…"]}
```

The app uploads the samples HealthKit has that the receiver lacks (as a
`reconciliation` batch) and sends deletions for UUIDs the receiver has that
HealthKit lacks. Reconciliation covers quantity, category and workout kinds
only.

### 9.5 `GET /healthz`

Not part of the protocol, but every receiver in this repository answers
`{"ok":true}` without authentication for load balancers and the smoke test.

## 10. Versioning

- The protocol version is a small integer: `1` is this document. A batch
  carries it as `schemaVersion` in the header and every request carries it as
  `X-Puls-Protocol`; a receiver advertises what it speaks in
  `protocolVersions`.
- **Additive changes keep the number.** A change is additive when a receiver
  written against this document still accepts every batch a newer sender
  emits: new optional fields on any line, new values in free-form string
  fields (`activityType`, `classification`, `status`, metadata keys), new
  type identifiers, new count fields whose lines a v1 receiver will never be
  sent, new optional read endpoints, new `features` names. Receivers MUST
  ignore unknown fields precisely so that this works.
- **Incompatible changes bump the number**: removing or renaming a field,
  changing a unit, a timestamp convention, an identity key, the line order or
  the framing, and any change that makes a v1 receiver return 400 for a
  batch a newer sender emits. A new sample `kind` or a new wrapper key is in
  that class, because a v1 receiver rejects unknown ones; such additions ship
  either under a new version or as an opt-in capability feature that a sender
  uses only when the receiver advertises it.
- A sender that supports several versions picks the highest the receiver
  advertises; a receiver that does not implement capabilities is assumed to
  speak exactly version 1. A receiver MAY accept several versions and MUST
  refuse the rest with the body in [7.3](#73-unsupported-protocol-version).
- Within a version the JSON Schemas in [`schema/`](schema/) and the fixtures
  in [`fixtures/`](fixtures/) change only additively, in the same pull request
  as the code on both sides (see the "wire format changes touch both sides"
  invariant in [`CLAUDE.md`](../../CLAUDE.md)).

### Additive changes in v1

Version 1 was published with server 0.1.0 (2026-09-14). Everything below
arrived since without moving the number, because a receiver written against
the earlier text still accepts every batch; a receiver author should expect
all of it. Newest first, reconstructed from [`CHANGELOG.md`](../../CHANGELOG.md)
and the repository's history (which starts on 2026-09-21). Fields that predate
publication but are absent from pre-versioning clients (`schemaVersion`, `clientVersion`, the counts after
`deletionCount`, the `…Context` objects, `localDate`) are described where they
are defined and exercised by fixture `04`.

| Change | Since | What a receiver sees |
|---|---|---|
| Type identifier `HKQuantityTypeIdentifierHeartRateVariabilityRMSSD` (`ms`, iOS 27) | app release after 1.6 | Sample and aggregate lines for a type it has not seen; §5 requires accepting unknown identifiers. |
| A per-device token MAY be bound to a user, answering **403** when `X-User-ID` names another ([2.4](#24-user-identity)) | server 0.2.0 | Nothing on the wire. The app never retries a 403; from the release after 1.6 its connection test names it as a user mismatch. |
| Recent data first: each backfill sends the last 30 days of a type before its history, and the full sweep later sends those samples again | app 1.6 | The same sample UUIDs in two batches with different `batchID`s; insert-if-absent ([6.2](#62-samples)) makes the second a duplicate. |
| The profile line goes out only when non-empty, or when the user has just emptied a filled profile ([4.7](#47-profile)) | app 1.6 | Fewer `{"profile":{}}` lines; a reinstall no longer clears a stored profile. |

**Corrections to this document** (the app always behaved this way; earlier
text said otherwise):

- The connection probe's `type` is the string `probe`, not the heart-rate
  identifier ([8.3](#83-connection-probe)).
- Activity-summary rings the app has no value for are omitted, not sent as
  `null`, and an omitted ring overwrites the stored value with null
  ([4.6](#46-activity-summary), [6.5](#65-activity-summaries)).
- Timestamps are usually fractional and UUIDs upper-case
  ([3.4](#34-conventions)); fixtures 01, 02, 04, 05 and 08 now carry that
  shape.
- `aggregateSamples` and `activitySummaries` in the response count distinct
  buckets and days, as the reference server always has ([7.1](#71-success)).

## 11. Minimal conformant receiver checklist

A receiver is conformant when it:

- [ ] accepts `POST /v1/batches` with a bearer token, answering 401 for a
  wrong or missing one;
- [ ] decodes a gzip body (and, ideally, a plain one) as UTF-8 NDJSON,
  tolerating blank lines, bounding memory and capping the decompressed size
  (413 past it);
- [ ] honours `X-Puls-Protocol` and `schemaVersion`: absent means 1,
  unsupported or contradictory means the fixed 400 body of
  [7.3](#73-unsupported-protocol-version);
- [ ] reads the header, then exactly the declared number of lines of each
  type in wire order, and answers 400 for a count mismatch, a trailing line,
  an unknown wrapper key, an unknown `kind`, or a missing required field;
- [ ] ignores every field it does not recognise, and accepts every type
  identifier and every v1 line type, discarding what it does not store;
- [ ] scopes everything by `X-User-ID` (default user when absent, 400 when
  malformed) and creates users on first sight;
- [ ] accepts fractional epoch-ms timestamps everywhere;
- [ ] stores samples keyed on UUID with insert-if-absent semantics, and
  applies deletions by UUID as no-ops when unknown, comparing UUIDs
  case-insensitively;
- [ ] if it stores them: upserts aggregate buckets on
  `(user, type, func, intervalValue, intervalUnit, deviceFilter, bucketStart)`
  with `null` clearing the value, upserts activity summaries on
  `(user, localDate)` replacing the whole row (an absent field is null),
  inserts route and series
  points if absent, and replaces the profile snapshot;
- [ ] commits before answering, answers any 2xx on success, and answers
  transient failures with 5xx or 429, never with a 4xx;
- [ ] answers within 60 seconds, handles up to 8 concurrent uploads, and
  copes with lines of several hundred kilobytes;
- [ ] accepts a header-only batch as the connection probe;
- [ ] optionally serves `/v1/capabilities` (recommended: it makes "Test
  connection" precise), `/v1/stats`, `/v1/digest`, `/v1/uuids`.

Prove it: run `tools/protocol-check` on batches you capture, and run
`examples/receivers/python-sqlite/smoke_test.py --url <your receiver>
--token <token>` against an empty store. It posts the whole corpus, replays
every batch, and checks the negative cases above. The rows a fixture's
`state` names (the activity-summary replace rule) cannot be read back over
the protocol, so against `--url` it prints them for you to check in your
store.

## 12. Known discrepancies between the Swift models and the Go parser

Places where `SyncModels.swift` (what is sent) and `parse.go`/`store.go`
(what the reference server accepts and stores) differ. None breaks the
protocol; each is called out so a receiver author can choose deliberately.

| Topic | Swift sender | Go reference receiver | Consequence |
|---|---|---|---|
| `category` value | `Int` (64-bit) | `int16` | A value outside ±32,767 is a 400. HealthKit's enumerations are small; the schema states the 16-bit bound. |
| ECG `voltagesUV` | `[Double]` | `[]float32`, stored as single precision | About 7 significant digits survive; adequate for microvolt traces, but lossy. |
| Metadata dates | `MetadataValue.date` encodes as an epoch-ms number | `map[string]any` stored as JSON | Receivers cannot distinguish a date from a number. Go also accepts nested objects and arrays the app never emits. |
| Header fields | Always sends `deviceID`, `reason`, `exportedAt` and every count; its own decoder requires `routeCount` | Requires only `batchID` and `type`; `reason` is stored verbatim without validation | The schema requires what every client sends (`batchID`, `deviceID`, `type`, `reason`, `exportedAt`, `sampleCount`, `deletionCount`) and constrains `reason` to the four values. |
| `end` on samples | Always sent | Absent or 0 becomes `start` | Receivers SHOULD apply the same default. |
| `biologicalSex` | `female`, `male`, `other` or null | Any string accepted | The schema constrains the enum. |
| `SampleKind.activitySummary` | Exists in the enum so the catalog can list the rings | `validKind` rejects it | Never on the wire as a sample; activity summaries always ride their own line. |
| `tzdbVersion` | Always encoded, `""` when unknown | Optional | Treat empty and absent alike. |
| Explicit nulls | Current builds omit absent optionals, `ActivitySummaryRow`'s rings included; older builds sent `null`; `ProfilePayload` and `AggregateSampleRow.value` always write explicit nulls | Absent and `null` are treated alike everywhere; an activity summary's absent ring overwrites the stored column with NULL | Receivers MUST treat them alike, including when overwriting ([6.5](#65-activity-summaries)); senders SHOULD keep writing the explicit nulls where this document says the key is present. |
| `duration` aggregates' `unit` | Sends `s` | Registers the type with its catalog unit and ignores `s` | Store the unit per bucket if you need it. |
| Activity-summary day without `localDate` | Older clients sent only `date` (start of the local day) | Falls back to the date of `date` shifted by the line's `temporalContext.utcOffsetSeconds` when present, else its UTC date | Without an offset, east of UTC that is the previous day (a 00:00+02:00 instant is 22:00Z the day before). Current clients always send `localDate`; receivers MUST prefer it. |
| `X-Batch-ID` vs body `batchID` | Always equal | Only logged when different | Receivers MAY trust either; the body is authoritative. |
| Sample `value` on quantity kinds | Always set | Optional, stored NULL when absent | The schema leaves it optional to match the receiver; senders always send it. |
| Route point `lat`/`lon` bounds | Core Location values | Not range-checked | The schema does not add bounds either. |
