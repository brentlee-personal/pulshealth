import CryptoKit
import Foundation

/// The committed outcome of an authenticated family upload.
public struct FamilyBatchReceipt: Sendable, Equatable, Decodable {
    public let memberID: UUID
    public let deviceGeneration: Int64
    public let batchID: UUID
    public let bodyDigest: String
    public let state: String
    public let recordCount: Int
    public let accepted: Int
    public let duplicate: Int
    public let deleted: Int
    public let suppressed: Int
    public let transactionID: UUID
    public let committedAt: String

    private enum CodingKeys: String, CodingKey, CaseIterable {
        case memberID = "member_id", deviceGeneration = "device_generation"
        case batchID = "batch_id", bodyDigest = "body_digest", state
        case recordCount = "record_count", accepted, duplicate, deleted, suppressed
        case transactionID = "transaction_id", committedAt = "committed_at"
    }

    static let version = "family-health-batch-receipt-v1"

    static func validateResponse(
        _ body: Data, version: String?, memberID: String, batchID: UUID, ndjson: Data
    ) throws -> FamilyBatchReceipt? {
        let object = (try? JSONSerialization.jsonObject(with: body)) as? [String: Any]
        guard version != nil || object?.keys.contains("receipt") == true else { return nil }
        struct Envelope: Decodable { let receipt: FamilyBatchReceipt }
        guard version == nil || version == Self.version,
              let fields = object?["receipt"] as? [String: Any],
              Set(fields.keys) == Set(CodingKeys.allCases.map(\.rawValue)),
              let receipt = try? JSONDecoder().decode(Envelope.self, from: body).receipt,
              let expectedMember = UUID(uuidString: memberID), receipt.memberID == expectedMember,
              receipt.batchID == batchID, receipt.deviceGeneration > 0,
              receipt.state == "committed", (0...5000).contains(receipt.recordCount),
              receipt.recordCount == ndjson.reduce(0, { $0 + ($1 == 10 ? 1 : 0) }) - 1,
              receipt.bodyDigest == SHA256.hash(data: ndjson).map({ String(format: "%02x", $0) }).joined(),
              receipt.committedAt.hasSuffix("Z"),
              receipt.committedAt.range(of: #"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$"#, options: .regularExpression) != nil
        else { throw TransportError.invalidAcknowledgement }
        let counts = [receipt.accepted, receipt.duplicate, receipt.deleted, receipt.suppressed]
        guard counts.allSatisfy({ (0...receipt.recordCount).contains($0) }),
              counts.reduce(0, +) == receipt.recordCount
        else { throw TransportError.invalidAcknowledgement }
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = receipt.committedAt.contains(".")
            ? [.withInternetDateTime, .withFractionalSeconds] : [.withInternetDateTime]
        guard formatter.date(from: receipt.committedAt) != nil else {
            throw TransportError.invalidAcknowledgement
        }
        return receipt
    }
}
