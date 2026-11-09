import Foundation
import Testing
@testable import PulsHealthSync

@Suite struct ConfigurationIsolationTests {
    actor PausedTransport: SyncTransport {
        private var started: CheckedContinuation<Void, Never>?
        private var finish: CheckedContinuation<Void, Never>?
        private var uploading = false

        func waitUntilUploading() async {
            if uploading { return }
            await withCheckedContinuation { started = $0 }
        }

        func upload(_ batch: SyncBatch) async throws -> UploadResult {
            uploading = true
            started?.resume()
            started = nil
            await withCheckedContinuation { finish = $0 }
            return UploadResult(bytesSent: 1, duration: 0)
        }

        func complete() { finish?.resume(); finish = nil }
    }

    func makeEngine() -> HealthSyncEngine {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("puls-tests-\(UUID())")
        return HealthSyncEngine(
            store: SyncStateStore(directory: dir, tokenStore: InMemoryTokenStore()),
            eventLog: SyncEventLog(directory: dir), wakeLog: WakeLog(directory: dir))
    }

    @Test func destinationAndDisconnectWaitForCapturedTransportToFinish() async throws {
        let engine = makeEngine()
        let original = SyncConfiguration(serverURL: URL(string: "https://original.test"), authToken: "old-token")
        #expect(await engine.configure(original))
        let paused = PausedTransport()
        await engine.setTransport(paused)
        let upload = Task { try await engine.syncProfile() }
        await paused.waitUntilUploading()

        var replacement = original
        replacement.serverURL = URL(string: "https://replacement.test")
        replacement.authToken = "new-token"
        #expect(!(await engine.configure(replacement, confirmServerIdentity: true)))
        #expect(!(await engine.clearAuthToken()))
        #expect(!(await engine.setAuthToken("rotated-token")))
        #expect(!(await engine.resetAll()))
        #expect(await engine.store.configuration.serverURL == original.serverURL)
        #expect(await engine.store.configuration.authToken == "old-token")

        await paused.complete()
        try await upload.value
        #expect(await engine.configure(replacement, confirmServerIdentity: true))
        #expect(await engine.store.configuration.serverURL == replacement.serverURL)
        #expect(await engine.clearAuthToken())
        #expect(await engine.transport == nil)
    }

    @Test func clearingAnEmptyDraftTokenIsPartOfTheConfigurationCommit() async {
        let engine = makeEngine()
        var config = SyncConfiguration(serverURL: URL(string: "https://original.test"), authToken: "old-token")
        #expect(await engine.configure(config))
        config.authToken = nil
        #expect(await engine.configure(config, clearAuthToken: true))
        #expect(await engine.store.configuration.authToken == nil)
        #expect(await engine.transport == nil)
        // A cold rebuild must not resurrect the old token either.
        await engine.ensureTransport()
        #expect(await engine.transport == nil)
    }

    @Test func aClaimedRawSweepPreventsChangingAggregateStateOrDestination() async {
        let engine = makeEngine()
        let aggregate = AggregateConfig(typeIdentifier: "HKQuantityTypeIdentifierStepCount", function: .sum)
        var original = SyncConfiguration(serverURL: URL(string: "https://original.test"), authToken: "token")
        original.aggregates = [aggregate]
        #expect(await engine.configure(original))
        let watermark = Date(timeIntervalSince1970: 1_000)
        await engine.store.recordAggregateUpload(configID: aggregate.id, newComputedThrough: watermark, buckets: 1, bytes: 1)
        #expect(await engine.claimTypes(["test-type"]) == ["test-type"])

        var replacement = original
        replacement.aggregates[0].intervalUnit = .week
        #expect(!(await engine.configure(replacement)))
        #expect(await engine.store.aggregateState(for: aggregate.id).computedThrough == watermark)

        // The unknown test type exits without querying HealthKit, releasing
        // exactly the claim the real backfill path holds across its uploads.
        await engine.sweep(["test-type"], reason: .backfill)
        #expect(await engine.configure(replacement))
        #expect(await engine.store.aggregateState(for: aggregate.id).computedThrough == nil)
    }
}

extension ConfigurationIsolationTests {
    @Test func savedDestinationSwitchWaitsForCapturedUpload() async throws {
        let engine = makeEngine()
        let a = SyncConfiguration(serverURL: URL(string: "https://alpha.test"), authToken: "alpha-token")
        let b = SyncConfiguration(serverURL: URL(string: "https://beta.test"), authToken: "beta-token")
        #expect(await engine.configure(b))
        let bID = try #require(await engine.store.activeDestinationID)
        #expect(await engine.configure(a))
        let aID = try #require(await engine.store.activeDestinationID)
        let paused = PausedTransport()
        await engine.setTransport(paused)
        let upload = Task { try await engine.syncProfile() }
        await paused.waitUntilUploading()
        do { try await engine.switchDestination(id: bID); Issue.record("Expected busy") }
        catch SavedDestinationError.busy { }
        do { try await engine.unpairDestination(id: aID); Issue.record("Expected busy") }
        catch SavedDestinationError.busy { }
        #expect(await engine.store.activeDestinationID == aID)
        await paused.complete()
        try await upload.value
        try await engine.switchDestination(id: bID)
        let transport = try #require(await engine.transport as? HTTPSyncTransport)
        #expect(transport.baseURL == b.serverURL)
        #expect(transport.authToken == "beta-token")
        #expect(transport.userID == b.userID)
    }

    @Test func outerConfigurationScopeBlocksSwitchEvenBetweenUploadPhases() async throws {
        let engine = makeEngine()
        #expect(await engine.configure(SyncConfiguration(serverURL: URL(string: "https://alpha.test"), authToken: "alpha")))
        let id = try #require(await engine.store.activeDestinationID)
        // Foreground, observer and scheduled paths hold this scope across
        // their complete run, including gaps where no individual type is busy.
        let scope = try #require(await engine.beginConfigurationUse())
        do { try await engine.switchDestination(id: id); Issue.record("Expected busy") }
        catch SavedDestinationError.busy { }
        do { try await engine.unpairDestination(id: id); Issue.record("Expected busy") }
        catch SavedDestinationError.busy { }
        await engine.releaseTestConfigurationUse(scope)
        try await engine.unpairDestination(id: id)
        #expect(await engine.transport == nil)
    }
}

private extension HealthSyncEngine {
    func releaseTestConfigurationUse(_ key: String) { activeSyncs.remove(key) }
}

extension ConfigurationIsolationTests {
    @Test func retryBackoffRetainsOldDestinationUntilWholeUploadFinishes() async throws {
        let engine = makeEngine()
        let aHost = "\(UUID().uuidString.lowercased()).test"
        let bHost = "\(UUID().uuidString.lowercased()).test"
        let a = SyncConfiguration(serverURL: URL(string: "https://\(aHost)"), authToken: "alpha")
        let b = SyncConfiguration(serverURL: URL(string: "https://\(bHost)"), authToken: "beta")
        #expect(await engine.configure(b))
        let bID = try #require(await engine.store.activeDestinationID)
        #expect(await engine.configure(a))
        let aRequests = RequestRecorder()
        let bRequests = RequestRecorder()
        let observations = RequestRecorder()
        MockURLProtocol.register(host: aHost) { request in
            if request.request.url?.path == "/v1/source-observations" {
                observations.append(request)
                return .http(404, Data())
            }
            aRequests.append(request)
            return .http(aRequests.all.count == 1 ? 503 : 204, Data())
        }
        MockURLProtocol.register(host: bHost) { request in
            if request.request.url?.path == "/v1/source-observations" {
                observations.append(request)
                return .http(404, Data())
            }
            bRequests.append(request)
            return .http(204, Data())
        }
        let pause = PausedTransport()
        var aTransport = HTTPSyncTransport(baseURL: a.serverURL!, authToken: "alpha",
                                          maxRetries: 1, session: MockURLProtocol.session())
        aTransport.sleeper = { _ in
            _ = try await pause.upload(SyncBatch(deviceID: "test", type: "test", reason: .manual,
                                                samples: [], deletions: []))
        }
        await engine.setTransport(aTransport)
        let upload = Task { try await engine.syncProfile() }
        await pause.waitUntilUploading()
        do { try await engine.switchDestination(id: bID); Issue.record("Expected busy during retry backoff") }
        catch SavedDestinationError.busy { }
        #expect(aRequests.all.count == 1)
        #expect(bRequests.all.isEmpty)
        await pause.complete()
        try await upload.value
        #expect(aRequests.all.count == 2)
        try await engine.switchDestination(id: bID)
        let selected = try #require(await engine.transport as? HTTPSyncTransport)
        #expect(selected.baseURL == b.serverURL)
        #expect(selected.authToken == "beta")
        // Preserve the engine-selected destination and credentials, replacing
        // only URLSession with the in-process receiver used by protocol tests.
        await engine.setTransport(HTTPSyncTransport(baseURL: selected.baseURL, authToken: selected.authToken,
                                                   userID: selected.userID, session: MockURLProtocol.session()))
        try await engine.syncProfile()
        #expect(aRequests.all.count == 2)
        #expect(bRequests.all.count == 1)
        #expect(bRequests.all.first?.request.value(forHTTPHeaderField: "Authorization") == "Bearer beta")
        #expect(observations.all.count == 2)
        #expect(observations.all.first?.request.url?.host == aHost)
        #expect(observations.all.last?.request.url?.host == bHost)
        #expect(observations.all.first?.request.value(forHTTPHeaderField: "Authorization") == "Bearer alpha")
        #expect(observations.all.last?.request.value(forHTTPHeaderField: "Authorization") == "Bearer beta")
    }
}

/// Holds the response without blocking URLProtocol's shared loading thread.
private actor DelayedReadGate {
    private var requested = false
    private var waiter: CheckedContinuation<Void, Never>?
    private var response: (@Sendable () -> Void)?

    func receive(_ response: @escaping @Sendable () -> Void) {
        self.response = response
        requested = true
        waiter?.resume()
        waiter = nil
    }
    func waitUntilRequested() async {
        if requested { return }
        await withCheckedContinuation { waiter = $0 }
    }
    func release() { response?(); response = nil }
}

private final class DelayedReadURLProtocol: URLProtocol, @unchecked Sendable {
    private struct Entry: Sendable {
        let gate: DelayedReadGate
        let requests: RequestRecorder
        let data: Data
    }
    private static let lock = NSLock()
    nonisolated(unsafe) private static var entries: [String: Entry] = [:]

    static func session(host: String, gate: DelayedReadGate,
                        requests: RequestRecorder, data: Data) -> URLSession {
        lock.withLock { entries[host] = Entry(gate: gate, requests: requests, data: data) }
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [DelayedReadURLProtocol.self]
        return URLSession(configuration: config)
    }
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        guard let entry = Self.lock.withLock({ Self.entries[request.url?.host ?? ""] }) else {
            client?.urlProtocol(self, didFailWithError: URLError(.cannotFindHost))
            return
        }
        entry.requests.append(.init(request: request, body: Data()))
        Task { @Sendable [self] in
            await entry.gate.receive { [self] in
                let response = HTTPURLResponse(url: request.url!, statusCode: 200,
                    httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"])!
                client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
                client?.urlProtocol(self, didLoad: entry.data)
                client?.urlProtocolDidFinishLoading(self)
            }
        }
    }
    override func stopLoading() {}
}

extension ConfigurationIsolationTests {
    @Test(arguments: [false, true])
    func pendingServerGETBlocksSwitchUntilResponseFinishes(capabilities: Bool) async throws {
        let engine = makeEngine()
        let aHost = "\(UUID().uuidString.lowercased()).test"
        let b = SyncConfiguration(serverURL: URL(string: "https://beta.test"), authToken: "beta")
        #expect(await engine.configure(b))
        let bID = try #require(await engine.store.activeDestinationID)
        let a = SyncConfiguration(serverURL: URL(string: "https://\(aHost)"), authToken: "alpha")
        #expect(await engine.configure(a))
        let aID = try #require(await engine.store.activeDestinationID)
        let requests = RequestRecorder()
        let gate = DelayedReadGate()
        defer { Task { await gate.release() } }
        let session = DelayedReadURLProtocol.session(host: aHost, gate: gate, requests: requests,
            data: Data((capabilities ? #"{"protocolVersions":[1]}"# : "[]").utf8))
        await engine.setTestReadClient(ServerAPIClient(baseURL: a.serverURL!, authToken: "alpha",
                                                       session: session))
        let read = Task {
            if capabilities { _ = try await engine.serverCapabilities() }
            else { _ = try await engine.serverStats() }
        }
        await gate.waitUntilRequested()
        #expect(!requests.all.isEmpty, "The mock receiver must observe the GET")
        do { try await engine.switchDestination(id: bID); Issue.record("Expected busy while GET is pending") }
        catch SavedDestinationError.busy { }
        do { try await engine.unpairDestination(id: aID); Issue.record("Expected busy while GET is pending") }
        catch SavedDestinationError.busy { }
        #expect(await engine.store.activeDestinationID == aID)
        #expect(requests.all.first?.request.httpMethod == "GET")
        #expect(requests.all.first?.request.url?.path == (capabilities ? "/v1/capabilities" : "/v1/stats"))
        #expect(requests.all.first?.request.value(forHTTPHeaderField: "Authorization") == "Bearer alpha")
        await gate.release()
        try await read.value
        try await engine.switchDestination(id: bID)
        #expect(await engine.store.activeDestinationID == bID)
        let transport = try #require(await engine.transport as? HTTPSyncTransport)
        #expect(transport.baseURL == b.serverURL)
        #expect(transport.authToken == "beta")
    }
}

private extension HealthSyncEngine {
    func setTestReadClient(_ client: ServerAPIClient) { apiClient = client }
}
