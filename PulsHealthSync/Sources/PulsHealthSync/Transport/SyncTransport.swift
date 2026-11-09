import Foundation
import os

/// Result of uploading one batch — fed into per-type stats.
public struct UploadResult: Sendable {
    /// Compressed bytes actually sent on the wire.
    public var bytesSent: Int
    public var duration: TimeInterval
    /// What the server said it did with the batch, when it said anything
    /// (`docs/protocol/README.md` § 7.1). Nil for a receiver that answers
    /// with an empty or unrecognised body. Family uploads additionally require
    /// the structured acknowledgement below.
    public var receipt: IngestReceipt?
    public var familyReceipt: FamilyBatchReceipt?

    public init(bytesSent: Int, duration: TimeInterval, receipt: IngestReceipt? = nil, familyReceipt: FamilyBatchReceipt? = nil) {
        self.bytesSent = bytesSent
        self.duration = duration
        self.receipt = receipt
        self.familyReceipt = familyReceipt
    }
}

/// The optional JSON body of a successful `POST /v1/batches`. Every field is
/// optional and unknown fields are ignored, so a receiver that reports only
/// some counts — or none — still decodes; the reference server returns all of
/// them. Informational only: nothing here decides whether an anchor advances.
/// An identical reference-server batch retry repeats its original counts.
public struct IngestReceipt: Sendable, Equatable, Decodable {
    /// Sample lines that created a row.
    public var accepted: Int?
    /// Sample lines whose UUID was already stored.
    public var duplicates: Int?
    /// Sample rows actually removed by deletion lines.
    public var deleted: Int?
    public var routePoints: Int?
    public var seriesPoints: Int?
    public var aggregateSamples: Int?
    public var activitySummaries: Int?

    public init(
        accepted: Int? = nil, duplicates: Int? = nil, deleted: Int? = nil,
        routePoints: Int? = nil, seriesPoints: Int? = nil,
        aggregateSamples: Int? = nil, activitySummaries: Int? = nil
    ) {
        self.accepted = accepted
        self.duplicates = duplicates
        self.deleted = deleted
        self.routePoints = routePoints
        self.seriesPoints = seriesPoints
        self.aggregateSamples = aggregateSamples
        self.activitySummaries = activitySummaries
    }

    /// Tolerant decode of a response body: nil for an empty body, a body that
    /// is not a JSON object, or one that carries none of the known counts.
    /// Never throws — a receipt the client cannot read is the same as none.
    public static func decode(_ body: Data) -> IngestReceipt? {
        guard !body.isEmpty,
              let receipt = try? JSONDecoder().decode(IngestReceipt.self, from: body),
              receipt != IngestReceipt()
        else { return nil }
        return receipt
    }

    /// `"812 new, 188 duplicates"` — the sample-level outcome for a log line,
    /// or nil when the server reported neither count. Either count alone is
    /// still reported (`"812 new"`).
    public var sampleOutcome: String? {
        var parts: [String] = []
        if let accepted { parts.append("\(accepted.formatted()) new") }
        if let duplicates { parts.append("\(duplicates.formatted()) duplicates") }
        return parts.isEmpty ? nil : parts.joined(separator: ", ")
    }
}

public enum TransportError: Error, LocalizedError, CustomStringConvertible {
    case notConfigured
    case serverError(status: Int, body: String)
    case network(Error)
    case invalidAcknowledgement
    /// HTTP 400 `{"error":"unsupported protocol version","supportedVersions":[…]}`:
    /// the server does not speak `PulsProtocol.version`. Distinct from
    /// `serverError` so the UI can name the cause instead of showing a raw 400.
    case unsupportedProtocol(supportedVersions: [Int])

    /// The response body is kept only in scrubbed, capped form: it is shown
    /// on screen, but it is also what ends up in `lastError` and the event log,
    /// and a server (or a proxy in front of it) may echo headers or return a
    /// whole HTML page.
    public var errorDescription: String? {
        switch self {
        case .notConfigured:
            return "Server URL or auth token not configured"
        case .serverError(let status, let body):
            let summary = ServerErrorSummary.summarize(body)
            return "Server returned \(status): \(ErrorScrubber.scrub(summary, limit: ErrorScrubber.displayLimit))"
        case .network(let error):
            return "Network error: \(ErrorScrubber.scrub(error.localizedDescription, limit: ErrorScrubber.displayLimit))"
        case .invalidAcknowledgement:
            return "Server acknowledgement did not match the uploaded batch"
        case .unsupportedProtocol(let versions):
            // Built from integers only — nothing here needs scrubbing.
            let list = versions.isEmpty ? "none" : versions.map(String.init).joined(separator: ", ")
            return "This server does not support this app version (server protocol \(list), app protocol \(PulsProtocol.version))"
        }
    }

    /// `"\(error)"` on an enum would otherwise dump the associated values —
    /// the entire raw response body — into whichever log interpolated it.
    public var description: String { errorDescription ?? "Transport error" }

    /// Server 4xx errors won't succeed on retry, nor will a network failure
    /// that is about the request itself rather than the path to the server
    /// (`isTerminal(_:)`); everything else might.
    var isRetryable: Bool {
        switch self {
        case .notConfigured: return false
        case .serverError(let status, _): return status >= 500 || status == 429
        case .network(let error): return !Self.isTerminal(error)
        case .invalidAcknowledgement: return false
        case .unsupportedProtocol: return false
        }
    }

    /// Whether this failure will repeat on every later attempt until
    /// something changes — the configuration, the server, its certificate —
    /// so the type that hit it should sit out a cooldown
    /// (`TypeSyncState.cooldownUntil`) rather than re-read, re-encode and
    /// re-send the same page on every wake. A 4xx other than 408 and 429
    /// (401 included: the token is wrong until someone changes it), an
    /// unsupported protocol, and a terminal network failure. Never a 5xx, a
    /// timeout or an outage: those clear up on their own.
    public var startsCooldown: Bool {
        switch self {
        case .notConfigured: return false
        case .serverError(let status, _):
            return (400..<500).contains(status) && status != 408 && status != 429
        case .network(let error): return Self.isTerminal(error)
        case .invalidAcknowledgement: return true
        case .unsupportedProtocol: return true
        }
    }

    /// Network failures no retry can fix: the URL itself is unusable, App
    /// Transport Security refuses it, or the TLS handshake failed on a
    /// certificate. Each needs a person to change something (the URL, the
    /// server's certificate, the device clock). Deliberately not here: DNS,
    /// connection, timeout and offline errors, and `secureConnectionFailed`,
    /// which a middlebox or a flaky network can cause once.
    static func isTerminal(_ error: any Error) -> Bool {
        guard let urlError = error as? URLError else { return false }
        switch urlError.code {
        case .badURL,
             .unsupportedURL,
             .appTransportSecurityRequiresSecureConnection,
             .serverCertificateUntrusted,
             .serverCertificateHasBadDate,
             .serverCertificateNotYetValid,
             .serverCertificateHasUnknownRoot,
             .clientCertificateRejected,
             .clientCertificateRequired:
            return true
        default:
            return false
        }
    }

    /// Classifies a non-2xx response. A 400 whose body is the protocol
    /// rejection becomes `unsupportedProtocol`; everything else is `serverError`.
    static func fromResponse(status: Int, body: Data) -> TransportError {
        if status == 400, let versions = Self.unsupportedProtocolVersions(in: body) {
            return .unsupportedProtocol(supportedVersions: versions)
        }
        return .serverError(status: status, body: ServerErrorSummary.responseExcerpt(body))
    }

    private static func unsupportedProtocolVersions(in body: Data) -> [Int]? {
        struct Rejection: Decodable {
            var error: String
            var supportedVersions: [Int]?
        }
        guard let rejection = try? JSONDecoder().decode(Rejection.self, from: body),
              rejection.error.lowercased() == "unsupported protocol version"
        else { return nil }
        return rejection.supportedVersions ?? []
    }
}

/// Pluggable upload destination. The engine only ever talks to this protocol,
/// so tests and benchmarks can swap in mocks, and other backends (file export,
/// S3, etc.) can be added without touching sync logic.
public protocol SyncTransport: Sendable {
    func upload(_ batch: SyncBatch) async throws -> UploadResult
    func beginSourceObservation(_ query: SourceSnapshotQuery) async throws -> UUID?
}

public extension SyncTransport {
    func beginSourceObservation(_ query: SourceSnapshotQuery) async throws -> UUID? { nil }
}

public enum SourceSnapshotQuery: Encodable, Sendable {
    case profile
    case activitySummary
    case aggregate(AggregateConfig)

    private enum CodingKeys: String, CodingKey {
        case kind, type, `func`, intervalValue, intervalUnit, deviceFilter
    }

    public func encode(to encoder: Encoder) throws {
        var fields = encoder.container(keyedBy: CodingKeys.self)
        switch self {
        case .profile:
            try fields.encode("profile", forKey: .kind)
        case .activitySummary:
            try fields.encode("activity_summary", forKey: .kind)
        case .aggregate(let config):
            try fields.encode("aggregate", forKey: .kind)
            try fields.encode(config.typeIdentifier, forKey: .type)
            try fields.encode(config.function.rawValue, forKey: .func)
            try fields.encode(config.intervalValue, forKey: .intervalValue)
            try fields.encode(config.intervalUnit.rawValue, forKey: .intervalUnit)
            try fields.encode(config.deviceFilter.rawValue, forKey: .deviceFilter)
        }
    }
}

/// POSTs gzip NDJSON batches to the Puls ingest server with exponential-backoff retry.
public struct HTTPSyncTransport: SyncTransport {
    public var baseURL: URL
    public var authToken: String
    /// The user every batch is attributed to (sent as the `X-User-ID` header).
    public var userID: String
    public var maxRetries: Int

    private let session: URLSession
    private let logger = Logger(subsystem: PulsLog.subsystem, category: "transport")
    /// How the retry loop waits. `Task.sleep` in production; tests replace it
    /// to record the schedule instead of sleeping through it.
    var sleeper: @Sendable (TimeInterval) async throws -> Void = { seconds in
        try await Task.sleep(for: .seconds(seconds))
    }
    /// The jitter factor applied to each backoff step; injectable for the
    /// same reason.
    var jitter: @Sendable () -> Double = { .random(in: 0.7...1.3) }

    /// The longest `Retry-After` a retry will wait out within one run. Longer
    /// than this the run gives up at once — anchors stay put and a later wake
    /// sends the page — rather than sleep through its background time.
    static let maxRetryAfter: TimeInterval = 60
    /// The same limit for an observer wake, whose HealthKit completion
    /// handlers are held until the wake ends (see `upload`).
    static let maxObserverRetryAfter: TimeInterval = 10

    public init(baseURL: URL, authToken: String, userID: String = PulsDefaultUser.id, maxRetries: Int = 4, session: URLSession? = nil) {
        self.baseURL = baseURL
        self.authToken = authToken
        self.userID = userID
        self.maxRetries = maxRetries
        if let session {
            self.session = session
        } else {
            let config = URLSessionConfiguration.default
            config.timeoutIntervalForRequest = 60
            config.httpMaximumConnectionsPerHost = 8
            // Uploads are useless stale; don't let the OS queue them for hours.
            config.waitsForConnectivity = false
            self.session = URLSession(configuration: config)
        }
    }

    /// Uploads a header-only batch (every count zero, `reason` manual, `type`
    /// `PulsProtocol.probeBatchType`). Any 2xx proves the server accepts
    /// batches from this client; used by the connection test when the server
    /// has no `/v1/capabilities`.
    public func probe(deviceID: String) async throws -> UploadResult {
        try await upload(SyncBatch(
            deviceID: deviceID, type: PulsProtocol.probeBatchType, reason: .manual,
            samples: [], deletions: []))
    }

    public func beginSourceObservation(_ query: SourceSnapshotQuery) async throws -> UUID? {
        var request = URLRequest(url: baseURL.appendingPathComponent("v1/source-observations"))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.setValue("Bearer \(authToken)", forHTTPHeaderField: "Authorization")
        request.setValue(userID, forHTTPHeaderField: "X-User-ID")
        request.setValue(String(PulsProtocol.version), forHTTPHeaderField: PulsProtocol.headerField)
        request.httpBody = try JSONEncoder().encode(query)
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw TransportError.network(URLError(.badServerResponse))
        }
        if http.statusCode == 404 || http.statusCode == 405 { return nil }
        guard (200..<300).contains(http.statusCode) else {
            throw TransportError.fromResponse(status: http.statusCode, body: data)
        }
        guard let fields = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            throw TransportError.invalidAcknowledgement
        }
        if fields.isEmpty { return nil }
        guard fields.count == 1, let value = fields["observation_id"] as? String,
              let id = UUID(uuidString: value) else { throw TransportError.invalidAcknowledgement }
        return id
    }

    public func upload(_ batch: SyncBatch) async throws -> UploadResult {
        let ndjson = try BatchSerializer.ndjson(for: batch)
        let body = BatchSerializer.gzip(ndjson)
        let request = makeRequest(for: batch, body: body)

        // Observer wakes hold HealthKit's completion handlers until the whole
        // wake finishes. Offline, the full backoff ladder (2+4+8+16 s, per pack)
        // can outlast the app's background allowance, and HealthKit stops
        // waking the app after three unacknowledged deliveries. Spend one retry
        // there and give up: anchors stay put, so the next wake resends.
        let isObserverWake = WakeScope.current?.trigger == .observer
        let retryBudget = isObserverWake ? min(maxRetries, 1) : maxRetries
        let retryAfterLimit = isObserverWake ? Self.maxObserverRetryAfter : Self.maxRetryAfter

        var attempt = 0
        while true {
            let start = ContinuousClock.now
            // What a 429 or 503 asked for, when it asked.
            var retryAfter: TimeInterval?
            do {
                let (data, response) = try await session.data(for: request)
                let elapsed = (ContinuousClock.now - start).seconds
                guard let http = response as? HTTPURLResponse else {
                    throw TransportError.network(URLError(.badServerResponse))
                }
                guard (200..<300).contains(http.statusCode) else {
                    if http.statusCode == 429 || http.statusCode == 503 {
                        retryAfter = Self.retryAfter(
                            http.value(forHTTPHeaderField: "Retry-After"), now: Date())
                    }
                    throw TransportError.fromResponse(status: http.statusCode, body: data)
                }
                let familyReceipt = try FamilyBatchReceipt.validateResponse(
                    data, version: http.value(forHTTPHeaderField: "X-Family-Receipt"),
                    memberID: userID, batchID: batch.batchID, ndjson: ndjson)
                if batch.sourceObservations?.isEmpty == false, familyReceipt == nil {
                    throw TransportError.invalidAcknowledgement
                }
                logger.debug("Uploaded \(batch.samples.count) samples (\(body.count) bytes gzip, \(ndjson.count) raw) in \(elapsed, format: .fixed(precision: 3))s")
                return UploadResult(bytesSent: body.count, duration: elapsed,
                                    receipt: IngestReceipt.decode(data), familyReceipt: familyReceipt)
            } catch {
                // A cancelled request surfaces as URLError.cancelled, which would
                // otherwise count as a retryable network error and burn a backoff
                // sleep before the CancellationError from Task.sleep escapes.
                if Task.isCancelled { throw CancellationError() }
                let transportError = (error as? TransportError) ?? .network(error)
                attempt += 1
                guard transportError.isRetryable, attempt <= retryBudget,
                      let delay = Self.retryDelay(
                        attempt: attempt, retryAfter: retryAfter, limit: retryAfterLimit,
                        jitter: self.jitter())
                else {
                    throw transportError
                }
                logger.warning("Upload attempt \(attempt) failed (\(String(describing: transportError))); retrying in \(delay, format: .fixed(precision: 1))s")
                try await self.sleeper(delay)
            }
        }
    }

    /// How long to wait before retry number `attempt` (1-based): the server's
    /// `Retry-After` when it sent one, else the exponential ladder (2, 4, 8,
    /// 16 s, at most 30) times `jitter`. Nil when the server asked for longer
    /// than `limit` — the run then stops instead of sleeping through its
    /// background time, and a later wake sends the page. A `Retry-After` is
    /// never shortened: retrying early would only be refused again.
    static func retryDelay(
        attempt: Int, retryAfter: TimeInterval?, limit: TimeInterval, jitter: Double
    ) -> TimeInterval? {
        if let retryAfter {
            return retryAfter <= limit ? retryAfter : nil
        }
        return min(30, pow(2.0, Double(attempt))) * jitter
    }

    /// A `Retry-After` value in seconds: the delta-seconds form (`"5"`), or
    /// an HTTP date, measured from `now`. Nil when absent or unparseable,
    /// in which case the ordinary ladder applies. A date in the past is 0.
    static func retryAfter(_ value: String?, now: Date) -> TimeInterval? {
        guard let value = value?.trimmingCharacters(in: .whitespaces), !value.isEmpty else {
            return nil
        }
        if let seconds = Int(value) {
            return seconds >= 0 ? TimeInterval(seconds) : nil
        }
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.timeZone = TimeZone(identifier: "GMT")
        formatter.dateFormat = "EEE, dd MMM yyyy HH:mm:ss zzz"
        guard let date = formatter.date(from: value) else { return nil }
        return max(0, date.timeIntervalSince(now))
    }

    /// The upload request for one batch. Internal so tests can verify the
    /// headers every upload must carry (protocol version, user, wake).
    func makeRequest(for batch: SyncBatch, body: Data) -> URLRequest {
        var request = URLRequest(url: baseURL.appendingPathComponent("v1/batches"))
        request.httpMethod = "POST"
        request.setValue("application/x-ndjson", forHTTPHeaderField: "Content-Type")
        request.setValue("gzip", forHTTPHeaderField: "Content-Encoding")
        request.setValue("Bearer \(authToken)", forHTTPHeaderField: "Authorization")
        request.setValue(String(PulsProtocol.version), forHTTPHeaderField: PulsProtocol.headerField)
        request.setValue(batch.batchID.uuidString, forHTTPHeaderField: "X-Batch-ID")
        request.setValue(userID, forHTTPHeaderField: "X-User-ID")
        // Stamp the originating wake (task-local, inherited from the sync that
        // built this batch) so the server's `batches` rows join back to the
        // device's wake records. Like X-User-ID, this is transport metadata, not
        // NDJSON body — absent for work outside a wake (reconciliation, tests).
        if let wake = WakeScope.current {
            request.setValue(wake.id.uuidString, forHTTPHeaderField: "X-Wake-ID")
            request.setValue(wake.trigger.rawValue, forHTTPHeaderField: "X-Wake-Trigger")
        }
        request.httpBody = body
        return request
    }
}

extension Duration {
    var seconds: TimeInterval {
        TimeInterval(components.seconds) + TimeInterval(components.attoseconds) / 1e18
    }
}
