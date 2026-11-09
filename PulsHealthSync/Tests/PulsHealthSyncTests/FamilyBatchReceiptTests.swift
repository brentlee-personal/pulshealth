import CryptoKit
import Foundation
import Testing
@testable import PulsHealthSync

@Suite struct FamilyBatchReceiptTests {
    private let member = "00000000-0000-4000-8000-000000000001"
    private var batch: SyncBatch {
        SyncBatch(deviceID: "receipt-test", type: "probe", reason: .manual, samples: [], deletions: [])
    }

    private func fields(_ batch: SyncBatch, ndjson: Data, count: Int = 0) -> [String: Any] {
        ["member_id": member, "device_generation": 7, "batch_id": batch.batchID.uuidString,
         "body_digest": SHA256.hash(data: ndjson).map { String(format: "%02x", $0) }.joined(),
         "state": "committed", "record_count": count, "accepted": count,
         "duplicate": 0, "deleted": 0, "suppressed": 0,
         "transaction_id": "00000000-0000-4000-8000-000000000002",
         "committed_at": "2026-01-01T00:00:00.123456Z"]
    }

    private func transport(body: Data, version: String? = FamilyBatchReceipt.version) -> HTTPSyncTransport {
        let host = "\(UUID().uuidString.lowercased()).test"
        let headers = version.map { ["X-Family-Receipt": $0] } ?? [:]
        MockURLProtocol.register(host: host) { _ in .httpWithHeaders(200, headers, body) }
        return HTTPSyncTransport(baseURL: URL(string: "https://\(host)")!, authToken: "synthetic",
                                 userID: member, maxRetries: 0, session: MockURLProtocol.session())
    }

    @Test func transportRequiresMatchingCommittedReceipt() async throws {
        let batch = batch
        let ndjson = try BatchSerializer.ndjson(for: batch)
        let body = try JSONSerialization.data(withJSONObject: ["receipt": fields(batch, ndjson: ndjson)])
        for version in [FamilyBatchReceipt.version, nil] {
            let result = try await transport(body: body, version: version).upload(batch)
            #expect(result.familyReceipt?.batchID == batch.batchID)
            #expect(result.familyReceipt?.memberID == UUID(uuidString: member))
            #expect(result.familyReceipt?.recordCount == 0)
        }
    }

    @Test func invalidTwoHundredResponseDoesNotAcknowledgeUpload() async throws {
        let batch = batch
        let ndjson = try BatchSerializer.ndjson(for: batch)
        let mutations: [(String, Any)] = [
            ("member_id", UUID().uuidString), ("batch_id", UUID().uuidString),
            ("body_digest", String(repeating: "0", count: 64)), ("state", "pending"),
            ("device_generation", 0), ("record_count", 1), ("accepted", -1),
            ("accepted", Int.max), ("suppressed", 1), ("transaction_id", "invalid"),
            ("committed_at", "2026-01-01T08:00:00+08:00"), ("extra", true),
        ]
        for (key, value) in mutations {
            var receipt = fields(batch, ndjson: ndjson)
            receipt[key] = value
            let body = try JSONSerialization.data(withJSONObject: ["receipt": receipt])
            await #expect(throws: TransportError.self) {
                try await transport(body: body).upload(batch)
            }
        }
        var missing = fields(batch, ndjson: ndjson)
        missing.removeValue(forKey: "transaction_id")
        let missingField = try JSONSerialization.data(withJSONObject: ["receipt": missing])
        let valid = try JSONSerialization.data(withJSONObject: ["receipt": fields(batch, ndjson: ndjson)])
        for (body, version) in [(Data(), FamilyBatchReceipt.version),
                                (Data("<html>ok</html>".utf8), FamilyBatchReceipt.version),
                                (missingField, FamilyBatchReceipt.version), (valid, "unknown-v2")] {
            do {
                _ = try await transport(body: body, version: version).upload(batch)
                Issue.record("Invalid family response acknowledged an upload")
            } catch TransportError.invalidAcknowledgement {
                #expect(!TransportError.invalidAcknowledgement.isRetryable)
                #expect(TransportError.invalidAcknowledgement.startsCooldown)
            }
        }
    }

    @Test func countsUseSerializedBusinessLines() throws {
        let batch = batch
        let ndjson = Data("{}\n{\"route\":{\"points\":[1,2,3]}}\n".utf8)
        let body = try JSONSerialization.data(withJSONObject: ["receipt": fields(batch, ndjson: ndjson, count: 1)])
        let receipt = try FamilyBatchReceipt.validateResponse(
            body, version: FamilyBatchReceipt.version, memberID: member, batchID: batch.batchID, ndjson: ndjson)
        #expect(receipt?.recordCount == 1)
        #expect(receipt?.accepted == 1)
    }
}
