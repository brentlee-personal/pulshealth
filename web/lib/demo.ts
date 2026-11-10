// Deterministic synthetic data so the UI is fully alive without a database.
// Everything is seeded by identifier + day index, so repeated renders are
// stable (no hydration flicker) yet each type looks distinct and plausible.

import { recordedDemo, recordedSeries } from "./recordedDemo";
import { CATALOG, typeByIdentifier } from "./catalog";
const SYNTHETIC_USER_ID = "5ea4d000-0000-4000-8000-000000000001";
import { isCumulative, RANGES } from "./metrics";
import type {
  ActivityRingsData, Latest, Profile, RangeKey, RoutePoint, Series, SeriesPoint, TypeStat,
  User, Workout, WorkoutActivitySegment, WorkoutDetail, WorkoutEvent, WorkoutSeries, WorkoutStat,
} from "./types";

const DAY = 86_400_000;

function hash(str: string): number {
  let h = 2166136261 >>> 0;
  for (let i = 0; i < str.length; i++) {
    h ^= str.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return h >>> 0;
}

function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

interface Baseline {
  typical: number; // daily total (cumulative) or representative reading (discrete)
  spread: number; // relative noise
  floor: number;
  ceil: number;
}

const KNOWN: Record<string, Baseline> = {
  HKQuantityTypeIdentifierStepCount: { typical: 8800, spread: 0.35, floor: 1500, ceil: 24000 },
  HKQuantityTypeIdentifierDistanceWalkingRunning: { typical: 6400, spread: 0.35, floor: 800, ceil: 22000 },
  HKQuantityTypeIdentifierFlightsClimbed: { typical: 11, spread: 0.5, floor: 0, ceil: 60 },
  HKQuantityTypeIdentifierActiveEnergyBurned: { typical: 620, spread: 0.3, floor: 120, ceil: 1800 },
  HKQuantityTypeIdentifierBasalEnergyBurned: { typical: 1650, spread: 0.08, floor: 1200, ceil: 2100 },
  HKQuantityTypeIdentifierAppleExerciseTime: { typical: 38, spread: 0.5, floor: 0, ceil: 180 },
  HKQuantityTypeIdentifierAppleStandTime: { typical: 720, spread: 0.2, floor: 120, ceil: 960 },
  HKQuantityTypeIdentifierHeartRate: { typical: 70, spread: 0.18, floor: 46, ceil: 165 },
  HKQuantityTypeIdentifierRestingHeartRate: { typical: 57, spread: 0.06, floor: 48, ceil: 72 },
  HKQuantityTypeIdentifierWalkingHeartRateAverage: { typical: 98, spread: 0.08, floor: 80, ceil: 130 },
  HKQuantityTypeIdentifierHeartRateVariabilitySDNN: { typical: 46, spread: 0.25, floor: 18, ceil: 110 },
  HKQuantityTypeIdentifierVO2Max: { typical: 43, spread: 0.04, floor: 35, ceil: 55 },
  HKQuantityTypeIdentifierOxygenSaturation: { typical: 97, spread: 0.012, floor: 92, ceil: 100 },
  HKQuantityTypeIdentifierRespiratoryRate: { typical: 15, spread: 0.12, floor: 10, ceil: 22 },
  HKQuantityTypeIdentifierBodyMass: { typical: 75.5, spread: 0.015, floor: 72, ceil: 80 },
  HKQuantityTypeIdentifierBodyMassIndex: { typical: 23.4, spread: 0.015, floor: 22, ceil: 25 },
  HKQuantityTypeIdentifierBodyFatPercentage: { typical: 18, spread: 0.04, floor: 14, ceil: 24 },
  HKQuantityTypeIdentifierBloodGlucose: { typical: 95, spread: 0.12, floor: 70, ceil: 160 },
  HKQuantityTypeIdentifierBodyTemperature: { typical: 36.7, spread: 0.01, floor: 36, ceil: 38 },
  HKQuantityTypeIdentifierBloodPressureSystolic: { typical: 118, spread: 0.06, floor: 100, ceil: 145 },
  HKQuantityTypeIdentifierBloodPressureDiastolic: { typical: 76, spread: 0.06, floor: 62, ceil: 95 },
  HKQuantityTypeIdentifierDietaryEnergyConsumed: { typical: 2080, spread: 0.22, floor: 900, ceil: 3600 },
  HKQuantityTypeIdentifierDietaryWater: { typical: 1900, spread: 0.3, floor: 300, ceil: 4000 },
  HKQuantityTypeIdentifierDietaryProtein: { typical: 95, spread: 0.25, floor: 30, ceil: 200 },
  HKQuantityTypeIdentifierDietaryCaffeine: { typical: 180, spread: 0.5, floor: 0, ceil: 500 },
  HKCategoryTypeIdentifierSleepAnalysis: { typical: 7.4, spread: 0.12, floor: 4.5, ceil: 9.5 },
  HKQuantityTypeIdentifierEnvironmentalAudioExposure: { typical: 68, spread: 0.1, floor: 45, ceil: 95 },
  HKQuantityTypeIdentifierTimeInDaylight: { typical: 95, spread: 0.6, floor: 0, ceil: 400 },
};

function baselineFor(identifier: string): Baseline {
  const k = KNOWN[identifier];
  if (k) return k;
  const t = typeByIdentifier(identifier);
  const unit = t?.unit ?? "";
  const cum = isCumulative(identifier);
  // Generic, unit-aware fallback.
  if (cum) {
    const seed = (hash(identifier) % 400) + 20;
    return { typical: seed, spread: 0.4, floor: 0, ceil: seed * 4 };
  }
  if (unit === "%") return { typical: 95, spread: 0.03, floor: 80, ceil: 100 };
  if (unit === "count/min") return { typical: 60, spread: 0.2, floor: 30, ceil: 140 };
  if (unit === "degC") return { typical: 36.6, spread: 0.01, floor: 35, ceil: 38 };
  const seed = (hash(identifier) % 80) + 5;
  return { typical: seed, spread: 0.2, floor: seed * 0.4, ceil: seed * 2.2 };
}

function clamp(v: number, lo: number, hi: number): number {
  return Math.max(lo, Math.min(hi, v));
}

// Day index since epoch — stable key for a calendar day.
function dayIndex(ms: number): number {
  return Math.floor(ms / DAY);
}

// A smooth-ish daily figure with weekly seasonality + slow trend + noise.
function dailyValue(identifier: string, b: Baseline, ms: number): number {
  const di = dayIndex(ms);
  const rnd = mulberry32(hash(identifier) ^ (di * 2654435761));
  const dow = new Date(ms).getDay(); // 0 Sun
  const weekend = dow === 0 || dow === 6;
  // Activity dips a touch on weekdays mornings, sleep a touch longer on weekends, etc.
  const season = 1 + Math.sin((di / 7) * Math.PI * 2) * 0.06 + (weekend ? 0.08 : -0.02);
  const trend = 1 + Math.sin(di / 90) * 0.05;
  const noise = 1 + (rnd() - 0.5) * 2 * b.spread;
  return clamp(b.typical * season * trend * noise, b.floor, b.ceil);
}

// Circadian weight for hour-of-day distribution of cumulative activity.
function circadian(hour: number): number {
  // Low overnight, ramps after 7, peaks midday + early evening.
  const pts = [0.2, 0.1, 0.05, 0.05, 0.05, 0.1, 0.4, 0.9, 1.0, 0.9, 0.8, 0.9, 1.0, 0.9, 0.8, 0.8, 0.9, 1.0, 0.9, 0.7, 0.6, 0.5, 0.4, 0.3];
  return pts[hour % 24];
}

export function demoSeries(identifier: string, range: RangeKey): Series {
  const recorded = recordedDemo();
  if (recorded) return recordedSeries(recorded, identifier, range);
  const spec = RANGES[range];
  const b = baselineFor(identifier);
  const cum = isCumulative(identifier);
  const t = typeByIdentifier(identifier);
  const now = Date.now();
  const points: SeriesPoint[] = [];

  if (range === "D") {
    const dayStart = now - (now % DAY);
    const dayTotal = dailyValue(identifier, b, dayStart);
    const nowHour = new Date(now).getHours();
    const weights = Array.from({ length: 24 }, (_, h) => circadian(h));
    const wsum = weights.reduce((a, c) => a + c, 0);
    for (let h = 0; h < 24; h++) {
      const tms = dayStart + h * 3600_000;
      const rnd = mulberry32(hash(identifier) ^ (dayIndex(dayStart) * 131 + h));
      if (cum) {
        const v = h <= nowHour ? (dayTotal * weights[h]) / wsum : 0;
        points.push({ t: tms, value: v, min: null, max: null, count: Math.round(weights[h] * 4) });
      } else {
        const base = dailyValue(identifier, b, dayStart);
        const hourFactor = 0.85 + circadian(h) * 0.3;
        const v = h <= nowHour ? clamp(base * hourFactor * (0.95 + rnd() * 0.1), b.floor, b.ceil) : NaN;
        const spreadAbs = base * b.spread * 0.6;
        if (!Number.isNaN(v))
          points.push({ t: tms, value: v, min: clamp(v - spreadAbs, b.floor, b.ceil), max: clamp(v + spreadAbs, b.floor, b.ceil), count: Math.round(circadian(h) * 30) + 1 });
        else points.push({ t: tms, value: NaN, min: null, max: null, count: 0 });
      }
    }
    return { identifier, unit: t?.unit ?? null, agg: cum ? "sum" : "avg", bucketMs: spec.bucketMs, points: points.filter((p) => !Number.isNaN(p.value)) };
  }

  // ALL has no fixed span; five years stands in for the demo.
  const buckets = Math.round((spec.spanMs ?? 5 * 365 * DAY) / spec.bucketMs);
  for (let i = buckets - 1; i >= 0; i--) {
    const tms = now - i * spec.bucketMs;
    if (spec.bucketMs === DAY) {
      const v = dailyValue(identifier, b, tms);
      const spreadAbs = v * b.spread;
      points.push({ t: tms - (tms % DAY), value: v, min: cum ? null : clamp(v - spreadAbs, b.floor, b.ceil), max: cum ? null : clamp(v + spreadAbs, b.floor, b.ceil), count: Math.round((t?.perDay ?? 10)) });
    } else {
      // weekly/larger bucket: aggregate the days inside it
      const days = Math.round(spec.bucketMs / DAY);
      let sum = 0;
      let min = Infinity;
      let max = -Infinity;
      for (let d = 0; d < days; d++) {
        const dv = dailyValue(identifier, b, tms - d * DAY);
        sum += dv;
        min = Math.min(min, dv);
        max = Math.max(max, dv);
      }
      const value = cum ? sum : sum / days;
      points.push({ t: tms, value, min: cum ? null : min, max: cum ? null : max, count: days * (t?.perDay ?? 10) });
    }
  }
  return { identifier, unit: t?.unit ?? null, agg: cum ? "sum" : "avg", bucketMs: spec.bucketMs, points };
}

export function demoLatest(identifier: string): Latest | null {
  const recorded = recordedDemo();
  if (recorded) return recorded.latest[identifier] ?? null;
  const b = baselineFor(identifier);
  const t = typeByIdentifier(identifier);
  const now = Date.now();
  const cum = isCumulative(identifier);
  let value: number;
  if (cum) {
    value = demoTodaySum(identifier);
  } else {
    const rnd = mulberry32(hash(identifier) ^ dayIndex(now));
    value = clamp(b.typical * (0.97 + rnd() * 0.06), b.floor, b.ceil);
  }
  // last reading a few minutes/hours ago
  const ago = (hash(identifier) % 90) * 60_000 + 120_000;
  return { identifier, value, unit: t?.unit ?? null, t: now - ago };
}

export function demoTodaySum(identifier: string): number {
  const recorded = recordedDemo();
  if (recorded) return recorded.daily[identifier]?.find(p => p.t === recorded.today)?.value ?? 0;
  const b = baselineFor(identifier);
  const now = Date.now();
  const dayStart = now - (now % DAY);
  const dayTotal = dailyValue(identifier, b, dayStart);
  // fraction of the day elapsed, weighted by circadian activity
  const nowHour = new Date(now).getHours();
  const weights = Array.from({ length: 24 }, (_, h) => circadian(h));
  const wsum = weights.reduce((a, c) => a + c, 0);
  const done = weights.slice(0, nowHour + 1).reduce((a, c) => a + c, 0);
  return (dayTotal * done) / wsum;
}

export function demoActivityRings(): ActivityRingsData {
  const recorded = recordedDemo();
  if (recorded) return recorded.rings;
  const now = Date.now();
  const hour = new Date(now).getHours();
  const move = demoTodaySum("HKQuantityTypeIdentifierActiveEnergyBurned");
  const exercise = demoTodaySum("HKQuantityTypeIdentifierAppleExerciseTime");
  // Stand hours accumulate roughly one per waking hour, capped at the 12 goal.
  const standHours = clamp(Math.round((hour - 6) * 0.8), 0, 12);
  return {
    date: now - (now % DAY),
    moveMode: 0,
    moveKcal: Math.round(move),
    moveGoalKcal: 600,
    exerciseMin: Math.round(exercise),
    exerciseGoalMin: 30,
    standHours,
    standGoalHours: 12,
    moveTimeMin: null,
    moveTimeGoalMin: null,
    hasData: true,
  };
}

export function demoStats(): TypeStat[] {
  const recorded = recordedDemo();
  if (recorded) return recorded.stats;
  const now = Date.now();
  return CATALOG.map((ty) => {
    const rnd = mulberry32(hash(ty.identifier));
    const historyDays = 200 + Math.floor(rnd() * 600);
    const perDay = Math.max(1, ty.perDay);
    const rows = Math.round(perDay * historyDays * (0.7 + rnd() * 0.6));
    return {
      identifier: ty.identifier,
      rows,
      earliest: now - historyDays * DAY,
      latest: now - Math.floor(rnd() * 4) * 3600_000,
    };
  });
}

const ACTIVITIES = ["Running", "Outdoor Walk", "Cycling", "Strength Training", "HIIT", "Yoga", "Pool Swim", "Hiking", "Rowing", "Elliptical"];

export function demoWorkouts(limit = 40): Workout[] {
  const recorded = recordedDemo();
  if (recorded) return recorded.workouts.slice(0, limit).map(({ uuid, activityType, start, end, durationS, energyKcal, distanceM }) => ({ uuid, activityType, start, end, durationS, energyKcal, distanceM }));
  const now = Date.now();
  const out: Workout[] = [];
  let cursor = now - 6 * 3600_000;
  let i = 0;
  while (out.length < limit && i < limit * 4) {
    const rnd = mulberry32(hash("workout") ^ (i * 2654435761));
    i++;
    // ~ every 1.4 days, with jitter
    cursor -= Math.floor((0.8 + rnd() * 1.6) * DAY);
    if (rnd() < 0.18) continue; // rest gap
    const activity = ACTIVITIES[Math.floor(rnd() * ACTIVITIES.length)];
    const durMin = 18 + Math.floor(rnd() * 75);
    const durationS = durMin * 60;
    const distanceM = ["Running", "Outdoor Walk", "Cycling", "Hiking", "Pool Swim", "Rowing"].includes(activity)
      ? Math.round((activity === "Cycling" ? 5500 : activity === "Pool Swim" ? 900 : 2200) * (durMin / 30) * (0.8 + rnd() * 0.5))
      : null;
    const energyKcal = Math.round((6 + rnd() * 7) * durMin);
    const start = cursor;
    out.push({ uuid: `demo-${i}`, activityType: activity, start, end: start + durationS * 1000, durationS, energyKcal, distanceM });
  }
  return out;
}

// Activities that carry an outdoor GPS route in the demo set.
const ROUTED = new Set(["Running", "Outdoor Walk", "Cycling", "Hiking", "Rowing"]);

// A handful of plausible starting locations so different demo workouts sit in
// different places (purely synthetic — coordinates near public parks).
const ORIGINS: [number, number][] = [
  [37.7694, -122.4862], // Golden Gate Park, SF
  [40.7829, -73.9654], // Central Park, NYC
  [51.5073, -0.1657], // Hyde Park, London
  [47.6553, -122.3035], // Seattle
  [34.0736, -118.4004], // LA
];

// Synthesize a wobbly loop whose length roughly matches the workout distance,
// with rolling altitude and per-point speed. Deterministic in the uuid.
function demoRoute(uuid: string, w: Workout): RoutePoint[] {
  if (!w.distanceM) return [];
  const rnd = mulberry32(hash(uuid) ^ 0x9e3779b9);
  const [oLat, oLon] = ORIGINS[Math.floor(rnd() * ORIGINS.length)];
  const baseAlt = 5 + Math.floor(rnd() * 180);

  // Loop "radius" so circumference ≈ distance; spread over a few harmonics.
  const radiusM = w.distanceM / (2 * Math.PI);
  const mPerDegLat = 111_320;
  const mPerDegLon = 111_320 * Math.cos((oLat * Math.PI) / 180);
  const ph1 = rnd() * Math.PI * 2;
  const ph2 = rnd() * Math.PI * 2;
  const ph3 = rnd() * Math.PI * 2;

  const n = Math.max(24, Math.min(600, Math.round(w.durationS / 8)));
  const avgSpeed = w.distanceM / Math.max(1, w.durationS); // m/s
  const out: RoutePoint[] = [];
  for (let i = 0; i < n; i++) {
    const u = i / (n - 1);
    const ang = u * Math.PI * 2;
    // Organic shape: base loop + two smaller harmonics.
    const dxM = radiusM * (Math.cos(ang + ph1) + 0.28 * Math.cos(3 * ang + ph2));
    const dyM = radiusM * (Math.sin(ang + ph1) + 0.22 * Math.sin(2 * ang + ph3));
    const jitter = (rnd() - 0.5) * radiusM * 0.04;
    const lat = oLat + (dyM + jitter) / mPerDegLat;
    const lon = oLon + (dxM + jitter) / mPerDegLon;
    const altitude = baseAlt + Math.sin(ang * 2 + ph2) * 24 + Math.sin(ang * 5 + ph3) * 6;
    const speed = Math.max(0, avgSpeed * (0.78 + 0.5 * (0.5 + 0.5 * Math.sin(ang * 3 + ph1)) + (rnd() - 0.5) * 0.15));
    out.push({ t: w.start + Math.round(u * w.durationS * 1000), lat, lon, altitude, speed });
  }
  return out;
}

// Per-metric statistics, mirroring what HealthKit attaches to a workout
// (HK identifier → value in canonical unit).
function demoWorkoutStats(uuid: string, w: Workout): Record<string, number> {
  const rnd = mulberry32(hash(uuid) ^ 0x85ebca6b);
  const durMin = w.durationS / 60;
  const stats: Record<string, number> = {};
  if (w.energyKcal != null) stats.HKQuantityTypeIdentifierActiveEnergyBurned = w.energyKcal;
  // Effort drives heart rate; vigorous activities run hotter.
  const hot = ["Running", "HIIT", "Cycling", "Rowing"].includes(w.activityType);
  const hrBase = (hot ? 148 : 122) + (rnd() - 0.5) * 16;
  stats.HKQuantityTypeIdentifierHeartRate = Math.round(clamp(hrBase, 95, 178));
  if (w.distanceM != null) {
    if (["Running", "Outdoor Walk", "Hiking"].includes(w.activityType)) {
      stats.HKQuantityTypeIdentifierDistanceWalkingRunning = w.distanceM;
      stats.HKQuantityTypeIdentifierStepCount = Math.round(w.distanceM * (1.35 + rnd() * 0.1));
    } else if (w.activityType === "Cycling") {
      stats.HKQuantityTypeIdentifierDistanceCycling = w.distanceM;
    }
  }
  stats.HKQuantityTypeIdentifierActiveEnergyBurned ??= Math.round((6 + rnd() * 6) * durMin);
  return stats;
}

// Min/avg/max/sum detail mirroring the client's WorkoutStat. Discrete metrics
// (HR/power/cadence/speed) get min/avg/max; cumulative (energy/distance/steps) get sum.
function demoWorkoutStatsDetail(uuid: string, w: Workout, flat: Record<string, number>): Record<string, WorkoutStat> {
  const rnd = mulberry32(hash(uuid) ^ 0xc2b2ae35);
  const out: Record<string, WorkoutStat> = {};
  const cumulative = new Set([
    "HKQuantityTypeIdentifierActiveEnergyBurned",
    "HKQuantityTypeIdentifierDistanceWalkingRunning",
    "HKQuantityTypeIdentifierDistanceCycling",
    "HKQuantityTypeIdentifierStepCount",
  ]);
  for (const [id, v] of Object.entries(flat)) {
    if (cumulative.has(id)) {
      out[id] = { min: null, avg: null, max: null, sum: v };
    } else {
      const avg = v;
      out[id] = {
        min: Math.round(avg * (0.7 - rnd() * 0.1)),
        avg: Math.round(avg),
        max: Math.round(avg * (1.18 + rnd() * 0.12)),
        sum: null,
      };
    }
  }
  return out;
}

// A few plausible lap/segment events spread across the workout.
function demoWorkoutEvents(uuid: string, w: Workout): WorkoutEvent[] {
  if (w.durationS < 600) return [];
  const out: WorkoutEvent[] = [];
  const laps = Math.min(6, Math.floor(w.durationS / 600));
  for (let i = 1; i <= laps; i++) {
    out.push({ type: "lap", start: w.start + Math.round((i / (laps + 1)) * w.durationS * 1000), end: null });
  }
  return out;
}

// HIIT/interval workouts get two demo sub-activities; everything else is single.
function demoActivities(uuid: string, w: Workout, detail: Record<string, WorkoutStat>): WorkoutActivitySegment[] {
  if (!["HIIT", "Functional Strength Training"].includes(w.activityType)) return [];
  const half = w.start + Math.round((w.durationS * 1000) / 2);
  return [
    { activityType: w.activityType, start: w.start, end: half, durationS: w.durationS / 2, statistics: detail },
    { activityType: w.activityType, start: half, end: w.end, durationS: w.durationS / 2, statistics: detail },
  ];
}

// Intra-workout streams: an HR curve for everything, plus speed/power/cadence
// where the activity warrants it. Deterministic in the uuid.
export function demoWorkoutSeries(uuid: string): WorkoutSeries[] {
  const recorded = recordedDemo();
  if (recorded) return recorded.workoutSeries[uuid] ?? [];
  const w = demoWorkouts(600).find((x) => x.uuid === uuid);
  if (!w) return [];
  const rnd = mulberry32(hash(uuid) ^ 0x27d4eb2f);
  const n = Math.max(20, Math.min(360, Math.round(w.durationS / 10)));
  const hot = ["Running", "HIIT", "Cycling", "Rowing"].includes(w.activityType);
  const hrBase = hot ? 150 : 124;
  const ph = rnd() * Math.PI * 2;

  const make = (id: string, unit: string | null, fn: (u: number) => number): WorkoutSeries => ({
    type: id, unit,
    points: Array.from({ length: n }, (_, i) => {
      const u = i / (n - 1);
      return { t: w.start + Math.round(u * w.durationS * 1000), value: Math.max(0, fn(u)) };
    }),
  });

  const out: WorkoutSeries[] = [
    make("HKQuantityTypeIdentifierHeartRate", "count/min", (u) =>
      Math.round(hrBase + 22 * Math.sin(u * Math.PI * 3 + ph) + 14 * u + (rnd() - 0.5) * 8)),
  ];
  if (w.activityType === "Running") {
    out.push(make("HKQuantityTypeIdentifierRunningPower", "W", (u) => Math.round(280 + 60 * Math.sin(u * Math.PI * 4 + ph) + (rnd() - 0.5) * 30)));
    out.push(make("HKQuantityTypeIdentifierRunningSpeed", "m/s", (u) => 3.2 + 0.8 * Math.sin(u * Math.PI * 5 + ph) + (rnd() - 0.5) * 0.2));
  } else if (w.activityType === "Cycling") {
    out.push(make("HKQuantityTypeIdentifierCyclingPower", "W", (u) => Math.round(210 + 70 * Math.sin(u * Math.PI * 4 + ph) + (rnd() - 0.5) * 40)));
    out.push(make("HKQuantityTypeIdentifierCyclingCadence", "count/min", (u) => Math.round(86 + 12 * Math.sin(u * Math.PI * 6 + ph) + (rnd() - 0.5) * 6)));
  }
  return out;
}

// The one demo user, under the seeded id so it matches the default viewer user.
export function demoUsers(): User[] {
  return [{ id: SYNTHETIC_USER_ID, name: "Demo", email: null }];
}

export function demoProfile(): Profile {
  const now = new Date();
  const dobMs = Date.UTC(1990, 5, 15);
  let age = now.getFullYear() - 1990;
  if (now.getMonth() < 5 || (now.getMonth() === 5 && now.getDate() < 15)) age--;
  return { dob: dobMs, biologicalSex: "male", age, maxHr: 220 - age, restingHr: 57 };
}

export function demoWorkoutDetail(uuid: string): WorkoutDetail | null {
  const recorded = recordedDemo();
  if (recorded) return recorded.workouts.find(w => w.uuid === uuid) ?? null;
  const w = demoWorkouts(600).find((x) => x.uuid === uuid);
  if (!w) return null;
  const route = ROUTED.has(w.activityType) ? demoRoute(uuid, w) : [];
  const stats = demoWorkoutStats(uuid, w);
  const statsDetail = demoWorkoutStatsDetail(uuid, w, stats);
  return {
    ...w,
    stats,
    statsDetail,
    events: demoWorkoutEvents(uuid, w),
    activities: demoActivities(uuid, w, statsDetail),
    metadata: {
      HKIndoorWorkout: route.length ? false : true,
      HKWeatherTemperature: `${Math.round(8 + (hash(uuid) % 22))} °C`,
    },
    source: route.length ? "Apple Watch" : "iPhone",
    route,
  };
}
