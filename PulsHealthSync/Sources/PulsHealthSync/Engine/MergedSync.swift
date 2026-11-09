import Foundation
import HealthKit

// Multi-type batching for incremental sync.
//
// `sync(type:)` uploads one type's page per request. That is right for a
// backfill, where every page is full and the four concurrent type-runs keep the
// network busy. It is badly wrong for observer-driven incremental sync, where
// production telemetry over 2026-06-14..2026-08-13 measured:
//
//   * 94% of sync wall time spent in upload, 6% in the HealthKit queries
//   * a median page of 7 samples / 1.2 KB that still cost ~1.5s of round trip
//   * 83% of pages carrying under 50 samples, moving 10.6% of the data while
//     consuming 79% of all upload time
//
// The cost is per-request, not per-sample, so the fix is fewer requests: fetch
// one page per type, pack the small ones together, and upload the packs.
//
// Anchor-after-ack is preserved exactly (see CLAUDE.md). A page is never split
// across batches, so each type's anchor advances after the single upload that
// carried its data was acked. When a batch fails, every anchor in it stays put
// and those types drop out of the run; the next run re-queries from the same
// anchor and re-sends the same samples, which is safe because every insert is
// ON CONFLICT DO NOTHING on sample UUID.
extension HealthSyncEngine {

    /// One anchored page per type, packed into as few uploads as the sample
    /// budget allows, repeating until every type is drained.
    ///
    /// Overlap semantics match `sync(type:)`: a type already syncing is not
    /// started again, it is marked for a follow-up run instead.
    func syncTypesMerged(_ ids: [String], reason: SyncReason) async {
        let claimed = claimTypes(ids)
        guard !claimed.isEmpty else { return }
        defer { for id in claimed { activeSyncs.remove(id) } }

        await sendRecentWindows(claimed, reason: reason)
        var round = claimed
        var nextReason = reason
        while !round.isEmpty {
            await runMergedSync(round, reason: nextReason)
            nextReason = .incremental
            // A cancelled run leaves the follow-ups marked for the next one
            // rather than spending them on passes that would stop at once.
            round = Task.isCancelled ? [] : claimed.filter { pendingResync.remove($0) != nil }
        }
    }

    /// The recent-window pass (`RecentSampleWindow`) over types the caller
    /// holds, ahead of their sweep. Only types still backfilling take part, so
    /// on a settled install this costs nothing; an engine built with
    /// `recentWindowFirst: false` skips it entirely.
    func sendRecentWindows(_ claimed: [String], reason: SyncReason) async {
        guard recentWindowFirst, !claimed.isEmpty, !Task.isCancelled else { return }
        await runMergedSync(claimed, reason: reason, pass: .recent)
    }

    // MARK: - One run

    private func runMergedSync(_ ids: [String], reason: SyncReason, pass: SweepPass = .main) async {
        await ensureTransport()
        guard let transport else {
            await eventLog.log(.error, "No transport configured — set server URL and token")
            return
        }
        guard await allowsSync(types: Set(ids)) else { return }
        let config = await store.configuration
        // A page is at most `batchSize`; keeping the pack budget at least that
        // large is what guarantees a page never has to be split across batches.
        let budget = max(config.maxMergedBatchSamples, config.batchSize)

        // In-memory cursor per type. Seeded from the persisted anchor and only
        // advanced past a page the server has acked, so a failed upload leaves
        // the type exactly where the last durable anchor put it.
        var cursors: [String: HKQueryAnchor?] = [:]
        // Where each type's query starts: the configured start date for the
        // sweep, the stream's fixed window start for the recent pass.
        var starts: [String: Date] = [:]
        var pending: [String] = []
        // Types sitting out a cooldown after a terminal refusal, and types
        // just back from one: those upload alone (`flush`), so a page the
        // server refuses again is pinned on its own type and does not hold
        // the rest of a pack back with it.
        var cooling: Set<String> = []
        var isolated: Set<String> = []
        let bypassCooldown = Self.bypassesCooldown(reason)
        let now = Date()
        for id in ids {
            guard let descriptor = HealthTypeCatalog.descriptor(for: id),
                  descriptor.sampleType != nil else {
                await eventLog.log(.error, type: id, "Unknown type identifier")
                continue
            }
            let state = await store.state(for: id)
            if state.isCoolingDown(at: now), !bypassCooldown {
                cooling.insert(id)
                continue
            }
            if state.terminalFailures != nil { isolated.insert(id) }
            switch pass {
            case .main:
                starts[id] = config.startDate
            case .recent:
                guard !state.backfillComplete,
                      let windowStart = state.recentWindowStart
                        ?? RecentSampleWindow.start(syncingFrom: config.startDate, now: now)
                else { continue }
                starts[id] = windowStart
            }
            cursors[id] = await storedAnchor(
                pass == .main ? state.anchorData : state.recentAnchorData, of: id, pass: pass)
            let isBackfill = !state.backfillComplete
            activities[id] = isBackfill ? .backfilling : .syncing
            pending.append(id)
        }
        if !cooling.isEmpty {
            await eventLog.log(
                .debug,
                "Skipped \(cooling.count) type(s) cooling down after the server refused an upload: \(cooling.sorted().joined(separator: ", "))")
        }
        guard !pending.isEmpty else { return }
        notifyChanged()

        let runStart = ContinuousClock.now
        var uploads = 0
        var totalSamples = 0
        var totalDeletions = 0
        /// Types HealthKit confirmed it had nothing more for, and whose last
        /// page (if any) was acked. Only these may be marked backfill-complete:
        /// a type dropped by a failed upload, a locked database, or
        /// cancellation has *not* been fully read, and claiming otherwise would
        /// permanently skip its remaining history.
        var drainedCleanly: Set<String> = []
        // Types where at least one page had samples that would not map. Such a
        // type must not be reported "backfill complete": we swept it, but we
        // uploaded nothing, and a green dashboard row would hide that.
        var droppedAnything: Set<String> = []

        while !pending.isEmpty, !Task.isCancelled {
            var carried: Set<String> = []
            // Pages held between fetch and upload, flushed once they add up to a
            // full batch. Fetching every pending type before uploading anything
            // would be simpler, but on a wide sweep (79 types, up to 1,000
            // samples each) that is tens of thousands of samples resident at
            // once — far too much for the memory a background wake gets. Here
            // the buffer only grows with real data and never holds much more
            // than one batch plus the wave being processed and the wave being
            // read.
            var buffer: [MergedPage] = []
            var buffered = 0

            let width = max(1, config.maxConcurrentTypes)
            var waves = stride(from: 0, to: pending.count, by: width).map {
                Array(pending[$0..<min($0 + width, pending.count)])
            }
            var pages = await fetchPages(
                waves.removeFirst(), cursors: cursors, starts: starts, config: config)
            while true {
                // Read the next wave while this one is packed and uploaded. Its
                // types are none of the buffered ones — within a round a type
                // is read once, and a round's last flush lands before the next
                // round reads anything — so no ack below can move a cursor it
                // was read from.
                let upcoming = Task.isCancelled || waves.isEmpty ? [] : waves.removeFirst()
                let readFrom = cursors
                let readStarts = starts
                async let nextPages = fetchPages(
                    upcoming, cursors: readFrom, starts: readStarts, config: config)

                for page in pages.compactMap({ $0 }) {
                    if page.dropped > 0 {
                        // SampleMapper.map returns nil when the quantity is not
                        // compatible with the catalog's unitString — a whole-type
                        // property — so one wrong unit makes every sample of that
                        // type unmappable. Silently, until now.
                        droppedAnything.insert(page.identifier)
                        noteUnmappableSamples(page.dropped, type: page.identifier)
                        await eventLog.log(
                            .warn, type: page.identifier,
                            "Dropped \(page.dropped) of \(page.rawCount) samples that could not be mapped — check this type's unitString in HealthTypeCatalog"
                        )
                    }
                    // Only a page HealthKit returned nothing for means the type is
                    // drained: persist the final anchor so the next run starts
                    // here, and clear any stale error — the query just succeeded,
                    // so an older authorization failure no longer holds.
                    if page.isRawEmpty {
                        await persistCursor(
                            of: page, pass: pass, windowStart: starts[page.identifier],
                            clearingError: true)
                        drainedCleanly.insert(page.identifier)
                    } else if page.isEmpty {
                        // HealthKit returned a page, but nothing on it survived
                        // mapping. There is nothing to upload and nothing to ack,
                        // so advance past it rather than re-querying the same
                        // unmappable samples forever — but let HealthKit's own
                        // short-page signal say whether more remain.
                        await persistCursor(
                            of: page, pass: pass, windowStart: starts[page.identifier],
                            clearingError: false)
                        if page.drained {
                            drainedCleanly.insert(page.identifier)
                        }
                    } else {
                        buffer.append(page)
                        buffered += page.count
                    }
                }
                if buffered >= budget {
                    let flushed = await flush(
                        buffer, budget: budget, keepPartial: true, isolating: isolated,
                        reason: reason, transport: transport, config: config, pass: pass,
                        starts: starts)
                    buffer = flushed.leftover
                    buffered = buffer.reduce(0) { $0 + $1.count }
                    uploads += flushed.uploads
                    totalSamples += flushed.samples
                    totalDeletions += flushed.deletions
                    carried.formUnion(flushed.carried)
                    drainedCleanly.formUnion(flushed.drained)
                    for (id, anchor) in flushed.cursors { cursors[id] = anchor }
                }
                pages = await nextPages
                if upcoming.isEmpty { break }
            }
            let flushed = await flush(
                buffer, budget: budget, keepPartial: false, isolating: isolated,
                reason: reason, transport: transport, config: config, pass: pass,
                starts: starts)
            uploads += flushed.uploads
            totalSamples += flushed.samples
            totalDeletions += flushed.deletions
            carried.formUnion(flushed.carried)
            drainedCleanly.formUnion(flushed.drained)
            for (id, anchor) in flushed.cursors { cursors[id] = anchor }

            // Only types whose page both acked and came back full go again.
            pending = pending.filter { carried.contains($0) }
        }

        for id in ids where activities[id] != .failed && !cooling.contains(id) {
            // The recent pass reads a window, so its drain says nothing about
            // the rest of the type's history.
            if pass == .main, drainedCleanly.contains(id), !droppedAnything.contains(id),
               !(await store.state(for: id)).backfillComplete {
                await store.markBackfillComplete(id)
                backfillRuns[id] = nil
            }
            activities[id] = .idle
        }
        let elapsed = (ContinuousClock.now - runStart).seconds
        if totalSamples + totalDeletions > 0 {
            let rate = Double(totalSamples) / max(elapsed, 0.001)
            await eventLog.log(
                .info,
                "\(reason.rawValue)\(pass == .recent ? " recent-window" : "") sync done: \(totalSamples) samples, \(totalDeletions) deletions across \(ids.count) type(s) in \(String(format: "%.1f", elapsed))s (\(Int(rate))/s, \(uploads) upload(s))"
            )
        }
        notifyChanged()
    }

    /// What one flush moved, returned rather than mutated in place: a nested
    /// async function that captured the run's counters across a suspension
    /// point is exactly the shape Swift 6 rejects as a data race.
    struct FlushResult {
        var leftover: [MergedPage] = []
        var uploads = 0
        var samples = 0
        var deletions = 0
        /// Acked and still has more pages waiting.
        var carried: Set<String> = []
        /// Acked and HealthKit had nothing further.
        var drained: Set<String> = []
        var cursors: [String: HKQueryAnchor] = [:]
    }

    /// Upload whole packs out of the buffer. `keepPartial` hands back a final
    /// under-budget pack so the next wave can top it up rather than sending a
    /// half-empty request; the last flush of a run takes it as-is. Pages of
    /// `isolating` types each go in a pack of their own (`packsToSend`).
    private func flush(
        _ buffer: [MergedPage], budget: Int, keepPartial: Bool, isolating isolated: Set<String>,
        reason: SyncReason, transport: SyncTransport, config: SyncConfiguration,
        pass: SweepPass, starts: [String: Date]
    ) async -> FlushResult {
        var result = FlushResult()
        guard !buffer.isEmpty else { return result }

        let planned = Self.packsToSend(
            buffer, budget: budget, keepPartial: keepPartial, isolating: isolated)
        let toSend = planned.send
        result.leftover = planned.leftover
        let acked = await uploadPacks(
            toSend, reason: reason, transport: transport, config: config,
            pass: pass, starts: starts)
        for (pack, ok) in zip(toSend, acked) where ok {
            result.uploads += 1
            for page in pack {
                result.samples += page.samples.count
                result.deletions += page.deletions.count
                result.cursors[page.identifier] = page.newAnchor
                if page.drained {
                    result.drained.insert(page.identifier)
                } else {
                    result.carried.insert(page.identifier)
                }
            }
        }
        return result
    }

    /// Upload `packs` up to `maxConcurrentTypes` at a time; the result says
    /// which were acked, in order. One request at a time left the phone idle
    /// for every round trip, which on a backlog is most of the run.
    ///
    /// Packs of one flush hold disjoint types — a type contributes one page per
    /// round — so no type ever has two uploads in flight, and each pack still
    /// advances only its own types' anchors, only on its own ack.
    func uploadPacks(
        _ packs: [[MergedPage]], reason: SyncReason,
        transport: SyncTransport, config: SyncConfiguration,
        pass: SweepPass = .main, starts: [String: Date] = [:]
    ) async -> [Bool] {
        var acked = Array(repeating: false, count: packs.count)
        await withTaskGroup(of: (Int, Bool).self) { group in
            var next = 0
            var inFlight = 0
            func addNext(_ group: inout TaskGroup<(Int, Bool)>) {
                guard next < packs.count else { return }
                let index = next
                let pack = packs[index]
                next += 1
                inFlight += 1
                group.addTask {
                    (index, await self.upload(
                        pack, reason: reason, transport: transport, config: config,
                        pass: pass, starts: starts))
                }
            }
            for _ in 0..<max(1, config.maxConcurrentTypes) { addNext(&group) }
            while inFlight > 0 {
                if let (index, ok) = await group.next() { acked[index] = ok }
                inFlight -= 1
                addNext(&group)
            }
        }
        return acked
    }

    /// Record `page`'s anchor on the stream `pass` advances, with no upload:
    /// HealthKit returned nothing, or nothing that would map.
    private func persistCursor(
        of page: MergedPage, pass: SweepPass, windowStart: Date?, clearingError: Bool
    ) async {
        await store.update(page.identifier) {
            switch pass {
            case .main:
                $0.anchorData = page.newAnchorData
            case .recent:
                $0.recentAnchorData = page.newAnchorData
                $0.recentWindowStart = windowStart
            }
            $0.lastSyncAt = Date()
            if clearingError {
                $0.lastError = nil
                $0.clearCooldown()
            }
        }
    }

    // MARK: - Fetch

    /// One page per type, at most `maxConcurrentTypes` queries in flight.
    /// A type that throws is logged, marked, and dropped from the run; it never
    /// takes the other types down with it.
    private func fetchPages(
        _ ids: [String], cursors: [String: HKQueryAnchor?], starts: [String: Date],
        config: SyncConfiguration
    ) async -> [MergedPage?] {
        var out: [MergedPage?] = []
        await withTaskGroup(of: MergedPage?.self) { group in
            var iterator = ids.makeIterator()
            var inFlight = 0
            func addNext(_ group: inout TaskGroup<MergedPage?>) {
                guard let id = iterator.next() else { return }
                inFlight += 1
                let anchor = cursors[id] ?? nil
                let start = starts[id] ?? config.startDate
                group.addTask {
                    await self.fetchPage(id, anchor: anchor, start: start, config: config)
                }
            }
            for _ in 0..<max(1, config.maxConcurrentTypes) { addNext(&group) }
            while inFlight > 0 {
                if let page = await group.next() { out.append(page) }
                inFlight -= 1
                addNext(&group)
            }
        }
        return out
    }

    /// One anchored page of `identifier` from `start` on, mapped and given the
    /// phase-1 enrichment, ready to upload. Throws what HealthKit threw; both
    /// sweeps read through here and handle the errors their own way.
    func queryPage(
        _ identifier: String, anchor: HKQueryAnchor?, start: Date, config: SyncConfiguration
    ) async throws -> MergedPage {
        guard let descriptor = HealthTypeCatalog.descriptor(for: identifier),
              let sampleType = descriptor.sampleType else {
            throw SyncError.unknownType(identifier)
        }
        // Per-kind cap (`maxPageSize`): an ECG or heartbeat page is enriched
        // in memory before upload, so it pages far smaller than the rest.
        let pageSize = descriptor.pageSize(batchSize: config.batchSize)
        let queryStart = ContinuousClock.now
        let predicate = HKSamplePredicate<HKSample>.sample(
            type: sampleType,
            // `readEnd` is nil for the app's engine; an export's engine bounds
            // every page here, the recent-window stream's included.
            predicate: HKQuery.predicateForSamples(
                withStart: await limitedSyncStart(start), end: readEnd, options: .strictStartDate
            )
        )
        let queryDescriptor = HKAnchoredObjectQueryDescriptor(
            predicates: [predicate], anchor: anchor, limit: pageSize
        )
        let result = try await queryDescriptor.result(for: healthStore)
        let queryDuration = (ContinuousClock.now - queryStart).seconds

        var samples = result.addedSamples.compactMap {
            SampleMapper.map($0, descriptor: descriptor)
        }
        // The raw sweep defers the expensive per-workout route/series fetches to
        // the dedicated route/stream phases (see WorkoutEnrichmentSync) so
        // basic data isn't starved waiting on enrichment queries. The workout
        // row (incl. enhanced stats/events/activities/effort) and the profile
        // line still go now; routes/series arrive in later batches keyed on the
        // workout UUID — fully idempotent server-side.
        let enrichment = try await enrich(
            &samples, from: result.addedSamples, descriptor: descriptor,
            includeRoutes: config.includeWorkoutRoutes,
            includeEnhanced: config.includeWorkoutEnhancedData,
            userProfile: config.userProfilePayload,
            deferEnrichment: true
        )
        let deletions = result.deletedObjects.map {
            SyncDeletion(uuid: $0.uuid, type: identifier)
        }
        // Compare raw result counts, not mapped counts: mapping can drop
        // samples, and a short raw page is what proves HealthKit is drained.
        let rawCount = result.addedSamples.count + result.deletedObjects.count
        return MergedPage(
            identifier: identifier,
            samples: samples,
            deletions: deletions,
            newAnchor: result.newAnchor,
            newAnchorData: try encodeAnchor(result.newAnchor),
            enrichment: enrichment,
            queryDuration: queryDuration,
            drained: rawCount < pageSize,
            rawCount: rawCount,
            dropped: result.addedSamples.count - samples.count
        )
    }

    private func fetchPage(
        _ identifier: String, anchor: HKQueryAnchor?, start: Date, config: SyncConfiguration
    ) async -> MergedPage? {
        guard HealthTypeCatalog.descriptor(for: identifier)?.sampleType != nil else { return nil }
        do {
            return try await queryPage(identifier, anchor: anchor, start: start, config: config)
        } catch {
            switch PassFailure(error) {
            case .authorizationNotDetermined:
                activities[identifier] = .failed
                backfillRuns[identifier] = nil
                await store.recordError(identifier: identifier, error: SyncError.authorizationNotDetermined)
                await eventLog.log(.error, type: identifier, "Health access not determined — tap Grant Health Access on the Explore tab")
                return nil
            case .databaseLocked:
                noteHealthDatabaseLocked()
                // Device locked: the Health DB relocks ~10 min after lock. Expected
                // during background runs — not a failure; the anchor is untouched
                // and the next wake/foreground catches up.
                activities[identifier] = .idle
                await eventLog.log(.warn, type: identifier, "Health database locked (device locked) — will retry on next wake")
                return nil
            case .cancelled:
                // Background time expired; the anchor is untouched and the outer
                // loop polls Task.isCancelled. Not a failure.
                activities[identifier] = .idle
                return nil
            case .failed(let error):
                activities[identifier] = .failed
                backfillRuns[identifier] = nil
                await store.recordError(identifier: identifier, error: error)
                await eventLog.log(.error, type: identifier, "Sync failed: \(error)")
                return nil
            }
        }
    }

    // MARK: - Pack

    /// What one flush sends, and the under-budget pack it holds back for the
    /// next wave when `keepPartial` (never a page of an isolated type).
    ///
    /// A type whose last upload was refused terminally
    /// (`TypeSyncState.terminalFailures`) travels alone: a merged pack the
    /// server refuses cools every type in it, so the first try after the
    /// cooldown has to say which page it was. The rest pack as usual. Either
    /// way no page is split, so each page is still tied to exactly one ack.
    static func packsToSend(
        _ buffer: [MergedPage], budget: Int, keepPartial: Bool, isolating isolated: Set<String>
    ) -> (send: [[MergedPage]], leftover: [MergedPage]) {
        var send = pack(buffer.filter { !isolated.contains($0.identifier) }, budget: budget)
        var leftover: [MergedPage] = []
        if keepPartial, let last = send.last,
           last.reduce(0, { $0 + $1.count }) < budget {
            send.removeLast()
            leftover = last
        }
        send += buffer.filter { isolated.contains($0.identifier) }.map { [$0] }
        return (send, leftover)
    }

    /// Greedily fill batches to `budget` samples+deletions, never splitting a
    /// page. A page at or over the budget travels alone rather than being cut,
    /// which is what keeps one page tied to exactly one ack.
    static func pack(_ pages: [MergedPage], budget: Int) -> [[MergedPage]] {
        var packs: [[MergedPage]] = []
        var current: [MergedPage] = []
        var currentCount = 0
        // Largest first so small pages fill the gaps left by big ones instead of
        // each starting a batch of its own.
        for page in pages.sorted(by: { $0.count > $1.count }) {
            if !current.isEmpty, currentCount + page.count > budget {
                packs.append(current)
                current = []
                currentCount = 0
            }
            current.append(page)
            currentCount += page.count
        }
        if !current.isEmpty { packs.append(current) }
        return packs
    }

    // MARK: - Upload

    /// Upload one pack and, on ack, advance every contained type's anchor.
    /// Returns false when the upload failed, in which case no anchor moved.
    private func upload(
        _ pack: [MergedPage], reason: SyncReason,
        transport: SyncTransport, config: SyncConfiguration,
        pass: SweepPass, starts: [String: Date]
    ) async -> Bool {
        guard !pack.isEmpty else { return true }

        do {
            var profile = pack.compactMap(\.enrichment.profile).first
            var observations: [UUID]?
            if let cached = profile {
                (profile, observations) = try await captureProfile(using: transport, legacyProfile: cached)
            }
            let batch = SyncBatch(
                deviceID: store.deviceID,
                // The header type is a label only — the server registers types from
                // the sample lines themselves and merely stores this in
                // `batches.type_identifier`. Name the biggest contributor so the
                // column stays meaningful for a single-type batch (unchanged from
                // before) and informative for a merged one.
                type: pack.max(by: { $0.count < $1.count })?.identifier ?? pack[0].identifier,
                reason: reason,
                samples: pack.flatMap(\.samples),
                deletions: pack.flatMap(\.deletions),
                routes: pack.flatMap(\.enrichment.routes),
                series: pack.flatMap(\.enrichment.series),
                profile: profile,
                sourceObservations: observations
            )

            let result = try await transport.upload(batch)
            // Split the measured cost across the pages that shared the request
            // so per-type stats stay comparable with the unmerged path. Bytes
            // go by share of the payload rather than evenly, so a 900-sample
            // page is not credited the same as the 7-sample page riding along
            // with it.
            let units = max(1, pack.reduce(0) { $0 + $1.count })
            // The server counts the batch as a whole, so its accepted/duplicate
            // counts belong to a type only when the pack held one page.
            let receipt = pack.count == 1 ? result.receipt : nil
            for page in pack {
                let share = result.bytesSent * page.count / units
                let dates = page.samples.map(\.start)
                let dateRange = dates.isEmpty ? nil : (dates.min()! ... dates.max()!)
                let duration = page.queryDuration + result.duration / Double(pack.count)
                switch pass {
                case .main:
                    let latency: TimeInterval? = (reason == .incremental)
                        ? page.samples.map { Date().timeIntervalSince($0.end) }.min()
                        : nil
                    await store.recordUploadedBatch(
                        identifier: page.identifier,
                        newAnchorData: page.newAnchorData,
                        samples: page.samples.count,
                        deletions: page.deletions.count,
                        bytes: share,
                        sampleDateRange: dateRange,
                        duration: duration,
                        latency: latency,
                        receipt: receipt
                    )
                    backfillRuns[page.identifier]?.samplesThisRun += page.samples.count
                case .recent:
                    await store.recordRecentWindowUpload(
                        identifier: page.identifier,
                        newAnchorData: page.newAnchorData,
                        windowStart: starts[page.identifier] ?? config.startDate,
                        bytes: share,
                        sampleDateRange: dateRange,
                        duration: duration
                    )
                }
                await reportWakeBatch(
                    type: page.identifier, samples: page.samples.count,
                    deletions: page.deletions.count, bytes: share)
            }
            let types = pack.count == 1
                ? pack[0].identifier
                : "\(pack.count) types"
            await eventLog.log(
                .debug,
                "Merged upload: \(batch.samples.count) samples\(result.receipt?.sampleOutcome.map { " (\($0))" } ?? ""), \(batch.deletions.count) deletions from \(types) — \(String(format: "%.2f", result.duration))s (\(result.bytesSent) B)"
            )
            notifyChanged()
            return true
        } catch is CancellationError {
            // Anchors stay put for every type in the pack (nothing was acked);
            // background time simply ran out. The next wake re-queries them.
            for page in pack { activities[page.identifier] = .idle }
            await eventLog.log(.debug, "Merged upload of \(pack.count) type(s) cancelled — will resume on next wake")
            notifyChanged()
            return false
        } catch {
            // Anchors stay put for every type in the pack; the pages will be
            // re-queried and re-sent next run.
            for page in pack {
                activities[page.identifier] = .failed
                await store.recordError(identifier: page.identifier, error: error)
            }
            await eventLog.log(
                .error,
                "Merged upload of \(pack.count) type(s) failed: \(error)"
            )
            notifyChanged()
            return false
        }
    }
}

/// One anchored-query page, held until it can be packed into an upload.
struct MergedPage {
    let identifier: String
    var samples: [SyncSample]
    var deletions: [SyncDeletion]
    let newAnchor: HKQueryAnchor
    let newAnchorData: Data?
    let enrichment: HealthSyncEngine.WorkoutEnrichment
    let queryDuration: TimeInterval
    /// HealthKit returned a short page: nothing more for this type right now.
    let drained: Bool
    /// How many objects HealthKit returned, before mapping. `isEmpty` is a
    /// mapped-count question and `count` a mapped-count answer; this is the
    /// raw one, and it is what may be used to decide the type is drained.
    let rawCount: Int
    /// Samples HealthKit returned that `SampleMapper` could not convert —
    /// a unit-incompatible quantity, a wrong-class sample.
    let dropped: Int

    var count: Int { samples.count + deletions.count }
    var isEmpty: Bool { samples.isEmpty && deletions.isEmpty }
    /// HealthKit returned nothing at all, as opposed to returning samples that
    /// did not survive mapping. Only this means the type has no more data.
    var isRawEmpty: Bool { rawCount == 0 }
}
