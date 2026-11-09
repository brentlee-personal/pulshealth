import Foundation

/// Serializes + compresses batches but discards them. Used by the app's built-in
/// benchmark mode to measure pure HealthKit-read + encode throughput with the
/// network taken out of the equation.
public struct DryRunTransport: SyncTransport {
    public init() {}

    public func upload(_ batch: SyncBatch) async throws -> UploadResult {
        let start = ContinuousClock.now
        let ndjson = try BatchSerializer.ndjson(for: batch)
        let gz = BatchSerializer.gzip(ndjson)
        return UploadResult(bytesSent: gz.count, duration: (ContinuousClock.now - start).seconds)
    }
}

/// Wraps a real transport and records per-batch timings, for A/B-testing batch
/// sizes and concurrency settings from the app's debug screen.
public actor InstrumentedTransport: SyncTransport {
    public struct Sample: Sendable {
        public let type: String
        public let sampleCount: Int
        public let bytes: Int
        public let duration: TimeInterval
        public let at: Date
    }

    private let wrapped: SyncTransport
    public private(set) var samples: [Sample] = []

    public init(wrapping transport: SyncTransport) {
        self.wrapped = transport
    }

    public func beginSourceObservation(_ query: SourceSnapshotQuery) async throws -> UUID? {
        try await wrapped.beginSourceObservation(query)
    }

    public func upload(_ batch: SyncBatch) async throws -> UploadResult {
        let result = try await wrapped.upload(batch)
        samples.append(Sample(
            type: batch.type, sampleCount: batch.samples.count,
            bytes: result.bytesSent, duration: result.duration, at: Date()
        ))
        if samples.count > 5_000 { samples.removeFirst(samples.count - 5_000) }
        return result
    }

    public func recentSamples() -> [Sample] { samples }
}
