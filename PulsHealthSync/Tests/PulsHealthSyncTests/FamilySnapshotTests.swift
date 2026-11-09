import Foundation
import Testing
@testable import PulsHealthSync

@Suite struct FamilySnapshotTests {
    actor Destination: SyncTransport {
        let observation = UUID()
        let store: SyncStateStore?
        let fails: Bool
        private(set) var queries = 0
        private(set) var batches: [SyncBatch] = []

        init(store: SyncStateStore? = nil, fails: Bool = false) {
            self.store = store
            self.fails = fails
        }

        func beginSourceObservation(_ query: SourceSnapshotQuery) async throws -> UUID? {
            queries += 1
            if let store {
                var config = await store.configuration
                config.userName = "fresh-after-query"
                await store.setConfiguration(config)
            }
            return observation
        }

        func upload(_ batch: SyncBatch) async throws -> UploadResult {
            batches.append(batch)
            if fails { throw TransportError.invalidAcknowledgement }
            return UploadResult(bytesSent: 100, duration: 0)
        }
    }

    private func http(_ handler: @escaping MockURLProtocol.Handler) -> HTTPSyncTransport {
        let host = "\(UUID().uuidString.lowercased()).test"
        MockURLProtocol.register(host: host, handler)
        return HTTPSyncTransport(baseURL: URL(string: "https://\(host)")!, authToken: "synthetic",
            userID: "00000000-0000-4000-8000-000000000001", maxRetries: 0, session: MockURLProtocol.session())
    }

    @Test func observationRequestCarriesVerifiedConnectionAndOnlyQueryFields() async throws {
        let id = UUID()
        let recorder = RequestRecorder()
        let transport = http { request in
            recorder.append(request)
            return .http(200, Data("{\"observation_id\":\"\(id)\"}".utf8))
        }
        let config = AggregateConfig(typeIdentifier: "HKQuantityTypeIdentifierHeartRate", function: .average,
            intervalValue: 2, intervalUnit: .hour, deviceFilter: .watch)
        #expect(try await transport.beginSourceObservation(.aggregate(config)) == id)
        let item = try #require(recorder.all.first)
        #expect(item.request.url?.path == "/v1/source-observations")
        #expect(item.request.httpMethod == "POST")
        #expect(item.request.value(forHTTPHeaderField: "Authorization") == "Bearer synthetic")
        #expect(item.request.value(forHTTPHeaderField: "X-User-ID") == transport.userID)
        #expect(item.request.value(forHTTPHeaderField: PulsProtocol.headerField) == "1")
        let fields = try #require(JSONSerialization.jsonObject(with: item.body) as? [String: Any])
        #expect(Set(fields.keys) == ["kind", "type", "func", "intervalValue", "intervalUnit", "deviceFilter"])
        #expect(fields["type"] as? String == config.typeIdentifier)
        #expect(fields["func"] as? String == "average")
        #expect(fields["intervalValue"] as? Int == 2)
        #expect(fields["intervalUnit"] as? String == "hour")
        #expect(fields["deviceFilter"] as? String == "watch")
        for query in [SourceSnapshotQuery.profile, .activitySummary] {
            let data = try JSONEncoder().encode(query)
            let fields = try #require(JSONSerialization.jsonObject(with: data) as? [String: String])
            #expect(Set(fields.keys) == ["kind"])
            #expect(["profile", "activity_summary"].contains(fields["kind"] ?? ""))
        }
    }

    @Test func observationFallbackIsLimitedToLegacyResponses() async throws {
        for status in [404, 405] {
            #expect(try await http { _ in .http(status, Data()) }.beginSourceObservation(.profile) == nil)
        }
        #expect(try await http { _ in .http(200, Data("{}".utf8)) }.beginSourceObservation(.profile) == nil)
        for body in ["", "[]", "{\"observation_id\":null}", "{\"observation_id\":\"invalid\"}", "{\"unexpected\":true}"] {
            do {
                _ = try await http { _ in .http(200, Data(body.utf8)) }.beginSourceObservation(.profile)
                Issue.record("Malformed source observation was accepted")
            } catch TransportError.invalidAcknowledgement {}
        }
        do {
            _ = try await http { _ in .http(401, Data("{}".utf8)) }.beginSourceObservation(.profile)
            Issue.record("Unauthorized query fell back to legacy mode")
        } catch TransportError.serverError(let status, _) { #expect(status == 401) }
    }

    @Test func instrumentedDestinationForwardsObservation() async throws {
        let destination = Destination()
        let wrapper = InstrumentedTransport(wrapping: destination)
        #expect(try await wrapper.beginSourceObservation(.profile) == destination.observation)
        #expect(await destination.queries == 1)
        #expect(try await DryRunTransport().beginSourceObservation(.profile) == nil)
    }

    @Test func wireRetainsObservationAndSourceCaptureTime() throws {
        let id = UUID()
        let captured = Date(timeIntervalSince1970: 1_767_225_600)
        let batch = SyncBatch(deviceID: "synthetic", type: "mixed", reason: .manual,
            samples: [], deletions: [], profile: ProfilePayload(capturedAt: captured), sourceObservations: [id])
        let lines = try BatchSerializer.ndjson(for: batch).split(separator: 0x0a)
        let header = try JSONDecoder.puls.decode(BatchSerializer.Header.self, from: Data(lines[0]))
        #expect(header.sourceObservations == [id])
        #expect(header.profileCount == 1)
        let profileLine = try #require(JSONSerialization.jsonObject(with: Data(lines[1])) as? [String: Any])
        let profile = try #require(profileLine["profile"] as? [String: Any])
        #expect(profile["capturedAt"] as? Double == 1_767_225_600_000)
        #expect(profile["name"] is NSNull)
        #expect(ProfilePayload(capturedAt: captured).isEmpty)
        let legacy = SyncBatch(deviceID: "legacy", type: "probe", reason: .manual, samples: [], deletions: [])
        let legacyHeader = try #require(BatchSerializer.ndjson(for: legacy).split(separator: 0x0a).first)
        #expect(!String(decoding: legacyHeader, as: UTF8.self).contains("sourceObservations"))
        #expect(!String(decoding: try JSONEncoder.puls.encode(ProfilePayload()), as: UTF8.self).contains("capturedAt"))
    }

    @Test func profileReadHappensAfterObservationAndDoesNotUseCachedValues() async throws {
        let engine = ConcurrentUploadTests().makeEngine()
        let destination = Destination(store: engine.store)
        await engine.setTransport(destination)
        let before = Date()
        try await engine.syncProfile()
        let batch = try #require(await destination.batches.first)
        #expect(batch.profile?.name == "fresh-after-query")
        #expect(try #require(batch.profile?.capturedAt) >= before)
        #expect(batch.sourceObservations == [destination.observation])
        #expect(await destination.queries == 1)
    }

    @Test func mergedWorkoutCapturesOneFreshProfileForTheWholePack() async throws {
        let fixture = ConcurrentUploadTests()
        let engine = fixture.makeEngine()
        let destination = Destination(store: engine.store)
        var first = fixture.page("workout", samples: 1)
        first.enrichment.profile = ProfilePayload(name: "cached-before-query")
        var second = fixture.page("other", samples: 1)
        second.enrichment.profile = ProfilePayload(name: "another-old-cache")
        let config = await engine.store.configuration
        #expect(await engine.uploadPacks([[first, second]], reason: .backfill, transport: destination, config: config) == [true])
        let batch = try #require(await destination.batches.first)
        #expect(batch.profile?.name == "fresh-after-query")
        #expect(batch.sourceObservations == [destination.observation])
        #expect(await destination.queries == 1)
        #expect(await engine.store.state(for: "workout").anchorData == Data([1]))
        #expect(await engine.store.state(for: "other").anchorData == Data([1]))
    }

    @Test func unmarkedSnapshotAcknowledgementMovesNoPackedAnchor() async throws {
        let id = UUID()
        let transport = http { request in
            if request.request.url?.path == "/v1/source-observations" {
                return .http(200, Data("{\"observation_id\":\"\(id)\"}".utf8))
            }
            return .http(200, Data("{}".utf8))
        }
        let fixture = ConcurrentUploadTests()
        let engine = fixture.makeEngine()
        var page = fixture.page("workout", samples: 1)
        page.enrichment.profile = ProfilePayload(name: "cached")
        let config = await engine.store.configuration
        #expect(await engine.uploadPacks([[page, fixture.page("other", samples: 1)]], reason: .backfill,
            transport: transport, config: config) == [false])
        for type in ["workout", "other"] {
            let state = await engine.store.state(for: type)
            #expect(state.anchorData == nil)
            #expect(state.totalBatchesUploaded == 0)
        }
    }

    @Test func unknownAggregateAndFailedAckKeepTheWatermark() async throws {
        let engine = ConcurrentUploadTests().makeEngine()
        let config = AggregateConfig(typeIdentifier: "HKQuantityTypeIdentifierHeartRate", function: .average)
        let start = Date(timeIntervalSince1970: 1_767_225_600)
        let window = DateInterval(start: start, duration: 86_400)
        var row = AggregateSampleRow(type: config.typeIdentifier, function: .average, intervalValue: 1,
            intervalUnit: .day, deviceFilter: .all, bucketStart: start, bucketEnd: window.end, value: nil, unit: "count/min")
        let successful = Destination()
        do {
            try await engine.uploadAggregateRows([row], config: config, configID: config.id, reason: .manual,
                transport: successful, chunk: window, pass: .scheduled, sourceObservations: [successful.observation])
            Issue.record("Unknown aggregate advanced progress")
        } catch SyncError.readableHistoryUnknown {}
        #expect(await successful.batches.isEmpty)
        row.value = 61
        let failing = Destination(fails: true)
        do {
            try await engine.uploadAggregateRows([row], config: config, configID: config.id, reason: .manual,
                transport: failing, chunk: window, pass: .scheduled, sourceObservations: [failing.observation])
            Issue.record("Failed acknowledgement advanced aggregate progress")
        } catch TransportError.invalidAcknowledgement {}
        #expect(await engine.store.aggregateState(for: config.id).computedThrough == nil)
        try await engine.uploadAggregateRows([row], config: config, configID: config.id, reason: .manual,
            transport: successful, chunk: window, pass: .scheduled, sourceObservations: [successful.observation])
        #expect(await engine.store.aggregateState(for: config.id).computedThrough == window.end)
        #expect(await successful.batches.first?.sourceObservations == [successful.observation])
    }

    @Test func unknownActivityAndFailedAckKeepTheWatermark() async throws {
        let engine = ConcurrentUploadTests().makeEngine()
        let day = Date(timeIntervalSince1970: 1_767_225_600)
        let destination = Destination()
        for rows in [[], [ActivitySummaryRow(date: day)]] {
            do {
                try await engine.uploadActivitySummaryRows(rows, through: day, reason: .manual,
                    transport: destination, sourceObservations: [destination.observation])
                Issue.record("Unknown ring read advanced progress")
            } catch SyncError.readableHistoryUnknown {}
        }
        #expect(await destination.batches.isEmpty)
        let row = ActivitySummaryRow(date: day, moveKcal: 61)
        let failing = Destination(fails: true)
        do {
            try await engine.uploadActivitySummaryRows([row], through: day, reason: .manual,
                transport: failing, sourceObservations: [failing.observation])
            Issue.record("Failed acknowledgement advanced activity progress")
        } catch TransportError.invalidAcknowledgement {}
        #expect(await engine.store.activitySummaryState.computedThrough == nil)
        try await engine.uploadActivitySummaryRows([row], through: day, reason: .manual,
            transport: destination, sourceObservations: [destination.observation])
        #expect(await engine.store.activitySummaryState.computedThrough == day)
        #expect(await destination.batches.first?.sourceObservations == [destination.observation])
    }
}
