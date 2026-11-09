# PulsHealthSync

Swift package (iOS 17+, Swift 6 strict concurrency, no external dependencies) that
syncs HealthKit data to an HTTP ingest server, exports it to files, and profiles
what HealthKit holds. The PulsHealth app is a thin UI over it. The wire format is
the [Puls Sync Protocol](../docs/protocol/README.md); the invariants are in
[`CLAUDE.md`](../CLAUDE.md).

If you want to sync your own phone, use the [server setup guide](../docs/setup.md)
and the App Store app. This reference is for developers working on or embedding
the Swift package.

- [How a sync runs](#how-a-sync-runs): execution order, retries and background work.
- [Secrets and state](#secrets-and-state-at-rest): credentials and destination isolation.
- [On-device export](#on-device-export) and [exploration](#exploring-what-healthkit-holds): APIs that do not advance sync progress.
- [Testing](#testing): run the package checks.

## Family snapshot observations

For a family receiver, the HTTP transport obtains a source observation before
each aggregate chunk, activity-summary query or fresh settings-profile read.
The uploaded header carries that observation; a profile also carries its
source-side capture time. Workout and merged batches refresh one profile at
batch construction instead of attaching a new observation to cached values.
`InstrumentedTransport` forwards this handshake. File and dry-run transports
use the protocol's default nil result; HTTP 404/405 or `{}` retain legacy
receiver behavior. Other failures stop the pass.

Observed batches require a complete matching family receipt even if the
response omits the family marker. Unknown aggregate or ring reads do not
upload clears or advance progress. Existing readable-history limits remain
in force. See the [wire contract](../docs/protocol/README.md#source-query-observations).

## Source map

All paths below are under `Sources/PulsHealthSync/`. Read the section for the
behavior you are changing; [the invariants](../CLAUDE.md#invariants--do-not-break)
apply across these modules.

| Area | Responsibility | Start with |
|---|---|---|
| `Engine/` | Authorization, backfill, incremental uploads, aggregates, rings, background execution and reconciliation | `HealthSyncEngine.swift`, `MergedSync.swift`, `AggregateSync.swift` |
| `Anchors/` | Destination-scoped progress, credential storage and protected state files | `SyncStateStore.swift`, `ServerIdentity.swift`, `TokenStore.swift` |
| `Transport/` | Gzip HTTP uploads, retries, capabilities, URL validation and pairing | `SyncTransport.swift`, `ConnectionTest.swift`, `PairingPayload.swift` |
| `Models/` | Configuration, the authoritative type catalog, aggregate choices and wire DTOs | `SyncConfiguration.swift`, `HealthTypeCatalog.swift`, `SyncModels.swift` |
| `Serialization/` | HealthKit-to-wire conversion, canonical units, metadata and NDJSON encoding | `SampleMapper.swift`, `NDJSONEncoder.swift` |
| `Export/` | Isolated export engine, CSV/JSON/JSONL writers, manifests and ZIPs | `HealthExporter.swift`, `ExportPlan.swift`, `ExportModels.swift` |
| `Explore/` | Read-only scans, profiles, aggregate previews and bounded statistical summaries | `HealthExplorer.swift`, `TypeProfile.swift`, `AggregateQuery.swift` |
| `Metrics/` | Scrubbed event logs and background-wake telemetry | `SyncEventLog.swift`, `ErrorScrubber.swift`, `WakeLog.swift` |
| `Resources/` | Privacy manifest | `PrivacyInfo.xcprivacy` |

`WakeScope` carries each wake's context through nested tasks and uploads;
`X-Wake-ID` and `X-Wake-Trigger` join phone diagnostics to server batch logs.
`finishWake` is idempotent so expiration and completion cannot overwrite each
other's outcome.

`Tests/PulsHealthSyncTests/` covers serialization, protocol handling, catalog
consistency, state isolation, exports, pairing, history limits and statistics.
The app-hosted tests in `PulsHealth/HostedTests/` cover real HealthKit query
construction, including the aggregate option matrix. See [Testing](#testing).

## Secrets and state at rest

The bearer token is the one secret the package holds. It lives in the Keychain
(`KeychainTokenStore`, `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`, so
background wakes after a reboot can still build a transport and a backup never
restores it onto another device) and in memory on `SyncConfiguration.authToken`.
`SyncConfiguration.encode(to:)` never writes it, and `SyncStateStore` fills it
back in on load. The one exception: when a Keychain write fails, the store parks
the token in `sync-state.json` (`PersistedState.fallbackAuthToken`), retries on
the next launch, and removes it once the Keychain accepts it — dropping it would
stall every sync until the user retyped it. A state file from a build that kept
the token inline is migrated the same way. Pass an `InMemoryTokenStore` to
`SyncStateStore(directory:tokenStore:)` for tests and throwaway engines.

A configuration applied with a nil `authToken` **keeps** the stored token as
long as it names the same database URL and user ID, so a caller that rebuilds
a configuration without the token cannot delete the credential by accident.
Deleting it is explicit: `clearAuthToken()`, or
`configure(_:confirmServerIdentity:clearAuthToken:)` with `clearAuthToken: true`
(the app uses the latter to apply an emptied token field or Disconnect atomically). A configuration for a different database or
user without a token still drops the old one; a token is never sent to a
database that did not issue it. `setAuthToken(_:)` replaces the token alone.

`configure`, `setAuthToken`, and `clearAuthToken` return `Bool`: `false` means
the update was not applied (an active sync, overlapping change, unreadable state,
credential problem or failed persistence).
Callers must keep the existing applied settings and ask the user to retry once
the sync finishes. Parent passes hold this exclusion between phases too, so
an upload to the old destination cannot advance the new destination's anchors.
Changed aggregate series reset their watermarks inside the same configuration
commit. A nil token still preserves the credential unless explicitly cleared.

`sync-state.json`, `event-log.json`, `wake-log.json` and any quarantined copy are
written with `FileProtectionType.completeUntilFirstUserAuthentication` and
excluded from backup (`ProtectedStateFile`): anchors are opaque, device-specific
`HKQueryAnchor` blobs that mean nothing on another device. A file that exists
but cannot be read (protected data during a prewarm launch before first
unlock) is never taken for a missing one: the store, the event log and the
wake log each open read-only for that process (`isReadOnly`) and write
nothing over it.

A stored anchor that will not decode (a restored or downgraded state file) is
never inspected: that type's stream starts over — both anchors cleared and the
backfill reopened for the main anchor, the recent-window anchor alone for that
stream — and the event log says so. The server ignores what it already has.

Persisted progress is isolated per saved `ServerIdentity` (normalized host,
port, path and user ID). Each destination retains its configuration, raw and
recent anchors, aggregates, rings and workout-enrichment state. A new receiver
starts without another receiver's progress; switching back restores its own.
`switchDestination(id:)` and `unpairDestination(id:)` run under the same
configuration exclusion as uploads and throw when the transition cannot be
applied. Unpairing is local only: it does not delete data or revoke tokens on
the remote server. There is never more than one active destination.

Workout streams retain a SHA-256 content fingerprint per workout and metric in
that destination's protected sync state, after every chunk of the workout is
acknowledged. The seven-day lookback still queries HealthKit but skips unchanged
metrics; new or changed metrics upload normally. A monthly or forced full pass
clears the fingerprints before starting, allowing recovery of receiver data.
Resuming an interrupted full pass preserves fingerprints already acknowledged in
that pass. Resetting sync state clears them too. Routes retain their existing
upload behavior. These are upload acknowledgments, not a server-edit protocol:
series points are insert-if-absent, so retransmission adds new timestamps but
does not overwrite edited values or remove points missing from HealthKit.

The app shows saved receivers in Sync → Database. Normal credentials use
separate destination-scoped Keychain items. Write-failure fallbacks retain the
same protected-file and backup-exclusion rules described above. An upgrade
adopts the previous receiver and its progress without starting over.

Error text that is persisted or logged goes through `ErrorScrubber`: bearer and
basic credentials, URL query strings and the configured token are redacted,
control characters dropped, and `lastError` is capped at 120 characters. The
event log never names sample UUIDs. This covers every `TransportError`,
including a server's rejection body and the `unsupportedProtocol` message.
HTML server errors use a readable page title from the first 16 KiB instead of
logging page markup or request diagnostics. A missing or incomplete title has
an explicit fallback; plain-text and JSON error excerpts are unchanged.

## Wire protocol version

The [protocol spec](../docs/protocol/README.md) owns the format; this is what
the package does with it.

- **Versions.** The first NDJSON line of every batch carries
  `"schemaVersion": 1` (`PulsProtocol.version`) and
  `"clientVersion": "<marketing version> (<build>)"` (`"unknown"` when the
  host bundle has none), and every request carries `X-Puls-Protocol: 1`.
- **Rejection.** A 400 with
  `{"error":"unsupported protocol version","supportedVersions":[…]}` becomes
  `TransportError.unsupportedProtocol` (never retried), so the app can say the
  server does not support this app version rather than show a bare 400. Any
  other 400 stays a `serverError` with its body.
- **Capabilities.** `GET /v1/capabilities` is optional: a receiver may answer
  404/405, and every field but `protocolVersions` may be omitted. The app
  hides reconciliation unless `digest` and `uuids` are both advertised, and
  server statistics unless `stats` is; unknown capabilities hide both.
- **Connection test.** `ConnectionTester` calls capabilities first; if the
  endpoint is missing it POSTs a header-only batch (`type` `"probe"`, `reason`
  `"manual"`, every count 0) with no retries — any 2xx is success. 401 is
  reported as a rejected token; 403 as a user mismatch (the ingest's answer
  to a device token presented with another user's `X-User-ID`: the token is
  fine, the user ID is not), naming the ID the app sent; a network failure
  as unreachable with the cause (TLS, DNS, timeout, refused, ATS), anything
  else as a server error. Nothing about the test is persisted.
- **Pairing codes.** `PairingPayload.parse` reads the
  `puls://pair?url=&token=&user=` string `scripts/bootstrap.sh` prints, as a QR
  code and as text. A code is untrusted input however it arrives: the URL is
  re-validated with `ServerURLValidation` (plain `http://` to a non-local host
  is refused), the user must be a UUID, unknown query items are ignored, and
  anything that is not a `puls://pair` URL is "not a pairing code". Because a
  paste rarely holds exactly the payload, the parser *finds* it: surrounding
  whitespace, `<…>`, quotes or back-ticks, or the rest of the printed pairing
  block are tolerated, but the payload must start the text or follow
  whitespace or an opening wrapper — a `puls://` buried in another URL's query
  string is not picked out. `apply(to:)` writes only the server URL, token and
  user ID into a `SyncConfiguration` draft; `ServerFieldsDraft` is the
  on-screen equivalent, staging all three until `commit(to:)`.
- **Pairing links.** The same string works as a link, since the app registers
  the `puls` URL scheme; that is how the iOS Camera app hands over a scanned
  code. A custom URL scheme authenticates nobody: any web page or app can fire
  a `puls://pair` link, and any installed app can claim the scheme, so a
  link's token may reach whichever app iOS picks. Scanning inside the app
  never leaves the app; the link is a convenience. `PairingConfirmation` is the
  prompt the app shows before a link may fill anything — the host, whether it
  would replace a different configured server (same normalization as
  `ServerIdentity`), whether the connection is unencrypted — and accepting it
  goes no further than a scan: fields filled and tested, nothing applied.
- **Pairing by signing in.** The app's PulsHealth database option gets the
  same code from an account page, through a web sign-in sheet, rather than
  from a QR code. Its database URL is the operator's to choose, so the app
  cannot recognize that database by address. `ServerFieldsDraft.fill(fromSignIn:)`
  marks the draft, and `commit(to:)` records the URL as
  `SyncConfiguration.signedInDatabaseURL`. `isSignedInDatabase` holds only
  while `serverURL` still equals it, so any other way of changing the URL
  ends it, and nothing has to remember to clear it. The key is optional
  (absent from 1.6 and older state files), is never sent, and
  `ExportPlan.configuration` drops it with the URL and token.

## How a sync runs

1. `HealthSyncEngine.syncTypes(_:reason:)` fans out over enabled types with a
   `TaskGroup` (default 4 concurrent — HealthKit query throughput degrades
   beyond that). Its **non-incremental** branch claims every type up front
   (`claimTypes`) and releases each as its own run ends (`sweep`); claimed
   later, the observer's merged pass would take the queued types first and
   send a whole first sync down the slow path. A whole-history Apply handed to
   the iOS 26 continued-processing task calls `expectBackfill()` first, so
   observer wakes leave still-backfilling types alone until it claims them (a
   minute at most). The sweep follows `HealthTypeCatalog.backfillOrder`: the
   heaviest type first, then cheapest-first. Heart rate alone is a little over
   half of the catalog's `estimatedSamplesPerDay`, so one slot on it from the
   start plus three retiring the tail is both the shortest sweep and the one
   that shows results earliest. Incremental runs take the merged path below
   and are not reordered.
2. Per type: `HKAnchoredObjectQuery` pages from the stored anchor (nil anchor +
   start-date predicate = backfill), 1,000 samples/page, each page read while
   the one before it uploads. Only the in-memory cursor runs ahead; an upload
   that fails cancels the read and records nothing. ECGs (20) and heartbeat
   series (200) page smaller (`HealthTypeDescriptor.maxPageSize`): their pages
   are enriched with every voltage or beat in memory before upload, and a
   1,000-sample page of either is ~100 MB. Both sweeps use the same
   `pageSize(batchSize:)`, never above `batchSize`.
3. `SampleMapper` converts to DTOs; `SeriesEnricher` fills in series payloads;
   `NDJSONEncoder` produces a gzip batch.
4. `HTTPSyncTransport` uploads. **Only on success** does `recordUploadedBatch`
   advance the anchor and counters — the transactional pattern that makes the
   pipeline crash-safe (server dedupes re-sent pages by UUID). A 5xx, a 429 or
   a network failure is retried on a 2, 4, 8, 16 s ladder (one retry in an
   observer wake); a 429 or 503 with `Retry-After` waits what it asks, and
   one asking longer than 60 s (10 s in an observer wake) ends the run instead.
   A 4xx and a network failure no retry can fix (bad URL, ATS, a certificate
   the system rejects) are not retried.
5. **Cooldown after a terminal refusal.** When the server refuses a type's
   upload in a way that will repeat (`TransportError.startsCooldown`: a 4xx
   other than 408/429, an unsupported protocol, those network failures), the
   anchor stays put and the type sits out a cooldown
   (`TypeSyncState.cooldownUntil`, `TypeSyncStatus.cooldownUntil`): 15 min,
   doubling per refusal in a row up to 6 h. Observer, background and
   foreground runs skip it; a manual run (Sync Now, Start Backfill, a type's
   Sync) tries anyway. An acked upload lifts it, and so does applying any
   configuration change. A merged pack refused that way cools every type in
   it, so a type back from a cooldown uploads in a pack of its own until one
   of its uploads is acked.
6. Deletions arrive as anchored-query tombstones and ride along in the same batch.

`syncAllEnabled` runs its phases so the cheap, immediately useful data lands
first: activity rings, the recent aggregate window (below), the raw types, the
full aggregate pass, then workout routes and streams (`WorkoutEnrichmentSync`).
Every phase boundary is a safe place to be interrupted. A backfill claims its
raw types before the first phase.

**Recent data first.** A nil-anchor sweep returns history roughly oldest first,
so a backfill would deliver the newest samples last. Every sweep therefore
begins with a recent-window pass over its types that have not finished
backfilling (`RecentSampleWindow`): the last 30 days, read through a
second anchor of the type's own (`TypeSyncState.recentAnchorData`) from a
window start fixed when the stream begins, so the first run sends the month and
later runs only what is new in it. Its acks move that anchor alone — never
`anchorData`, never `backfillComplete`, not the sample count — and the sweep
sends those samples again when it gets there; the server ignores the repeats.
The stream is dropped when the backfill completes and skipped for a sync range
under 60 days. `HealthSyncEngine(recentWindowFirst: false)` turns it off where a
repeated sample would be a duplicate row rather than a no-op.

Incremental sync is the same loop, triggered by one multi-type `HKObserverQuery`
with `.immediate` background delivery, plus a `BGProcessingTask` safety net and a
full pass on every foreground open. Two things differ (`Engine/MergedSync.swift`):

**Observer callbacks are coalesced.** HealthKit delivers callbacks in bursts —
dozens within a few seconds — rather than one per change. They accumulate for
`observerCoalesceWindow` (default 2 s, measured from the burst's *first*
callback so a continuous stream cannot starve the flush) and run as one wake
over the deduped union of types. Every collected completion handler is
released afterwards: HealthKit stops waking the app after three unacknowledged
deliveries. `ObserverCoalescerTests` pins the burst, the order (acknowledged
after the wake, which stays for types another run holds), the expiry path
(acknowledged from the expiration handler, once) and `stopObserving`.

**Incremental uploads merge across types.** Upload cost is per request, not
per sample, and incremental pages are small (a median of a few samples), so
incremental runs fetch one page per type and pack pages into shared batches up
to `maxMergedBatchSamples`. Anchor-after-ack is unchanged: the budget is
clamped up to `batchSize`, so **a page is never split across batches**, one
page maps to exactly one ack, and a failed upload leaves every anchor in its
pack untouched for an idempotent replay. The merged path also reads the next
wave of types while the current packs upload, up to `maxConcurrentTypes` packs
at once. Both are safe for the same reason: within a round a type contributes
one page, so a flush's packs hold disjoint types and the wave being read holds
none of the buffered ones. Backfill keeps the per-type path: its pages are
already full, and four independent type pipelines overlap query and upload
better.

**Background wakes check the lock first.** HealthKit is unreadable while the
device is locked, and iOS runs `BGProcessingTask` when the device is idle —
overnight, locked. Background paths test `ProtectedData.isAvailable` up front
and record the wake as `skippedLocked` rather than run ~80 queries that would
all fail.

**Work the app starts itself asks for background time.** An observer delivery,
or leaving the app, buys a few seconds before iOS suspends the process.
Observer wakes and the app's own runs go through `BackgroundExecution.run`,
which takes the grace period iOS grants on request and **cancels** the work
when it expires: every sweep stops at a page boundary with its acked anchors
recorded and its claims released, and the wake is logged `expired`. A sweep
frozen mid-upload would keep its types claimed, and the next wake would find
them busy and do nothing. An observer wake whose types another run holds waits
for it (up to 25 s) instead of acknowledging HealthKit at once, and
acknowledges from the expiration handler if time runs out first.
`BGTaskScheduler` handlers keep their own expiration and never nest a request.

## How aggregates run

Aggregate configs (`SyncConfiguration.aggregates`, quantity types only) are
computed on-device with one-shot `HKStatisticsCollectionQueryDescriptor` runs —
no anchors exist for statistics, so each config keeps a `computedThrough`
watermark instead (advanced only after the server acks, like anchors):

1. Window = `[max(start, watermark − lookback), bucketFloor(now − settleDelay))`,
   where lookback = `max(7 d, 3×interval)` re-covers buckets late Watch data may
   have changed, and `settleDelay` holds back buckets that are still filling.
   First run (and a ~monthly full pass that repairs older edits/deletes) starts
   from the start date instead.
2. The window is split into ≤2,000-bucket chunks (`AggregateBucketing` — all
   boundaries are `Calendar`-computed, so day/month buckets survive DST).
3. Every bucket in a chunk uploads as an `{"aggregate": …}` NDJSON line — empty
   buckets carry an explicit `null` so the server upsert clears stale values.
   That is why no bucket starting before iOS 27's earliest readable date is
   computed at all (see "Limited history access" below): an unreadable bucket
   looks exactly like an empty one.

Triggers are shared with raw sync: the observer covers the *union* of raw-enabled
and aggregate types (aggregate-only types never get a raw sync), and
`syncAllEnabled` runs the full aggregate pass after the raw pass.

Ahead of the raw pass it runs `syncRecentAggregates`, a bounded recent window
(`AggregateSchedule.priorityWindow` — 30 days, or three buckets for intervals
coarser than that) over every enabled config whose `computedThrough` is still
nil. The full pass walks a series oldest-first, so the newest buckets come
last; and the server's `metric_daily` joins `aggregate_series`, which only an
aggregate line writes, so until one lands the viewer's daily charts are empty
however much raw data has arrived. A few dozen buckets per config fixes both.

The pass moves **no watermark**: it records through
`recordAggregateUploadWithoutWatermark`. Its chunks end near *now*, so
recording them as progress would push `computedThrough` (and, mid-full-pass,
`fullRecomputeThrough`) past the whole unprocessed history, and the full pass
would compute nothing older than the window — the aggregate twin of reusing a
raw type's `HKQueryAnchor` for a date-bounded query. Because it moves nothing
it is safe to run, repeat or skip; it self-gates on `computedThrough == nil`,
so it stops once the full pass makes its first acked progress.

Function legality is the sharp edge: HealthKit raises an uncatchable
NSInvalidArgumentException at query *execution* for illegal option×type combos.
`HealthTypeCatalog.allowedAggregateFunctions(for:)` (cumulative → sum/mostRecent/
duration; any discrete style → average/min/max/mostRecent/duration) is enforced
in the UI and re-checked in the engine, and verified against all 378 combos by
the app-hosted `AggregateMatrixTests`.

## Limited history access (iOS 27)

From iOS 27 the Health permission sheet has a second page, "How much data
would you like to share?" — *Past 30 Days and Future Data* or *All Recorded
Data and Future Data* — and Settings → Privacy & Security → Health → (app) →
(type) offers *Limited Access* or *Full Access* per type afterwards. Limited,
HealthKit reads a type only from an earliest date (30 days before the choice,
fixed from then on) and answers every query about older history as if it were
empty. It reports the date through `HKHealthStore.earliestAuthorizedSampleDate(for:)`;
`HealthSyncEngine.earliestAuthorizedDates()` is the package's view of it (type
identifier → date, limited types only; empty before iOS 27, when built with
the iOS 26 SDK, and for undecided types).

On the iOS 27.0 simulator: every type one sheet grants gets the same date, the
activity-summary type included; a sample is hidden only when it *ends* before
the date; a bucket straddling the date returns part of its value; narrowing
access reports no deletions to an older anchor; and an anchor taken under the
limit never returns the older samples once the limit is lifted.

Four safeguards handle this:

- **Empty must not reach the server.** The passes that overwrite or delete to
  match what they read are clamped (`ReadableHistory`): aggregate passes —
  scheduled, full, priority and lookback alike — compute only buckets that
  start at or after the date, so no `null` lands on unreadable history and no
  partial value on the bucket that straddles it; the rings start at the first
  whole readable day; reconciliation compares from the date and throws
  `readableHistoryUnknown` rather than guess when HealthKit cannot say (and
  a type HealthKit lists with a date is the one case reconciliation knows it
  may read — see "How reconciliation runs"). The raw
  sweep adds what HealthKit returns and deletes only HealthKit's own
  tombstones, and the route and stream phases only follow workouts HealthKit
  returns and never overwrite, so neither needs a clamp.
- **Widening must re-read.** Each pass records the date it ran under
  (`readableSince` on the type, aggregate, rings and enrichment states;
  optional, so older state files decode). `refreshReadableHistory` — before
  claims in `syncAllEnabled`, the backfill branch of `syncTypes`,
  `sync(type:)` and every observer wake, at most every 15 minutes, forced by
  the app on foreground and after each permission sheet — resets the anchors
  of a raw type whose date moved earlier or went away and reopens its
  backfill; the server ignores what it already has. Aggregate series, the
  rings and the enrichment phases notice on their next run and start over
  from the start date. The same date (within a day) resets nothing, and a
  narrowing only records the new date.
- **A widening must be proved.** A type set to *None* in Settings drops out
  of `earliestAuthorizedSampleDate(for:)` exactly like one set to Full Access,
  and every query for it comes back empty without an error. Treated as a
  widening, that would reset an aggregate series with nothing to clamp it and
  send every bucket as a null. So a reported widening counts only once
  HealthKit returns a sample (for the rings, a day) that ends before the
  recorded date (`ReadableHistory.resolve`); otherwise the recorded date stands
  and the clamps stay. A type with no older data fails that test too, which
  costs nothing: there is nothing older to re-read.
- **Unknown fails closed.** Every HealthKit call here gives up after ten
  seconds, since these calls can stall far longer. An aggregate or ring pass
  that cannot learn its limit records `readableHistoryUnknown` and sends
  nothing (a locked device just skips); reconciliation throws; an export
  reports that it may start later than asked.

One case goes unnoticed: a 1.4 or 1.5 install whose backfill ran under a
30-day limit that was widened again before 1.6's first refresh. Its state
carries no `readableSince`, so nothing says a re-sweep is due, and the older
history stays off the server. Settings → Reset All Anchors re-reads it.

The API exists only in the iOS 27 SDK, and CI also builds with Xcode 26.5, so
its one call sits behind `#if compiler(>=6.4)` (Xcode 27.0 ships Swift 6.4;
Xcode 26.5 ships 6.3.2) as well as `#available(iOS 27.0, *)`. Built with the
older SDK, the package cannot query this API; that does not prove that a phone
running a newer iOS grants unrestricted history.

Don't Allow on the history page throws `errorAuthorizationDenied` and leaves
the types undetermined, where Don't Allow on the first page returns normally;
`requestAuthorization` returns `HealthAccessRequestOutcome.declined` for it
instead of throwing.

## How activity rings run

The "Activity Rings" type (`HealthTypeCatalog.activitySummaryIdentifier`) exports
daily `HKActivitySummary` objects — Move (active energy or, in `appleMoveTime`
mode, move minutes), Exercise, and Stand, each with the user's goal. These aren't
`HKSample`s: no UUID, one per *local calendar day*, and the current day keeps
changing. So they mirror the aggregate model — a `HKActivitySummaryQueryDescriptor`
over a day window, a *singleton* `computedThrough` watermark
(`SyncStateStore.activitySummaryState`) advanced only after ack, and a trailing
lookback (today is always re-queried). Each day uploads as an
`{"activitySummary": …}` line; the server upserts by `date`.

Differences from raw/aggregate sync: `HKActivitySummaryType` is an `HKObjectType`,
not an `HKSampleType`, so its catalog entry has `sampleType == nil` (kept out of
`bulkReadAuthorizationSampleTypes` and the observer) and the engine unions
`HKObjectType.activitySummaryType()` into the read-auth set separately. There is
**no observer / no background delivery** for summaries, so they ride other wakes:
`syncAllEnabled` (foreground/periodic/scheduled — the *first* phase, so a first
backfill shows rings before every type has drained) and
`refreshActivitySummaryIfStale()` at the tail of every observer wake.

That second path is load-bearing. The scheduled path runs from the
`BGProcessingTask`, which iOS starts while the device is idle and therefore
locked, when every ring query fails with `errorDatabaseInaccessible`; an
observer wake is by definition a moment when HealthKit is readable. The
refresh is rate-limited to hourly via `ActivitySummaryState.lastComputedAt`,
because today's ring mutates all day and observer wakes are frequent.

## How reconciliation runs

Reconciliation holds the same per-type claim as raw sync from its first read
through its last deletion. A busy type rejects another reconciliation before
any reads; observer updates arriving during it queue a raw pass that drains
before the claim is released. Cancellation releases the claim and leaves any
pending update for the next wake.

HealthKit purges deletion tombstones after a while, so an observer or
scheduled sync can miss a delete. *Reconcile with Database* on a type's sync
detail (`HealthSyncEngine.reconcile(type:)`, quantity, category and workout
kinds only) repairs that by hand: it asks the server for one UUID XOR digest
per UTC month (`GET /v1/digest`), queries HealthKit for the same months by
start date, and for every month whose digest or count differs fetches the
server's UUIDs (`GET /v1/uuids`), re-uploads what the server lacks and sends
a deletion for every server row the device did not return. A month is read
as UUIDs only, 5,000 samples at a time through an anchored query; the samples
the server lacks are fetched again by UUID, one upload batch at a time. The comparison
starts at the sync's start date, or at the type's earliest readable date when
iOS 27 limits it (above).

**A month the device returned nothing for is left alone.** A type whose Health
read access is off — switched to *None* in Settings, or never granted — answers
every query with an empty result and no error, and
`HKHealthStore.authorizationStatus(for:)` reports only *sharing* (write)
status; nothing tells such a type from a month the user cleared in Health. So
where HealthKit returned no samples and the server has rows,
`ReconcileDigest.orphanVerdict` is `.withhold`: no deletion goes out (there is
nothing to re-upload either), the month is counted in the report's
`windowsUnverified` with its rows in `orphanDeletionsWithheld`, and the
summary says so. The one proof of read access is iOS 27 listing the type in
`earliestAuthorizedSampleDate(for:)` just now — a type set to None drops out
of that answer — so with `ReadableLimit.isConfirmed` an empty month from the
confirmed date on is the truth and its orphans are deleted. A run that read
nothing in any month while the server has rows throws
`SyncError.reconciliationUnreadable` (the app shows it as the last error)
rather than record itself as in sync; a month with real samples still has its
missing ones deleted, as before. The cost is that a type the user emptied in
Health keeps its server rows until something of it is readable again; the
alternative was a type switched off in Settings wiping its server copy.

## On-device export

`HealthExporter` writes HealthKit data to files with no server involved — the
public API behind the app's export screen. File formats, columns and how they
relate to the server's `/v1/export` are in [`docs/export.md`](../docs/export.md),
"On-device export"; this is the package side.

```swift
let request = ExportRequest(
    selection: ExportSelection(configuration: config), // or one chosen for this export
    configuration: config,            // batch size, concurrency, user ID, grid anchor
    startDate: nil,                   // nil = all time
    endDate: nil,                     // nil = now; exclusive
    format: .jsonl,                   // or .json / .csv
    zipped: false,                    // true = one .zip in result.files
    deviceID: engine.store.deviceID)  // attribute a replay to this install
let result = try await HealthExporter().run(request) { progress in
    // arbitrary executor: phase, currentType, rowsWritten, bytesWritten
}
// result.files → share sheet; then HealthExporter.removeAllExports()
```

**An export is a sync sweep pointed at files.** The same anchored queries,
`SampleMapper`, enrichment and aggregate math run; `ExportFileTransport` stands
where `HTTPSyncTransport` would and appends each batch instead of POSTing it.
JSONL is therefore the wire format itself (uncompressed batches, concatenated —
replayable into `/v1/batches`), and CSV is a per-dataset flattening of the same
batches. The default dataset layout keeps the product API's shared column lists;
category labels can be supplied from the caller's vocabulary. `request.csvOptions`
selects dataset, per-type, combined or daily-summary tables, optional source/device
and metadata columns, and an optional data-row limit per CSV part. Defaults preserve
the existing columns and filenames. JSON and JSONL ignore CSV options. JSON
is a single array of the same header and record objects as JSONL, streamed one
batch at a time and closed on successful completion. Daily summary requires
1-day aggregates and preserves raw selections in their separate tables. See
`docs/export.md` for column identity, missing values, splitting and summary bounds.
Records are buffered into `FileHandle` writes, with CSV rotation only between
complete records. Raw-data memory is bounded by `batchSize`; the optional daily
summary also keeps a bounded pivot (at most one million series/bucket entries).
Workout rows wait in memory because their `hasRoute`/`availableMetrics` columns describe route and
stream batches that arrive two phases later.

**It never shares sync state — the one rule that must not bend.** Anchors and
watermarks belong to the active saved destination's types, and the engine
advances them whenever its transport returns normally. Hand
`ExportFileTransport` to the app's real engine and every exported sample is
recorded as delivered: the server never receives it and nothing reports a
problem. So each `run` builds its own `HealthSyncEngine` over a `SyncStateStore`,
`SyncEventLog` **and `WakeLog`** in a throwaway directory with an
`InMemoryTokenStore`, and deletes the directory on every exit path. (The wake
log matters: the engine's default one opens the app's real `wake-log.json` and
rewrites any wake still marked running as interrupted.) An empty store is also
what makes the export whole — nil anchors mean "everything since the start
date". That second engine has no side effects on the first: it never calls
`startObserving` (no observer query, no background-delivery changes, which are
per-app), no `BackgroundSyncScheduler` is built over it, and its configuration
has no server URL, token or identity fields, so no HTTP transport or API client
exists and no `{"profile":…}` line is written.

It calls the sweep's phases itself — rings, raw types (`.manual`, so the
per-type backfill path), the full aggregate pass, routes, streams — rather than
`syncAllEnabled`, which would add the recent-aggregate priority window and
write the newest month of every series twice; and it builds the engine with
`recentWindowFirst: false`, because the raw recent-window pass would do the
same to the newest month of samples. "All time" queries from 1900
rather than `.distantPast` (whose local day is in 1 BC west of Greenwich), and
each aggregate series starts at its type's first sample, snapped to the bucket
grid the real sync uses so a replay overwrites the server's buckets instead of
interleaving a second set.

**Failures are reported, not logged.** The engine survives a type it cannot
read by logging and moving on, which is right for a sync that retries on the
next wake and wrong for an export, where it would be a short file presented as
a whole one. After the sweep, `ExportPlan.failures` asks the throwaway store
whether each unit reached its completion marker (`backfillComplete`, an
aggregate's or enrichment phase's `lastFullRecomputeAt`, the rings'
`computedThrough`) — a test every failure path fails the same way, including
future ones — and takes only the wording from the event log, which
`ExportEventCollector` follows live because the log is a 2,000-entry ring that
a real export overflows. Samples `SampleMapper` could not convert come through
`HealthSyncEngine.unmappableSampleCounts` rather than being parsed out of the
warning. The result:

- a locked device, no HealthKit, nothing selected, nothing readable, no data,
  or a write error **throws** `HealthExportError`, and a throw (cancellation
  included) always leaves no files;
- a partial export **returns**, with `failures`, `unmappableSamples` and
  `isComplete == false`, and the manifest says `"complete": false`;
- an export some of whose types iOS 27 lets the app read only from a date
  after the export's start **returns** too, with those types and dates in
  `limitedHistory` (and the manifest's) and `isComplete == false` — nothing
  failed, but the files are not all of the range asked for;
- a CSV export counts what it has no file for (ECG traces, heartbeat series,
  deletions) in `notRepresented`.

Exported files are `.completeUntilFirstUserAuthentication` — a long export must
survive the phone locking mid-run — and deliberately not run through
`ProtectedStateFile`: they exist to leave the app. By default they are staged
under `HealthExporter.stagingRoot` in the temporary directory, which
`removeAllExports()` clears, leftovers from a crash included.

`ExportPresentation.swift` is the screen's pure half, kept in the package so
it can be tested (`ExportPresentationTests`): `ExportRange` (a range starts at a local
midnight, because daily series are whole local days), `ExportSelectionSummary`,
`ExportFailureCopy` (what each `HealthExportError` is called — "no data" and
"access declined" are one answer from HealthKit, so that copy gives both
readings), display names for datasets and phases, and
`ExportResult.writtenRowCounts`, which leaves out the rows a CSV export counted
but has no file for.

## Exploring what HealthKit holds

`HealthExplorer` answers "what is in here?" for one catalog type before anyone
decides to sync or export it — the read-only layer behind a type-browser
screen. It owns a plain `HKHealthStore` and returns numbers, never samples.

```swift
let explorer = HealthExplorer()

// Three small queries: oldest sample, newest sample, the writers.
let facts = try await explorer.quickFacts(for: "HKQuantityTypeIdentifierHeartRate")

// One ascending scan of the type, reduced as it goes.
var options = HealthExplorer.ProfileOptions()   // rangeStart/End, lookbackDays, histogramBins, reservoirCapacity, calendar
options.lookbackDays = 365                      // only the past year; judged by the number, not the date it resolved to
let profile = try await explorer.profile(for: facts.typeIdentifier, options: options) { progress in
    // phase (probing/scanning/finishing), samplesScanned, pagesScanned, scannedThrough
}
profile.sampleCount        // raw HealthKit count — the count "drained" decisions use
profile.unmappableCount    // quantities not convertible to the catalog unit
profile.values?.median     // exact min/max/mean/stddev; quantiles + histogram estimated past the reservoir
profile.values?.histogram  // round-width bins over the 1st–99th percentile (5th–95th past a long tail); belowCount/aboveCount are the tails left off
profile.cadence, profile.dailyCounts, profile.coverage, profile.sources, profile.devices

// What a configured aggregate series would produce — same query, same bucket
// math, same recovery as the sync — computed for the caller and uploaded nowhere.
let buckets = try await explorer.aggregatePreview(
    AggregateConfig(typeIdentifier: facts.typeIdentifier, function: .average, intervalUnit: .day),
    from: start, to: Date())

// Cache: one JSON file per type under Application Support/PulsHealthSync/profiles/.
let store = TypeProfileStore()
if let cached = await store.profile(for: facts.typeIdentifier),
   !TypeProfileStore.isStale(cached, facts: facts, options: options, maxAge: 7 * 86_400) {
    // still current
}
try await store.save(profile)
```

Cached profiles are refreshed when their calendar time zone changes. Profile
version 6 invalidates percentiles and histograms produced by the earlier biased
reservoir scheduler; exact counts and means were not derived from that sample.
Activity-ring queries and wire days use the Gregorian calendar in the phone's
time zone, including when the phone prefers Buddhist or Japanese dates.

**This layer never touches sync state.** It has no engine, no
`SyncStateStore`, no event or wake log and no transport, and that is the
point rather than a limitation: the engine advances a type's anchor whenever
its transport returns normally, and that progress belongs to the active saved
destination — the reason the on-device export builds a
throwaway engine over its own state. Exploring does not need an engine at
all. `profile` pages with a date-sorted `HKSampleQueryDescriptor`
(`SampleCursor`: ascending by start, 5,000 per page, the UUIDs at the last
instant carried into the next page and dropped; a page made of one instant is
widened up to 4× and, past that, stepped over and noted in `failureReason`),
and `aggregatePreview` runs `AggregateQuery` — the same
`HKStatisticsCollectionQuery` and missing-data-source split retry the engine
uses — with a closure that appends rows where the engine's uploads and acks.
Neither has a cursor to persist. What the explorer shares with the engine is
pure code only: the catalog, `AggregateBucketing`, `AggregateQuery` and
`ReadableHistory`. Under iOS 27's limited history access a profile scan starts
at the type's earliest readable date when that is later than the requested
start, and says so (`TypeProfile.readableSince`, version 5); quick facts carry
the date, `TypeProfileStore.isStale` treats a profile scanned under another
one as stale, and the aggregate preview starts at the first whole readable
bucket.

`TypeProfile` is a summary, not data: no UUIDs, no metadata, no per-sample
values. Its memory and its file are bounded whatever the count — two
fixed-capacity reservoirs (8,192 values by default; `isEstimated` flags the
quantiles, histogram and cadence percentiles once a type exceeds it), one
entry per local day with samples, and one per distinct source, device and
label. That is what lets `TypeProfileStore` keep it on disk without changing
the privacy documents' "no health samples stored on the device" claim; it is
still about health data, so it is written through `ProtectedStateFile`
(unreadable until first unlock, excluded from backup) with epoch-millisecond
dates like every other state file. `TypeProfile.currentVersion` is bumped
when the shape or meaning changes, and the store deletes any file from
another version rather than showing numbers computed under old rules.

Failure rules mirror the export's: a locked device (`deviceLocked`), no
HealthKit, an unknown identifier or an illegal aggregate function
(`aggregatePreview` re-validates against
`HealthTypeCatalog.allowedAggregateFunctions`, because HealthKit crashes on an
illegal option × aggregation-style combination rather than throwing) and a
failure before the first page all **throw** `HealthExploreError`; a failure
after some pages **returns** the profile with `isComplete == false` and the
scrubbed reason. Cancellation always throws and keeps nothing.

## Testing

Run from the repository root with full Xcode selected:

```bash
python3 scripts/test-ios.py --ios 27.0 --suite package
```

The runner creates its own simulator and retains logs and a receipt. See
[CONTRIBUTING.md](../CONTRIBUTING.md) for SDK selection and the older-runtime
check. A simulator pass does not establish background behavior on a real phone.

### Medication identity

Medication doses (iOS 26+) carry a readable `medication` name when authorized
and an optional `medicationID` UUID alias. IDs group equal HealthKit medication
concepts within this app installation, even across launches, exports and saved
receivers. Identical or changed display names do not determine identity.
`MedicationIdentityStore` persists secure archives of Apple's opaque identifiers
with random UUIDs in a protected, backup-excluded registry separate from anchors.
Exports share only this registry, never sync progress. File loss or reinstall
creates new IDs; cross-device stability is not promised. A registry failure
prevents upload acknowledgement/export completion.

The public Apple API exposes concept equality, display text, general form and
related clinical codes, but no public raw concept string or per-dose conversion
from `count` into mg/g. An alias is not a clinical code, formulation, strength or
mass conversion. Never infer those from a name. Previously inserted server rows
are not overwritten by normal resync; use the explicitly reviewed
[historical repair tool](../tools/repair-medications/README.md) with a fresh phone
export to repair exact dose UUIDs.
