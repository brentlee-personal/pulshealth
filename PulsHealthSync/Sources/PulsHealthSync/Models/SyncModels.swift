import Foundation

/// The kind of HealthKit object a `SyncSample` represents.
public enum SampleKind: String, Codable, Sendable {
    case quantity
    case category
    case workout
    case heartbeatSeries
    case ecg
    case stateOfMind
    case medicationDose
    /// Daily activity summary (HKActivitySummary — the activity rings). Never
    /// carried by a `SyncSample` (summaries have no UUID and ride their own
    /// `{"activitySummary":...}` line); this case exists so the catalog can list
    /// the rings as an enable-able type and the server never sees it as a sample
    /// kind.
    case activitySummary
}

/// A JSON-safe metadata value extracted from `HKObject.metadata`.
public enum MetadataValue: Codable, Sendable, Equatable {
    case string(String)
    case number(Double)
    case bool(Bool)
    case date(Date)

    public init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if let b = try? container.decode(Bool.self) {
            self = .bool(b)
        } else if let d = try? container.decode(Double.self) {
            self = .number(d)
        } else {
            self = .string(try container.decode(String.self))
        }
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case .string(let s): try container.encode(s)
        case .number(let n): try container.encode(n)
        case .bool(let b): try container.encode(b)
        case .date(let d): try container.encode(d)
        }
    }
}

/// Temporal context needed to reconstruct a source event's local wall time.
/// The timestamp itself remains the canonical UTC instant; this captures the
/// timezone/offset used when the event was observed or inferred.
public struct TemporalContext: Codable, Sendable, Equatable {
    public var timeZoneID: String
    public var utcOffsetSeconds: Int
    public var source: String
    public var confidence: String
    public var tzdbVersion: String

    public init(
        timeZoneID: String,
        utcOffsetSeconds: Int,
        source: String,
        confidence: String,
        tzdbVersion: String = ""
    ) {
        self.timeZoneID = timeZoneID
        self.utcOffsetSeconds = utcOffsetSeconds
        self.source = source
        self.confidence = confidence
        self.tzdbVersion = tzdbVersion
    }

    public static func deviceCurrent(
        for date: Date,
        timeZone: TimeZone = .current,
        confidence: String = "inferred"
    ) -> TemporalContext {
        TemporalContext(
            timeZoneID: timeZone.identifier,
            utcOffsetSeconds: timeZone.secondsFromGMT(for: date),
            source: "device_current",
            confidence: confidence
        )
    }
}

/// Wire representation of a single HealthKit sample. One NDJSON line on the wire.
public struct SyncSample: Codable, Sendable, Equatable {
    public var uuid: UUID
    public var type: String
    public var kind: SampleKind
    public var start: Date
    public var end: Date
    public var startContext: TemporalContext?
    public var endContext: TemporalContext?
    /// Quantity value converted to the canonical unit for the type (see `HealthTypeCatalog`).
    public var value: Double?
    public var unit: String?
    /// Raw `HKCategorySample.value` for category samples (e.g. sleep stage).
    public var category: Int?
    public var sourceName: String?
    public var sourceBundleID: String?
    public var sourceVersion: String?
    public var device: String?
    public var metadata: [String: MetadataValue]?
    public var workout: WorkoutDetail?
    /// Beat offsets for `kind == .heartbeatSeries`.
    public var heartbeats: [Heartbeat]?
    /// Voltage trace + classification for `kind == .ecg`.
    public var ecg: ECGDetail?
    /// Mood/emotion log for `kind == .stateOfMind`.
    public var stateOfMind: StateOfMindDetail?
    /// Dose log for `kind == .medicationDose`.
    public var medicationDose: MedicationDoseDetail?

    public init(
        uuid: UUID, type: String, kind: SampleKind, start: Date, end: Date,
        startContext: TemporalContext? = nil, endContext: TemporalContext? = nil,
        value: Double? = nil, unit: String? = nil, category: Int? = nil,
        sourceName: String? = nil, sourceBundleID: String? = nil, sourceVersion: String? = nil,
        device: String? = nil, metadata: [String: MetadataValue]? = nil,
        workout: WorkoutDetail? = nil, heartbeats: [Heartbeat]? = nil,
        ecg: ECGDetail? = nil, stateOfMind: StateOfMindDetail? = nil,
        medicationDose: MedicationDoseDetail? = nil
    ) {
        self.uuid = uuid
        self.type = type
        self.kind = kind
        self.start = start
        self.end = end
        self.startContext = startContext
        self.endContext = endContext
        self.value = value
        self.unit = unit
        self.category = category
        self.sourceName = sourceName
        self.sourceBundleID = sourceBundleID
        self.sourceVersion = sourceVersion
        self.device = device
        self.metadata = metadata
        self.workout = workout
        self.heartbeats = heartbeats
        self.ecg = ecg
        self.stateOfMind = stateOfMind
        self.medicationDose = medicationDose
    }
}

/// One beat in a heartbeat series, encoded on the wire as `[secondsSinceSeriesStart, precededByGap]`.
public struct Heartbeat: Codable, Sendable, Equatable {
    public var timeSinceSeriesStart: TimeInterval
    public var precededByGap: Bool

    public init(timeSinceSeriesStart: TimeInterval, precededByGap: Bool) {
        self.timeSinceSeriesStart = timeSinceSeriesStart
        self.precededByGap = precededByGap
    }

    public init(from decoder: Decoder) throws {
        var container = try decoder.unkeyedContainer()
        timeSinceSeriesStart = try container.decode(TimeInterval.self)
        precededByGap = try container.decode(Bool.self)
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.unkeyedContainer()
        try container.encode(timeSinceSeriesStart)
        try container.encode(precededByGap)
    }
}

/// Extra fields carried only by ECG samples.
public struct ECGDetail: Codable, Sendable, Equatable {
    /// Stable wire string, e.g. "sinusRhythm", "atrialFibrillation".
    public var classification: String
    public var averageHeartRateBpm: Double?
    public var samplingFrequencyHz: Double?
    /// "notSet" | "none" | "present"
    public var symptomsStatus: String
    /// Lead I voltage trace in microvolts, one entry per measurement.
    public var voltagesUV: [Double]

    public init(
        classification: String, averageHeartRateBpm: Double? = nil,
        samplingFrequencyHz: Double? = nil, symptomsStatus: String, voltagesUV: [Double]
    ) {
        self.classification = classification
        self.averageHeartRateBpm = averageHeartRateBpm
        self.samplingFrequencyHz = samplingFrequencyHz
        self.symptomsStatus = symptomsStatus
        self.voltagesUV = voltagesUV
    }
}

/// Extra fields carried only by State of Mind samples (iOS 18+).
public struct StateOfMindDetail: Codable, Sendable, Equatable {
    /// "momentaryEmotion" | "dailyMood"
    public var kind: String
    /// -1 (very unpleasant) ... +1 (very pleasant)
    public var valence: Double
    public var valenceClassification: String
    public var labels: [String]
    public var associations: [String]

    public init(
        kind: String, valence: Double, valenceClassification: String,
        labels: [String], associations: [String]
    ) {
        self.kind = kind
        self.valence = valence
        self.valenceClassification = valenceClassification
        self.labels = labels
        self.associations = associations
    }
}

/// Extra fields carried only by medication dose events (iOS 26+).
public struct MedicationDoseDetail: Codable, Sendable, Equatable {
    /// HealthKit display name when resolvable, otherwise nil. Not a stable identifier.
    public var medication: String?
    /// Locally assigned concept UUID; stable only while the device identity map survives.
    public var medicationID: String?
    /// "taken" | "skipped" | "snoozed" | "notLogged" | ...
    public var status: String
    public var scheduledAt: Date?
    public var scheduledAtContext: TemporalContext?
    public var doseQuantity: Double?
    public var doseUnit: String?

    public init(
        medication: String? = nil, medicationID: String? = nil, status: String, scheduledAt: Date? = nil,
        scheduledAtContext: TemporalContext? = nil,
        doseQuantity: Double? = nil, doseUnit: String? = nil
    ) {
        self.medication = medication
        self.medicationID = medicationID
        self.status = status
        self.scheduledAt = scheduledAt
        self.scheduledAtContext = scheduledAtContext
        self.doseQuantity = doseQuantity
        self.doseUnit = doseUnit
    }
}

/// GPS points for one workout's route, sent as separate NDJSON lines after deletions.
/// Long routes are split into multiple payloads of at most `maxPointsPerPayload` points.
public struct RoutePayload: Codable, Sendable, Equatable {
    public static let maxPointsPerPayload = 4000

    public var workoutUUID: UUID
    public var points: [RoutePoint]

    public init(workoutUUID: UUID, points: [RoutePoint]) {
        self.workoutUUID = workoutUUID
        self.points = points
    }
}

/// One GPS fix. Optional fields are omitted when Core Location reports them invalid (< 0).
public struct RoutePoint: Codable, Sendable, Equatable {
    public var t: Date
    public var temporalContext: TemporalContext?
    public var lat: Double
    public var lon: Double
    public var alt: Double?
    public var hAcc: Double?
    public var vAcc: Double?
    public var speed: Double?
    public var course: Double?

    public init(
        t: Date, temporalContext: TemporalContext? = nil,
        lat: Double, lon: Double, alt: Double? = nil, hAcc: Double? = nil,
        vAcc: Double? = nil, speed: Double? = nil, course: Double? = nil
    ) {
        self.t = t
        self.temporalContext = temporalContext
        self.lat = lat
        self.lon = lon
        self.alt = alt
        self.hAcc = hAcc
        self.vAcc = vAcc
        self.speed = speed
        self.course = course
    }
}

/// One quantity type's aggregate statistics over a workout (or one of its
/// activities): min/avg/max for discrete types (e.g. heart rate), sum for
/// cumulative types (e.g. active energy). Values are in the type's canonical unit.
public struct WorkoutStat: Codable, Sendable, Equatable {
    public var min: Double?
    public var avg: Double?
    public var max: Double?
    public var sum: Double?

    public init(min: Double? = nil, avg: Double? = nil, max: Double? = nil, sum: Double? = nil) {
        self.min = min
        self.avg = avg
        self.max = max
        self.sum = sum
    }

    public var isEmpty: Bool { min == nil && avg == nil && max == nil && sum == nil }
}

/// A workout event marker (pause/resume/lap/segment/marker/motionPaused…).
/// `end` is nil for point-in-time events and set for spans (segments, laps).
public struct WorkoutEvent: Codable, Sendable, Equatable {
    public var type: String
    public var start: Date
    public var end: Date?
    public var startContext: TemporalContext?
    public var endContext: TemporalContext?
    public var metadata: [String: MetadataValue]?

    public init(
        type: String, start: Date, end: Date? = nil,
        startContext: TemporalContext? = nil, endContext: TemporalContext? = nil,
        metadata: [String: MetadataValue]? = nil
    ) {
        self.type = type
        self.start = start
        self.end = end
        self.startContext = startContext
        self.endContext = endContext
        self.metadata = metadata
    }
}

/// One sub-activity of a multi-sport / interval workout (iOS 16+ `HKWorkoutActivity`).
public struct WorkoutActivitySegment: Codable, Sendable, Equatable {
    public var activityType: String
    public var start: Date
    public var end: Date?
    public var startContext: TemporalContext?
    public var endContext: TemporalContext?
    public var duration: TimeInterval
    public var statistics: [String: WorkoutStat]?

    public init(
        activityType: String, start: Date, end: Date? = nil,
        startContext: TemporalContext? = nil, endContext: TemporalContext? = nil,
        duration: TimeInterval, statistics: [String: WorkoutStat]? = nil
    ) {
        self.activityType = activityType
        self.start = start
        self.end = end
        self.startContext = startContext
        self.endContext = endContext
        self.duration = duration
        self.statistics = statistics
    }
}

/// Extra fields carried only by workouts.
public struct WorkoutDetail: Codable, Sendable, Equatable {
    public var activityType: String
    public var duration: TimeInterval
    public var totalEnergyKcal: Double?
    public var totalDistanceMeters: Double?
    /// Flat per-type aggregate (representative value), kept for back-compat.
    public var statistics: [String: Double]?
    /// Per-type min/avg/max/sum aggregates (iOS 16+), keyed by type identifier.
    public var statisticsDetail: [String: WorkoutStat]?
    /// Pause/resume/lap/segment/marker events, in order.
    public var events: [WorkoutEvent]?
    /// Sub-activities for multi-sport / interval workouts (iOS 16+).
    public var activities: [WorkoutActivitySegment]?

    public init(
        activityType: String, duration: TimeInterval,
        totalEnergyKcal: Double? = nil, totalDistanceMeters: Double? = nil,
        statistics: [String: Double]? = nil,
        statisticsDetail: [String: WorkoutStat]? = nil,
        events: [WorkoutEvent]? = nil,
        activities: [WorkoutActivitySegment]? = nil
    ) {
        self.activityType = activityType
        self.duration = duration
        self.totalEnergyKcal = totalEnergyKcal
        self.totalDistanceMeters = totalDistanceMeters
        self.statistics = statistics
        self.statisticsDetail = statisticsDetail
        self.events = events
        self.activities = activities
    }
}

/// Intra-workout time series for one quantity type (heart rate, power, cadence,
/// speed, altitude…), fetched via `HKQuantitySeriesSampleQuery`. Sent as separate
/// NDJSON lines (`{"series": ...}`); long streams split into payloads of at most
/// `maxPointsPerPayload` points so a single line stays bounded.
public struct WorkoutSeriesPayload: Codable, Sendable, Equatable {
    public static let maxPointsPerPayload = 4000

    public var workoutUUID: UUID
    /// HealthKit quantity type identifier, e.g. "HKQuantityTypeIdentifierHeartRate".
    public var type: String
    /// Canonical unit the values are expressed in (see `HealthTypeCatalog`).
    public var unit: String?
    public var points: [SeriesPoint]

    public init(workoutUUID: UUID, type: String, unit: String? = nil, points: [SeriesPoint]) {
        self.workoutUUID = workoutUUID
        self.type = type
        self.unit = unit
        self.points = points
    }
}

/// One datum in a workout series: timestamp + value in the payload's canonical unit.
public struct SeriesPoint: Codable, Sendable, Equatable {
    public var t: Date
    public var temporalContext: TemporalContext?
    public var value: Double

    public init(t: Date, temporalContext: TemporalContext? = nil, value: Double) {
        self.t = t
        self.temporalContext = temporalContext
        self.value = value
    }
}

/// The current user's settings-backed identity (name/email) and characteristics
/// (DOB/sex, used for heart-rate zones). Sent as a single `{"profile": ...}`
/// NDJSON line; all four keys encode explicitly (including JSON null) so the
/// server can replace the batch user's complete profile and honor cleared values.
public struct ProfilePayload: Codable, Sendable, Equatable {
    private enum CodingKeys: String, CodingKey {
        case name, email, dateOfBirth, biologicalSex, capturedAt
    }

    public var name: String?
    public var email: String?
    public var dateOfBirth: Date?
    /// "female" | "male" | "other" | nil
    public var biologicalSex: String?
    public var capturedAt: Date?

    public init(
        name: String? = nil, email: String? = nil,
        dateOfBirth: Date? = nil, biologicalSex: String? = nil, capturedAt: Date? = nil
    ) {
        self.name = name
        self.email = email
        self.dateOfBirth = dateOfBirth
        self.biologicalSex = biologicalSex
        self.capturedAt = capturedAt
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decodeIfPresent(String.self, forKey: .name)
        email = try c.decodeIfPresent(String.self, forKey: .email)
        dateOfBirth = try c.decodeIfPresent(Date.self, forKey: .dateOfBirth)
        biologicalSex = try c.decodeIfPresent(String.self, forKey: .biologicalSex)
        capturedAt = try c.decodeIfPresent(Date.self, forKey: .capturedAt)
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        if let name { try c.encode(name, forKey: .name) }
        else { try c.encodeNil(forKey: .name) }
        if let email { try c.encode(email, forKey: .email) }
        else { try c.encodeNil(forKey: .email) }
        if let dateOfBirth { try c.encode(dateOfBirth, forKey: .dateOfBirth) }
        else { try c.encodeNil(forKey: .dateOfBirth) }
        if let biologicalSex { try c.encode(biologicalSex, forKey: .biologicalSex) }
        else { try c.encodeNil(forKey: .biologicalSex) }
        try c.encodeIfPresent(capturedAt, forKey: .capturedAt)
    }

    public var isEmpty: Bool {
        name == nil && email == nil && dateOfBirth == nil && biologicalSex == nil
    }

    /// Whether Save & Apply should send `profile`, given the one this install
    /// had applied before it.
    ///
    /// The line replaces the server's copy, so an empty profile clears it. That
    /// is right when the user has just emptied the fields, and wrong when the
    /// fields were never filled: a fresh install pairing with a server that
    /// already holds the user's name, e-mail, date of birth and sex used to
    /// erase all four on its first Apply — and with them the inputs heart-rate
    /// zones are computed from. So an empty profile goes out only when it
    /// replaces a non-empty one — or when an earlier clear never reached the
    /// server (`clearPending`): from the Apply after a failed one, the stored
    /// profile is already empty, and without the flag the clear would never be
    /// retried.
    public static func shouldUpload(
        _ profile: ProfilePayload, replacing previous: ProfilePayload, clearPending: Bool = false
    ) -> Bool {
        !profile.isEmpty || !previous.isEmpty || clearPending
    }
}

/// One computed statistics value (covering one time bucket) from an aggregate
/// series; stored in the server's `aggregate_samples` table. One
/// `{"aggregate": ...}` NDJSON line on the wire. Unlike raw samples, these have
/// no UUID: identity is (type, func, interval, deviceFilter, bucketStart) and
/// the server upserts — recomputes overwrite earlier values.
public struct AggregateSampleRow: Codable, Sendable, Equatable {
    public var type: String
    public var function: AggregateFunction
    public var intervalValue: Int
    public var intervalUnit: AggregateIntervalUnit
    public var deviceFilter: AggregateDeviceFilter
    public var bucketStart: Date
    public var bucketEnd: Date
    public var bucketStartContext: TemporalContext?
    public var bucketEndContext: TemporalContext?
    /// Nil = bucket has no matching samples. Encoded as an explicit JSON null
    /// so the server clears stale values on recompute.
    public var value: Double?
    /// Canonical unit for the type ("s" for duration), see `AggregateConfig.unitString`.
    public var unit: String?

    enum CodingKeys: String, CodingKey {
        case type
        case function = "func"
        case intervalValue, intervalUnit, deviceFilter
        case bucketStart, bucketEnd, bucketStartContext, bucketEndContext, value, unit
    }

    public init(
        type: String, function: AggregateFunction,
        intervalValue: Int, intervalUnit: AggregateIntervalUnit,
        deviceFilter: AggregateDeviceFilter,
        bucketStart: Date, bucketEnd: Date,
        bucketStartContext: TemporalContext? = nil, bucketEndContext: TemporalContext? = nil,
        value: Double? = nil, unit: String? = nil
    ) {
        self.type = type
        self.function = function
        self.intervalValue = intervalValue
        self.intervalUnit = intervalUnit
        self.deviceFilter = deviceFilter
        self.bucketStart = bucketStart
        self.bucketEnd = bucketEnd
        self.bucketStartContext = bucketStartContext
        self.bucketEndContext = bucketEndContext
        self.value = value
        self.unit = unit
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(type, forKey: .type)
        try c.encode(function, forKey: .function)
        try c.encode(intervalValue, forKey: .intervalValue)
        try c.encode(intervalUnit, forKey: .intervalUnit)
        try c.encode(deviceFilter, forKey: .deviceFilter)
        try c.encode(bucketStart, forKey: .bucketStart)
        try c.encode(bucketEnd, forKey: .bucketEnd)
        try c.encodeIfPresent(bucketStartContext, forKey: .bucketStartContext)
        try c.encodeIfPresent(bucketEndContext, forKey: .bucketEndContext)
        // Explicit null, not omitted: empty buckets must overwrite on the server.
        if let value {
            try c.encode(value, forKey: .value)
        } else {
            try c.encodeNil(forKey: .value)
        }
        try c.encodeIfPresent(unit, forKey: .unit)
    }
}

/// One daily activity summary (`HKActivitySummary` — the activity rings),
/// stored in the server's `activity_summaries` table. One `{"activitySummary":
/// ...}` NDJSON line on the wire. Like aggregates these have no UUID: identity
/// is the local-calendar `date` and the server upserts — a later recompute of
/// the same day overwrites the earlier one (today's rings change all day).
/// Omitted/null value or goal fields overwrite the server's column with NULL.
public struct ActivitySummaryRow: Codable, Sendable, Equatable {
    /// Start of the local calendar day this summary covers.
    public var date: Date
    /// Calendar date in the source local timezone, e.g. "2024-06-10".
    public var localDate: String?
    public var temporalContext: TemporalContext?
    /// Move ring (active energy), kilocalories.
    public var moveKcal: Double?
    public var moveGoalKcal: Double?
    /// Exercise ring, minutes.
    public var exerciseMin: Double?
    public var exerciseGoalMin: Double?
    /// Stand ring, hours.
    public var standHours: Double?
    public var standGoalHours: Double?
    /// 0 = activeEnergy (Move ring is calories), 1 = appleMoveTime (Move ring is minutes).
    public var moveMode: Int?
    /// Move minutes + goal, populated only for `moveMode == 1` (wheelchair / move-time users).
    public var moveTimeMin: Double?
    public var moveTimeGoalMin: Double?

    public init(
        date: Date, localDate: String? = nil, temporalContext: TemporalContext? = nil,
        moveKcal: Double? = nil, moveGoalKcal: Double? = nil,
        exerciseMin: Double? = nil, exerciseGoalMin: Double? = nil,
        standHours: Double? = nil, standGoalHours: Double? = nil,
        moveMode: Int? = nil, moveTimeMin: Double? = nil, moveTimeGoalMin: Double? = nil
    ) {
        self.date = date
        self.localDate = localDate
        self.temporalContext = temporalContext
        self.moveKcal = moveKcal
        self.moveGoalKcal = moveGoalKcal
        self.exerciseMin = exerciseMin
        self.exerciseGoalMin = exerciseGoalMin
        self.standHours = standHours
        self.standGoalHours = standGoalHours
        self.moveMode = moveMode
        self.moveTimeMin = moveTimeMin
        self.moveTimeGoalMin = moveTimeGoalMin
    }
}

/// A HealthKit deletion tombstone.
public struct SyncDeletion: Codable, Sendable, Equatable {
    public var uuid: UUID
    public var type: String

    public init(uuid: UUID, type: String) {
        self.uuid = uuid
        self.type = type
    }
}

/// One upload unit. Serialized as a JSON header line followed by NDJSON sample lines,
/// or as a single JSON document depending on transport.
public struct SyncBatch: Codable, Sendable {
    /// Stable ID so the server can deduplicate retried uploads.
    public var batchID: UUID
    /// Originating device install, stable across launches.
    public var deviceID: String
    public var type: String
    /// "backfill" | "incremental"
    public var reason: SyncReason
    public var samples: [SyncSample]
    public var deletions: [SyncDeletion]
    /// Route point payloads for workout batches (empty for other types).
    public var routes: [RoutePayload]
    /// Intra-workout time-series payloads for workout batches (empty otherwise).
    public var series: [WorkoutSeriesPayload]
    /// Statistics buckets for aggregate batches (empty for raw-sample batches).
    public var aggregates: [AggregateSampleRow]
    /// Daily activity-summary rows (empty for everything but activity-ring batches).
    public var activitySummaries: [ActivitySummaryRow]
    /// Settings-backed user identity and characteristics.
    public var profile: ProfilePayload?
    public var sourceObservations: [UUID]?
    public var exportedAt: Date

    public init(
        batchID: UUID = UUID(), deviceID: String, type: String, reason: SyncReason,
        samples: [SyncSample], deletions: [SyncDeletion], routes: [RoutePayload] = [],
        series: [WorkoutSeriesPayload] = [], aggregates: [AggregateSampleRow] = [],
        activitySummaries: [ActivitySummaryRow] = [], profile: ProfilePayload? = nil,
        sourceObservations: [UUID]? = nil,
        exportedAt: Date = Date()
    ) {
        self.batchID = batchID
        self.deviceID = deviceID
        self.type = type
        self.reason = reason
        self.samples = samples
        self.deletions = deletions
        self.routes = routes
        self.series = series
        self.aggregates = aggregates
        self.activitySummaries = activitySummaries
        self.profile = profile
        self.sourceObservations = sourceObservations
        self.exportedAt = exportedAt
    }

    public var isEmpty: Bool {
        samples.isEmpty && deletions.isEmpty && routes.isEmpty
            && series.isEmpty && aggregates.isEmpty && activitySummaries.isEmpty
            && profile == nil
    }
}

public enum SyncReason: String, Codable, Sendable {
    case backfill
    case incremental
    case manual
    case reconciliation
}
