import Foundation
import HealthKit
import os

// MARK: - Bucket math (pure, testable without HealthKit)

/// Bucket-boundary math for one aggregate series: boundaries are `anchor + N ×
/// interval`, always computed via `Calendar` so day/week/month buckets stay
/// correct across DST transitions and month-length changes.
struct AggregateBucketing: Sendable {
    var anchor: Date
    var intervalValue: Int
    var intervalUnit: AggregateIntervalUnit
    var calendar: Calendar

    init(
        anchor: Date, intervalValue: Int, intervalUnit: AggregateIntervalUnit,
        calendar: Calendar = .current
    ) {
        self.anchor = anchor
        self.intervalValue = max(1, intervalValue)
        self.intervalUnit = intervalUnit
        self.calendar = calendar
    }

    private var approximateSeconds: TimeInterval {
        Double(intervalValue) * intervalUnit.approximateSeconds
    }

    /// Start of bucket `index` (bucket 0 starts at the anchor).
    func start(ofBucket index: Int) -> Date {
        guard index != 0 else { return anchor }
        return calendar.date(
            byAdding: intervalUnit.dateComponents(value: intervalValue, times: index),
            to: anchor
        ) ?? anchor.addingTimeInterval(Double(index) * approximateSeconds)
    }

    /// Index of the bucket containing `date` (start ≤ date < next start).
    /// O(1): estimate by approximate length, then correct for calendar drift.
    func index(of date: Date) -> Int {
        var i = Int((date.timeIntervalSince(anchor) / approximateSeconds).rounded(.down))
        while start(ofBucket: i) > date { i -= 1 }
        while start(ofBucket: i + 1) <= date { i += 1 }
        return i
    }

    /// Largest bucket boundary at or before `date`, clamped to the anchor.
    func floorBoundary(_ date: Date) -> Date {
        date <= anchor ? anchor : start(ofBucket: index(of: date))
    }

    /// Bucket-aligned chunks covering [from, to), each at most `maxBuckets`
    /// buckets so years of small buckets never become one giant query/upload.
    /// `to` is expected to be a bucket boundary; `from` is floored to one.
    func chunks(
        from: Date, to: Date, maxBuckets: Int = AggregateSchedule.maxBucketsPerChunk
    ) -> [DateInterval] {
        guard to > anchor, from < to else { return [] }
        let first = max(0, index(of: max(from, anchor)))
        let end = index(of: to) // `to` is a boundary: the bucket starting there is excluded
        guard end > first else { return [] }
        var out: [DateInterval] = []
        var i = first
        while i < end {
            let j = Swift.min(i + maxBuckets, end)
            out.append(DateInterval(start: start(ofBucket: i), end: start(ofBucket: j)))
            i = j
        }
        return out
    }

    /// Split a bucket-aligned chunk into two smaller bucket-aligned chunks.
    /// Returns nil when the chunk is already one bucket wide.
    func split(_ chunk: DateInterval) -> (DateInterval, DateInterval)? {
        let first = index(of: chunk.start)
        let end = index(of: chunk.end)
        let count = end - first
        guard count > 1 else { return nil }
        let mid = start(ofBucket: first + count / 2)
        guard mid > chunk.start, mid < chunk.end else { return nil }
        return (
            DateInterval(start: chunk.start, end: mid),
            DateInterval(start: mid, end: chunk.end)
        )
    }
}

/// Window policy for aggregate recomputes. Statistics queries have no anchors,
/// so progress is a watermark plus a trailing lookback that re-covers buckets
/// late-arriving Watch data may have changed.
enum AggregateSchedule {
    static let maxBucketsPerChunk = 2_000
    /// Full-series recompute cadence — repairs edits/deletes older than the lookback.
    static let fullRecomputeInterval: TimeInterval = 30 * 86_400
    /// How far back the priority pass reaches before the first full pass runs.
    static let priorityWindowSpan: TimeInterval = 30 * 86_400

    static func lookback(intervalSeconds: TimeInterval) -> TimeInterval {
        max(7 * 86_400, 3 * intervalSeconds)
    }

    /// The settled window to (re)compute this run, or nil when nothing has
    /// settled yet. `to` is the last fully-elapsed bucket boundary at least
    /// `settleDelay` old; `from` trails the watermark by the lookback (or the
    /// start date on a full pass / first run).
    static func window(
        startDate: Date,
        computedThrough: Date?,
        fullPass: Bool,
        fullRecomputeThrough: Date? = nil,
        settleDelay: TimeInterval,
        now: Date,
        bucketing: AggregateBucketing,
        intervalSeconds: TimeInterval
    ) -> (from: Date, to: Date)? {
        let to = bucketing.floorBoundary(now.addingTimeInterval(-settleDelay))
        let from: Date
        if fullPass, let fullRecomputeThrough {
            // Initial and later scheduled full passes use a separate durable
            // cursor because the normal high-water mark may already be near now.
            from = max(startDate, fullRecomputeThrough)
        } else if !fullPass, let computedThrough {
            from = max(startDate, computedThrough.addingTimeInterval(
                -lookback(intervalSeconds: intervalSeconds)))
        } else {
            from = startDate
        }
        guard from < to else { return nil }
        return (from, to)
    }

    /// The bounded recent window a priority pass covers, or nil when nothing has
    /// settled yet. `to` is the same settled boundary a scheduled run would use;
    /// `from` trails it by `priorityWindowSpan`, or three buckets for intervals
    /// coarser than that, so a month-bucket series still gets a usable series
    /// rather than a single point. Clamped to the start date.
    ///
    /// This window is deliberately *not* a function of any watermark: the pass
    /// exists to put recent buckets on the server before the full pass has run,
    /// and it must be safe to run, repeat, or skip without changing what the
    /// full pass covers.
    static func priorityWindow(
        startDate: Date,
        settleDelay: TimeInterval,
        now: Date,
        bucketing: AggregateBucketing,
        intervalSeconds: TimeInterval
    ) -> (from: Date, to: Date)? {
        let to = bucketing.floorBoundary(now.addingTimeInterval(-settleDelay))
        let span = max(priorityWindowSpan, 3 * intervalSeconds)
        let from = max(startDate, bucketing.floorBoundary(to.addingTimeInterval(-span)))
        guard from < to else { return nil }
        return (from, to)
    }
}

/// What one run of an aggregate config is for.
enum AggregatePass: Sendable {
    /// The normal watermark-driven run: a trailing lookback, or a full recompute.
    case scheduled
    /// A bounded recent window, run ahead of the raw sweep on a first backfill.
    /// Moves no watermark — see `recordAggregateUploadWithoutWatermark`.
    case priority
}

/// Migration helper for state written before durable full-pass markers existed.
/// In that schema, `lastFullRecomputeAt == nil` plus a non-nil high-water mark
/// can only mean the initial full pass was interrupted after making progress.
enum FullRecomputeMigration {
    static func legacyInitialCursor(
        computedThrough: Date?,
        lastFullRecomputeAt: Date?,
        fullRecomputeStartedAt: Date?
    ) -> Date? {
        guard fullRecomputeStartedAt == nil, lastFullRecomputeAt == nil else { return nil }
        return computedThrough
    }
}

// MARK: - Engine integration

extension HealthSyncEngine {
    /// Run every enabled aggregate config, `maxConcurrentTypes` at a time.
    /// Called after the raw sweep by `syncAllEnabled`, and directly by the app.
    /// `syncRecentAggregates` is the sibling that runs *before* it.
    public func syncAllAggregates(reason: SyncReason = .incremental) async {
        guard let configurationUse = beginConfigurationUse() else { return }
        defer { activeSyncs.remove(configurationUse) }
        await fanOutAggregates(
            await store.configuration.aggregates.filter(\.enabled),
            reason: reason, pass: .scheduled)
    }

    /// Cover a bounded recent window for every enabled aggregate config that has
    /// never been computed, ahead of the raw sweep on a first backfill.
    ///
    /// Two things make this worth its own phase rather than leaving it to the
    /// full pass. The server's daily views join `aggregate_series`, and only an
    /// aggregate line creates a row there — so until *some* aggregate has landed
    /// there is nothing daily to show however many raw samples have arrived. And
    /// the full pass runs oldest-first from the start date, so on a multi-year
    /// series the newest buckets are the last thing it produces.
    ///
    /// Self-limiting: a config whose `computedThrough` is set has already had
    /// buckets acked, so the window is already covered and this skips it. That
    /// also means an interrupted first backfill keeps re-covering the recent
    /// window on each run until the full pass makes its first acked progress,
    /// which is the behaviour we want while there is nothing else to show.
    public func syncRecentAggregates(reason: SyncReason = .backfill) async {
        guard let configurationUse = beginConfigurationUse() else { return }
        defer { activeSyncs.remove(configurationUse) }
        var pending: [AggregateConfig] = []
        for agg in await store.configuration.aggregates where agg.enabled {
            if await store.aggregateState(for: agg.id).computedThrough == nil {
                pending.append(agg)
            }
        }
        await fanOutAggregates(pending, reason: reason, pass: .priority)
    }

    private func fanOutAggregates(
        _ configs: [AggregateConfig], reason: SyncReason, pass: AggregatePass
    ) async {
        guard !configs.isEmpty else { return }
        let concurrency = await store.configuration.maxConcurrentTypes
        await withTaskGroup(of: Void.self) { group in
            var iterator = configs.makeIterator()
            var inFlight = 0
            func addNext(_ group: inout TaskGroup<Void>) {
                if let next = iterator.next() {
                    inFlight += 1
                    group.addTask {
                        await self.syncAggregate(configID: next.id, reason: reason, pass: pass)
                    }
                }
            }
            for _ in 0..<max(1, concurrency) { addNext(&group) }
            while inFlight > 0 {
                await group.next()
                inFlight -= 1
                addNext(&group)
            }
        }
        notifyChanged()
    }

    /// Every enabled aggregate config for one type — what an observer fire runs.
    public func syncAggregates(forType identifier: String, reason: SyncReason = .incremental) async {
        guard let configurationUse = beginConfigurationUse() else { return }
        defer { activeSyncs.remove(configurationUse) }
        let configs = await store.configuration.aggregates
            .filter { $0.enabled && $0.typeIdentifier == identifier }
        for config in configs {
            await syncAggregate(configID: config.id, reason: reason)
        }
    }

    /// Recompute + upload one aggregate config. Overlap-guarded like `sync(type:)`.
    public func syncAggregate(configID: UUID, reason: SyncReason = .incremental) async {
        await syncAggregate(configID: configID, reason: reason, pass: .scheduled)
    }

    func syncAggregate(configID: UUID, reason: SyncReason, pass: AggregatePass) async {
        guard !configurationMutationInProgress else { return }
        let key = "agg:\(configID.uuidString)"
        guard !activeSyncs.contains(key) else {
            pendingResync.insert(key)
            return
        }
        activeSyncs.insert(key)
        defer { activeSyncs.remove(key) }

        var nextReason = reason
        var nextPass = pass
        repeat {
            await runAggregateSync(configID: configID, reason: nextReason, pass: nextPass)
            nextReason = .incremental
            // Work queued while this run was in flight is ordinary sync work,
            // whatever this run happened to be for.
            nextPass = .scheduled
        } while pendingResync.remove(key) != nil
    }

    private func runAggregateSync(
        configID: UUID, reason: SyncReason, pass: AggregatePass
    ) async {
        let config = await store.configuration
        guard let agg = config.aggregates.first(where: { $0.id == configID }), agg.enabled else {
            return
        }
        let typeID = agg.typeIdentifier
        guard await allowsSync(types: [typeID]) else { return }
        guard let descriptor = HealthTypeCatalog.descriptor(for: typeID),
              descriptor.kind == .quantity else {
            await eventLog.log(.error, type: typeID, "Aggregate \(agg.summaryLabel): not a quantity type")
            return
        }
        // Re-validate even though the UI only offers legal functions: an illegal
        // option×aggregation-style combo raises an ObjC exception inside
        // HealthKit (a crash, not a catchable Swift error).
        guard HealthTypeCatalog.allowedAggregateFunctions(for: typeID).contains(agg.function) else {
            await eventLog.log(
                .error, type: typeID,
                "Aggregate \(agg.summaryLabel): \(agg.function.rawValue) is not supported by this type's aggregation style — skipping"
            )
            return
        }
        await ensureTransport()
        guard let transport else {
            await eventLog.log(.error, type: typeID, "No transport configured — set server URL and token")
            return
        }

        let calendar = Calendar.current
        let startDate = agg.startDate ?? config.startDate
        let anchor = calendar.startOfDay(for: startDate)
        let bucketing = AggregateBucketing(
            anchor: anchor, intervalValue: agg.intervalValue,
            intervalUnit: agg.intervalUnit, calendar: calendar
        )

        // An export's engine reads up to `readEnd`: clamping "now" to it makes
        // the window's settled boundary (`floorBoundary(now - settleDelay)`)
        // the last bucket boundary at or before the bound, so a bucket that
        // straddles it is not computed — it would be a partial value that
        // looked like a whole one.
        let now = min(Date(), readEnd ?? .distantFuture)
        var state = await store.aggregateState(for: configID)

        // iOS 27 limited history access. HealthKit answers a statistics query
        // over history it will not let the app read with empty buckets, and
        // an empty bucket is uploaded as an explicit null — which on the
        // server overwrites the real value. So no bucket before the type's
        // earliest readable date, or straddling it, is computed (the window
        // is clamped below); the date is recorded with the series, because
        // the watermark is a high-water mark that will sit past the buckets
        // left out; and when access has widened since, the series starts
        // over from the start date and computes them.
        let lookup = await readableLimit(for: typeID, recorded: state.readableSince)
        let readable = ReadableLimit(
            since: ReadableHistory.effectiveLimit(lookup.since, readingFrom: anchor),
            isFresh: lookup.isFresh)
        if ReadableHistory.isSupported, !readable.isFresh, readable.since == nil {
            // Nothing to stand in for the answer: computing unclamped could
            // null out history, so wait for a run where HealthKit can say.
            // Locked is the expected case and no fault (CLAUDE.md, "A locked
            // device…"); anything else is recorded against the series, so
            // it shows rather than the series just going quiet.
            if await isHealthDataAccessible() {
                await store.recordAggregateError(
                    configID: configID, error: SyncError.readableHistoryUnknown(descriptor.displayName))
                await eventLog.log(
                    .warn, type: typeID,
                    "Aggregate \(agg.summaryLabel): could not tell how much Health history is readable — skipped, will retry")
            } else {
                await eventLog.log(
                    .warn, type: typeID, "Aggregate \(agg.summaryLabel): Health database locked — will retry on next wake")
            }
            return
        }
        if readable.isFresh {
            switch ReadableHistory.change(from: state.readableSince, to: readable.since) {
            case .widened:
                await store.resetAggregate(configID: configID)
                await eventLog.log(
                    .info, type: typeID,
                    "Aggregate \(agg.summaryLabel): Health access widened — recomputing the whole series from its start date")
                state = await store.aggregateState(for: configID)
                if let since = readable.since {
                    await store.updateAggregate(configID) { $0.readableSince = since }
                    state = await store.aggregateState(for: configID)
                }
            case .narrowed:
                let since = readable.since
                await store.updateAggregate(configID) { $0.readableSince = since }
                state = await store.aggregateState(for: configID)
            case .unchanged:
                break
            }
        }

        // A priority pass never opens, resumes or completes a full recompute:
        // its whole contract is to leave the watermarks where it found them.
        let fullPass: Bool
        let resolvedWindow: (from: Date, to: Date)?
        switch pass {
        case .priority:
            fullPass = false
            resolvedWindow = AggregateSchedule.priorityWindow(
                startDate: anchor,
                settleDelay: agg.settleDelay,
                now: now,
                bucketing: bucketing,
                intervalSeconds: agg.approximateIntervalSeconds
            )
        case .scheduled:
            let fullPassDue = state.computedThrough == nil
                || state.lastFullRecomputeAt.map {
                    now.timeIntervalSince($0) > AggregateSchedule.fullRecomputeInterval
                } ?? true
            fullPass = state.fullRecomputeStartedAt != nil || fullPassDue
            if fullPass, state.fullRecomputeStartedAt == nil {
                let legacyCursor = FullRecomputeMigration.legacyInitialCursor(
                    computedThrough: state.computedThrough,
                    lastFullRecomputeAt: state.lastFullRecomputeAt,
                    fullRecomputeStartedAt: state.fullRecomputeStartedAt)
                await store.beginAggregateFullRecompute(
                    configID: configID, at: now, resumeThrough: legacyCursor)
                state = await store.aggregateState(for: configID)
            }
            resolvedWindow = AggregateSchedule.window(
                startDate: anchor,
                computedThrough: state.computedThrough,
                fullPass: fullPass,
                fullRecomputeThrough: state.fullRecomputeThrough,
                settleDelay: agg.settleDelay,
                now: now,
                bucketing: bucketing,
                intervalSeconds: agg.approximateIntervalSeconds
            )
        }
        // Nil also when the limit leaves nothing readable in the window: a
        // full pass is then as complete as HealthKit allows.
        let clampedWindow = resolvedWindow.flatMap {
            ReadableHistory.clampAggregateWindow($0, readableSince: readable.since, bucketing: bucketing)
        }
        guard let window = clampedWindow else {
            // The last acked chunk may already have reached this run's settled
            // boundary before interruption, leaving only completion to persist.
            if fullPass {
                await store.markAggregateFullRecompute(configID: configID, at: now)
            }
            return // nothing settled yet
        }
        let clamped = window.from != resolvedWindow?.from

        let chunks = bucketing.chunks(from: window.from, to: window.to)
        guard !chunks.isEmpty else {
            if fullPass { await store.markAggregateFullRecompute(configID: configID, at: now) }
            return
        }

        let runStart = ContinuousClock.now
        var totalBuckets = 0

        do {
            for chunk in chunks {
                try Task.checkCancellation()
                totalBuckets += try await computeAndUploadAggregateChunk(
                    config: agg, configID: configID,
                    unit: descriptor.unit, reason: reason, transport: transport,
                    calendar: calendar, anchor: anchor, chunk: chunk, pass: pass
                )
                notifyChanged()
            }
            if fullPass {
                await store.markAggregateFullRecompute(configID: configID, at: now)
            }
            let elapsed = (ContinuousClock.now - runStart).seconds
            await eventLog.log(
                .info, type: typeID,
                "Aggregate \(agg.summaryLabel): \(totalBuckets) buckets in \(chunks.count) batch(es), \(String(format: "%.1f", elapsed))s\(fullPass ? " (full recompute)" : "")\(pass == .priority ? " (recent window)" : "")\(clamped ? " (from \(window.from.formatted(date: .abbreviated, time: .omitted)) — Health access is limited to recent history)" : "")"
            )
        } catch {
            switch PassFailure(error) {
            case .authorizationNotDetermined:
                await store.recordAggregateError(configID: configID, error: SyncError.authorizationNotDetermined)
                await eventLog.log(.error, type: typeID, "Aggregate \(agg.summaryLabel): Health access not determined")
            case .databaseLocked:
                noteHealthDatabaseLocked()
                // Device locked — expected in background; watermark untouched.
                await eventLog.log(.warn, type: typeID, "Aggregate \(agg.summaryLabel): Health database locked — will retry on next wake")
            case .cancelled:
                // Background time expired mid-series; the watermark sits at the last
                // acked chunk and the next wake resumes. Not a failure.
                await eventLog.log(.debug, type: typeID, "Aggregate \(agg.summaryLabel): cancelled — will resume on next wake")
            case .failed(let error):
                await store.recordAggregateError(configID: configID, error: error)
                await eventLog.log(.error, type: typeID, "Aggregate \(agg.summaryLabel) failed: \(error)")
            }
        }
        notifyChanged()
    }

    /// Compute and upload one aggregate chunk through `AggregateQuery`. On
    /// HealthKit's internal missing data-source error the query layer retries
    /// over smaller bucket-aligned windows; each successful subwindow is
    /// uploaded and acked here, so the watermark advances subwindow by
    /// subwindow and an interrupted recovery resumes from the last ack.
    private func computeAndUploadAggregateChunk(
        config: AggregateConfig,
        configID: UUID,
        unit: HKUnit?,
        reason: SyncReason,
        transport: any SyncTransport,
        calendar: Calendar,
        anchor: Date,
        chunk: DateInterval,
        pass: AggregatePass
    ) async throws -> Int {
        let observation = try await transport.beginSourceObservation(.aggregate(config))
        return try await AggregateQuery.bucketsRecovering(
            for: config, unit: unit, canonicalAnchor: anchor, calendar: calendar,
            chunk: chunk, healthStore: healthStore,
            onRecoveryStart: { _ in
                await self.eventLog.log(
                    .warn, type: config.typeIdentifier,
                    "Aggregate \(config.summaryLabel): HealthKit statistics data-source cache unavailable — retrying with smaller HealthKit statistics windows"
                )
            },
            onChunk: { rows, window in
                try await self.uploadAggregateRows(
                    rows, config: config, configID: configID, reason: reason,
                    transport: transport, chunk: window, pass: pass,
                    sourceObservations: observation.map { [$0] }
                )
            }
        )
    }

    func uploadAggregateRows(
        _ rows: [AggregateSampleRow],
        config: AggregateConfig,
        configID: UUID,
        reason: SyncReason,
        transport: any SyncTransport,
        chunk: DateInterval,
        pass: AggregatePass,
        sourceObservations: [UUID]?
    ) async throws {
        if sourceObservations?.isEmpty == false, !rows.contains(where: { $0.value != nil }) {
            throw SyncError.readableHistoryUnknown(config.typeIdentifier)
        }
        let batch = SyncBatch(
            deviceID: store.deviceID,
            type: config.typeIdentifier,
            reason: reason,
            samples: [],
            deletions: [],
            aggregates: rows,
            sourceObservations: sourceObservations
        )
        let uploadResult = try await transport.upload(batch)
        switch pass {
        case .scheduled:
            await store.recordAggregateUpload(
                configID: configID,
                newComputedThrough: chunk.end,
                buckets: rows.count,
                bytes: uploadResult.bytesSent
            )
        case .priority:
            // Counters only. `chunk.end` here is near *now*, so recording it as
            // progress would tell a full pass that everything up to the recent
            // window is already done and it would skip the entire history —
            // the aggregate twin of reusing a raw type's anchor for a
            // date-bounded query.
            await store.recordAggregateUploadWithoutWatermark(
                configID: configID,
                buckets: rows.count,
                bytes: uploadResult.bytesSent
            )
        }
        await reportWakeBatch(
            type: "agg:\(config.typeIdentifier)", samples: rows.count,
            deletions: 0, bytes: uploadResult.bytesSent)
    }

    // MARK: - Debug matrix validation

    /// Executes a one-day statistics query for every quantity type × allowed
    /// function and returns failure descriptions. The option×style legality
    /// check happens inside HealthKit at query time and surfaces as an ObjC
    /// exception (process crash) rather than a Swift error — so run this from
    /// the debug UI after catalog changes or on a new iOS release: surviving
    /// the call IS the pass signal for legality; returned strings are softer
    /// errors (auth etc.) for context.
    public func validateAggregateFunctionMatrix() async -> [String] {
        var failures: [String] = []
        var combos = 0
        let end = Date()
        let start = end.addingTimeInterval(-86_400)
        for descriptor in HealthTypeCatalog.quantityTypes {
            let functions = HealthTypeCatalog.allowedAggregateFunctions(for: descriptor.identifier)
            if functions.isEmpty {
                failures.append("\(descriptor.identifier): no allowed functions (unknown aggregation style)")
                continue
            }
            guard let quantityType = descriptor.sampleType as? HKQuantityType else {
                failures.append("\(descriptor.identifier): unavailable on this runtime")
                continue
            }
            for function in functions {
                combos += 1
                let queryDescriptor = HKStatisticsCollectionQueryDescriptor(
                    predicate: HKSamplePredicate.quantitySample(
                        type: quantityType,
                        predicate: HKQuery.predicateForSamples(
                            withStart: start, end: end, options: .strictStartDate)
                    ),
                    options: function.statisticsOption,
                    anchorDate: start,
                    intervalComponents: DateComponents(hour: 1)
                )
                do {
                    _ = try await queryDescriptor.result(for: healthStore)
                } catch let error as HKError where error.code == .errorAuthorizationNotDetermined {
                    continue // query was accepted; auth just isn't granted — legality verified
                } catch {
                    failures.append("\(descriptor.identifier) × \(function.rawValue): \(error)")
                }
            }
        }
        await eventLog.log(
            failures.isEmpty ? .info : .error,
            "Aggregate matrix validation: \(combos) combos executed, \(failures.count) failures"
        )
        return failures
    }
}
