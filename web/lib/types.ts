// Shared shapes returned by the data layer. Real (Postgres) and demo sources
// both produce exactly these, so the UI never knows which backend it's on.

export type RangeKey = "D" | "7D" | "30D" | "90D" | "6M" | "Y" | "2Y" | "5Y" | "ALL";

export interface TypeStat {
  identifier: string;
  rows: number;
  earliest: number | null; // epoch ms
  latest: number | null; // epoch ms
}

export interface SeriesPoint {
  t: number; // bucket start, epoch ms
  value: number; // the headline value for this bucket (sum or avg per aggKind)
  min: number | null;
  max: number | null;
  count: number;
}

export interface Series {
  identifier: string;
  unit: string | null;
  agg: "sum" | "avg";
  bucketMs: number;
  points: SeriesPoint[];
}

export interface Workout {
  uuid: string;
  activityType: string;
  start: number; // epoch ms
  end: number; // epoch ms
  durationS: number;
  energyKcal: number | null;
  distanceM: number | null;
}

export interface RoutePoint {
  t: number; // epoch ms
  lat: number;
  lon: number;
  altitude: number | null; // meters
  speed: number | null; // m/s
}

// Per-type aggregate over a workout (or one of its activities), in the catalog's
// canonical unit. Mirrors the client's WorkoutStat.
export interface WorkoutStat {
  min: number | null;
  avg: number | null;
  max: number | null;
  sum: number | null;
}

// A workout event marker (pause/resume/lap/segment/marker/…). `end` set for spans.
export interface WorkoutEvent {
  type: string;
  start: number; // epoch ms
  end: number | null; // epoch ms
}

// One sub-activity of a multi-sport / interval workout.
export interface WorkoutActivitySegment {
  activityType: string;
  start: number; // epoch ms
  end: number | null;
  durationS: number;
  statistics: Record<string, WorkoutStat>;
}

// One intra-workout time series (heart rate, power, cadence, speed, …) in the
// type's canonical unit.
export interface WorkoutSeries {
  type: string; // HealthKit identifier
  unit: string | null;
  points: { t: number; value: number }[]; // epoch ms, value
}

// A workout plus everything we can show on its detail page: the per-metric
// statistics map (HealthKit identifier → value in the catalog's canonical
// unit), richer min/avg/max/sum stats, events, sub-activities, arbitrary
// metadata, the recording source, and the GPS route.
export interface WorkoutDetail extends Workout {
  stats: Record<string, number>;
  statsDetail: Record<string, WorkoutStat>;
  events: WorkoutEvent[];
  activities: WorkoutActivitySegment[];
  metadata: Record<string, unknown>;
  source: string | null;
  route: RoutePoint[];
}

// One row of the `users` table, as the switcher lists them. `name` and
// `email` arrive with the phone's first {"profile":…} line and stay null
// until then, so the UI falls back to the id.
export interface User {
  id: string; // uuid
  name: string | null;
  email: string | null;
}

// User characteristics for derived metrics.
export interface Profile {
  dob: number | null; // epoch ms of date of birth
  biologicalSex: string | null;
  age: number | null;
  maxHr: number; // 220 − age, or DEFAULT_MAX_HR fallback
  restingHr: number | null; // latest HKQuantityTypeIdentifierRestingHeartRate, bpm
}

export interface Latest {
  identifier: string;
  value: number;
  unit: string | null;
  t: number; // epoch ms
}

// The newest daily activity summary (HKActivitySummary — the activity rings).
// Goals fall back to Apple's defaults when the stored value is null. `hasData`
// is false when no activity_summaries row exists yet, so the dashboard can fall
// back to quantity-totals rings.
export interface ActivityRingsData {
  date: number | null; // epoch ms of the summary's local day, null when no data
  moveMode: 0 | 1; // 0 = activeEnergy (Move ring is Calories), 1 = appleMoveTime (minutes)
  moveKcal: number;
  moveGoalKcal: number;
  exerciseMin: number;
  exerciseGoalMin: number;
  standHours: number;
  standGoalHours: number;
  moveTimeMin: number | null;
  moveTimeGoalMin: number | null;
  hasData: boolean;
}

export interface DataSourceInfo {
  // "error": no usable database (unset, unreachable, or the wrong role in
  // accounts mode) and demo fallback is disabled (production). Pages show
  // "Database unavailable" (lib/data/source.ts), never fabricated demo data.
  source: "live" | "error";
  detail: string;
}
