import Foundation
import HealthKit

/// HealthKit's activity predicates and the wire's YYYY-MM-DD use Gregorian
/// dates, independent of the phone's preferred calendar. Keep its time zone.
enum ActivitySummaryCalendar {
    static func gregorian(_ calendar: Calendar) -> Calendar {
        var result = Calendar(identifier: .gregorian)
        result.timeZone = calendar.timeZone
        return result
    }

    static func localDate(for date: Date, calendar: Calendar) -> String {
        let comps = gregorian(calendar).dateComponents([.year, .month, .day], from: date)
        return String(format: "%04d-%02d-%02d", comps.year ?? 0, comps.month ?? 0, comps.day ?? 0)
    }
}

// Daily activity-summary (HKActivitySummary — the activity rings) export.
//
// Activity summaries are not HKSamples: they have no UUID, there is one per
// local calendar day, and the current day keeps changing as more activity is
// recorded. So like aggregates (see AggregateSync) they ride their own
// `{"activitySummary":...}` NDJSON line, the server upserts them keyed on date,
// and progress is a day watermark — not an HKQueryAnchor — advanced only after
// the server acks the upload (anchor-after-ack). Each run re-covers a trailing
// lookback (so recent days and today self-heal) plus a ~monthly full pass that
// repairs older edits. There is no observer/background-delivery for activity
// summaries, so they refresh on foreground/periodic/scheduled syncs plus an
// opportunistic hourly refresh at the tail of observer wakes
// (`refreshActivitySummaryIfStale`). They are the *first* phase of
// `syncAllEnabled`: one row per local day is seconds of work and the first
// thing a dashboard can show, so a long first backfill no longer runs to
// completion before any ring data exists.

extension HealthSyncEngine {
    /// Export activity summaries. Overlap-guarded like `sync(type:)`.
    public func syncActivitySummary(reason: SyncReason = .incremental) async {
        guard !configurationMutationInProgress else { return }
        let key = "activitySummary"
        guard !activeSyncs.contains(key) else {
            pendingResync.insert(key)
            return
        }
        activeSyncs.insert(key)
        defer { activeSyncs.remove(key) }

        var nextReason = reason
        repeat {
            await runActivitySummarySync(reason: nextReason)
            nextReason = .incremental
        } while pendingResync.remove(key) != nil
    }

    /// Refresh the rings if they look stale, cheaply enough to hang off any
    /// wake that happens to have HealthKit available.
    ///
    /// Rings have no observer of their own, so their only refresh was
    /// `syncAllEnabled` — and the scheduled caller of that is the
    /// BGProcessingTask, which iOS runs while the device is idle and therefore
    /// locked. Every ring refresh from 2026-08-11 onward failed with
    /// `errorDatabaseInaccessible`, and the server's newest ring row was three
    /// days old *before* the ingest outage even started. An observer wake is by
    /// definition a moment when HealthKit is readable, so use those instead.
    ///
    /// The interval guard matters: observer wakes are frequent, today's ring
    /// mutates all day, and without it a burst of wakes would re-upload the
    /// same day over and over.
    func refreshActivitySummaryIfStale(minimumInterval: TimeInterval = 3600) async {
        guard let configurationUse = beginConfigurationUse() else { return }
        defer { activeSyncs.remove(configurationUse) }
        let config = await store.configuration
        guard config.enabledTypes.contains(HealthTypeCatalog.activitySummaryIdentifier) else { return }
        let state = await store.activitySummaryState
        if let last = state.lastComputedAt,
           Date().timeIntervalSince(last) < minimumInterval { return }
        await syncActivitySummary(reason: .incremental)
    }

    private func runActivitySummarySync(reason: SyncReason) async {
        guard await allowsSync(types: [HealthTypeCatalog.activitySummaryIdentifier]) else { return }
        await ensureTransport()
        let typeID = HealthTypeCatalog.activitySummaryIdentifier
        guard let transport else {
            await eventLog.log(.error, type: typeID, "No transport configured — set server URL and token")
            return
        }

        let calendar = ActivitySummaryCalendar.gregorian(.current)
        let config = await store.configuration
        let startDay = calendar.startOfDay(for: config.startDate)
        var state = await store.activitySummaryState
        let now = Date()

        // iOS 27 limited history access, as for aggregates (`ReadableHistory`):
        // the rings upsert by day, so a day HealthKit will not let the app
        // read must not be uploaded as if it had no activity. The window
        // starts at the first whole readable day; the date is kept with the
        // rings, and a widened grant recomputes every day from the start date.
        let lookup = await readableLimit(for: typeID, recorded: state.readableSince)
        let readable = ReadableLimit(
            since: ReadableHistory.effectiveLimit(lookup.since, readingFrom: startDay),
            isFresh: lookup.isFresh)
        if ReadableHistory.isSupported, !readable.isFresh, readable.since == nil {
            // As for aggregates: locked skips quietly, anything else is
            // recorded so the rings do not just go quiet.
            if await isHealthDataAccessible() {
                await store.recordActivitySummaryError(
                    error: SyncError.readableHistoryUnknown(
                        HealthTypeCatalog.descriptor(for: typeID)?.displayName ?? typeID))
                await eventLog.log(
                    .warn, type: typeID,
                    "Activity rings: could not tell how much Health history is readable — skipped, will retry")
            } else {
                await eventLog.log(.warn, type: typeID, "Activity rings: Health database locked — will retry on next wake")
            }
            return
        }
        if readable.isFresh {
            switch ReadableHistory.change(from: state.readableSince, to: readable.since) {
            case .widened:
                await store.resetActivitySummary()
                await eventLog.log(
                    .info, type: typeID,
                    "Activity rings: Health access widened — recomputing every day from the start date")
                fallthrough
            case .narrowed:
                let since = readable.since
                await store.updateActivitySummary { $0.readableSince = since }
                state = await store.activitySummaryState
            case .unchanged:
                break
            }
        }
        // The last day to read: today, or — for an export's engine, which
        // reads up to `readEnd` — the local day before the bound, since the
        // bound's own day is not over as of the bound.
        var today = calendar.startOfDay(for: now)
        if let readEnd,
           let lastDay = calendar.date(byAdding: .day, value: -1, to: calendar.startOfDay(for: readEnd)) {
            today = min(today, lastDay)
        }

        activities[typeID] = state.computedThrough == nil ? .backfilling : .syncing
        defer { activities[typeID] = nil }
        notifyChanged()

        let fullPass = state.computedThrough == nil
            || state.lastFullRecomputeAt.map {
                now.timeIntervalSince($0) > AggregateSchedule.fullRecomputeInterval
            } ?? true

        // Window [from, today]: today is always included (rings change live) and
        // a trailing lookback re-covers recent days that late Watch data may have
        // changed. A full pass goes back to the start date.
        let lookback = AggregateSchedule.lookback(intervalSeconds: 86_400)
        var from: Date
        if !fullPass, let computedThrough = state.computedThrough {
            from = max(startDay, calendar.startOfDay(
                for: computedThrough.addingTimeInterval(-lookback)))
        } else {
            from = startDay
        }
        if let since = readable.since {
            from = max(from, ReadableHistory.firstWholeDay(atOrAfter: since, calendar: calendar))
            guard from <= today else {
                // Not a readable day left in the window. Nothing to send, and
                // nothing to do again until tomorrow — record it as done, as
                // for an empty window below.
                await store.recordActivitySummaryUpload(newComputedThrough: today, days: 0, bytes: 0)
                return
            }
        }
        guard from <= today else { return }

        let runStart = ContinuousClock.now
        do {
            let observation = try await transport.beginSourceObservation(.activitySummary)
            let rows = try await fetchActivitySummaries(from: from, to: today, calendar: calendar)
            try await uploadActivitySummaryRows(rows, through: today, reason: reason,
                transport: transport, sourceObservations: observation.map { [$0] })
            // An empty *full* pass is not a completed one: HealthKit answers a
            // denied read with an empty result, not an error, so marking it
            // complete here would leave everything older than the lookback
            // unsynced until the next monthly pass after access is granted.
            // Leaving the mark unset keeps each run a full pass (one cheap query)
            // until the first real rows arrive.
            if fullPass, !rows.isEmpty {
                await store.markActivitySummaryFullRecompute(at: now)
            }
            let elapsed = (ContinuousClock.now - runStart).seconds
            await eventLog.log(
                .info, type: typeID,
                "Activity rings: \(rows.count) day(s), \(String(format: "%.1f", elapsed))s\(fullPass ? " (full recompute)" : "")")
            notifyChanged()
        } catch {
            switch PassFailure(error) {
            case .authorizationNotDetermined:
                await store.recordActivitySummaryError(error: SyncError.authorizationNotDetermined)
                await eventLog.log(.error, type: typeID, "Activity rings: Health access not determined")
            case .databaseLocked:
                noteHealthDatabaseLocked()
                // Device locked — expected in background; watermark untouched.
                await eventLog.log(.warn, type: typeID, "Activity rings: Health database locked — will retry on next wake")
            case .cancelled:
                await eventLog.log(.debug, type: typeID, "Activity rings: cancelled — will resume on next wake")
            case .failed(let error):
                await store.recordActivitySummaryError(error: error)
                await eventLog.log(.error, type: typeID, "Activity rings sync failed: \(error)")
            }
        }
        notifyChanged()
    }

    func uploadActivitySummaryRows(
        _ rows: [ActivitySummaryRow], through: Date, reason: SyncReason,
        transport: any SyncTransport, sourceObservations: [UUID]?
    ) async throws {
        let typeID = HealthTypeCatalog.activitySummaryIdentifier
        if sourceObservations?.isEmpty == false, !rows.contains(where: { row in
            row.moveKcal != nil || row.moveGoalKcal != nil || row.exerciseMin != nil
                || row.exerciseGoalMin != nil || row.standHours != nil || row.standGoalHours != nil
                || row.moveTimeMin != nil || row.moveTimeGoalMin != nil
        }) { throw SyncError.readableHistoryUnknown(typeID) }
        var bytes = 0
        if !rows.isEmpty {
            let batch = SyncBatch(deviceID: store.deviceID, type: typeID, reason: reason,
                samples: [], deletions: [], activitySummaries: rows, sourceObservations: sourceObservations)
            let result = try await transport.upload(batch)
            bytes = result.bytesSent
            await reportWakeBatch(type: typeID, samples: rows.count, deletions: 0, bytes: bytes)
        }
        await store.recordActivitySummaryUpload(newComputedThrough: through, days: rows.count, bytes: bytes)
    }

    /// One HKActivitySummaryQuery over [from, to] (inclusive of today), mapped to
    /// wire rows. HealthKit wants `DateComponents` carrying era/year/month/day
    /// and an explicit calendar for the date predicate.
    private func fetchActivitySummaries(
        from: Date, to: Date, calendar: Calendar
    ) async throws -> [ActivitySummaryRow] {
        let calendar = ActivitySummaryCalendar.gregorian(calendar)
        let units: Set<Calendar.Component> = [.era, .year, .month, .day]
        var startComps = calendar.dateComponents(units, from: from)
        startComps.calendar = calendar
        var endComps = calendar.dateComponents(units, from: to)
        endComps.calendar = calendar

        let predicate = HKQuery.predicate(forActivitySummariesBetweenStart: startComps, end: endComps)
        let descriptor = HKActivitySummaryQueryDescriptor(predicate: predicate)
        let summaries = try await descriptor.result(for: healthStore)
        return summaries.compactMap { Self.row(from: $0, calendar: calendar) }
    }

    /// Map one HKActivitySummary to a wire row. The project floor is iOS 17, so
    /// the iOS-16 optional `*Goal: HKQuantity?` accessors are used directly; a
    /// nil goal is sent as nil (the viewer falls back to a default goal).
    private nonisolated static func row(
        from summary: HKActivitySummary, calendar: Calendar
    ) -> ActivitySummaryRow? {
        let calendar = ActivitySummaryCalendar.gregorian(calendar)
        guard let date = calendar.date(from: summary.dateComponents(for: calendar)) else { return nil }
        let localDate = ActivitySummaryCalendar.localDate(for: date, calendar: calendar)

        let moveKcal = summary.activeEnergyBurned.doubleValue(for: .kilocalorie())
        let moveGoal = summary.activeEnergyBurnedGoal.doubleValue(for: .kilocalorie())
        let exerciseMin = summary.appleExerciseTime.doubleValue(for: .minute())
        let exerciseGoal = summary.exerciseTimeGoal?.doubleValue(for: .minute())
        let standHours = summary.appleStandHours.doubleValue(for: .count())
        let standGoal = summary.standHoursGoal?.doubleValue(for: .count())

        let moveMode: Int
        let moveTimeMin: Double?
        let moveTimeGoalMin: Double?
        if summary.activityMoveMode == .appleMoveTime {
            moveMode = 1
            moveTimeMin = summary.appleMoveTime.doubleValue(for: .minute())
            moveTimeGoalMin = summary.appleMoveTimeGoal.doubleValue(for: .minute())
        } else {
            moveMode = 0
            moveTimeMin = nil
            moveTimeGoalMin = nil
        }

        return ActivitySummaryRow(
            date: date,
            localDate: localDate,
            temporalContext: .deviceCurrent(
                for: date,
                timeZone: calendar.timeZone,
                confidence: "inferred"
            ),
            moveKcal: moveKcal, moveGoalKcal: moveGoal,
            exerciseMin: exerciseMin, exerciseGoalMin: exerciseGoal,
            standHours: standHours, standGoalHours: standGoal,
            moveMode: moveMode, moveTimeMin: moveTimeMin, moveTimeGoalMin: moveTimeGoalMin
        )
    }
}
