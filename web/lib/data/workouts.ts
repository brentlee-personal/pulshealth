// Workouts: the list, one workout with its route, and its series streams.

import { cache } from "react";
import { scoped } from "../db";
import type {
  RoutePoint,
  Workout,
  WorkoutActivitySegment,
  WorkoutDetail,
  WorkoutEvent,
  WorkoutSeries,
  WorkoutStat,
} from "../types";
import { isUuid } from "../uuid";
import { liveRead } from "./source";

// ── workouts ─────────────────────────────────────────────────────────────
export async function getWorkouts(userId: string, limit = 40): Promise<Workout[]> {
  return liveRead("getWorkouts", async () => {
    const rows = await scoped(userId, (q) => q<{
      uuid: string;
      activity_type: string;
      start: string;
      end: string;
      duration_s: number | null;
      energy_kcal: number | null;
      distance_m: number | null;
    }>(
      `SELECT uuid::text,
              activity_type,
              (extract(epoch from start_ts) * 1000)::bigint AS start,
              (extract(epoch from end_ts) * 1000)::bigint AS end,
              duration_s::float8,
              energy_kcal::float8,
              distance_m::float8
         FROM workouts
        WHERE user_id = $2::uuid
        ORDER BY start_ts DESC
        LIMIT $1`,
      [limit, userId],
    ));
    if (!rows.length) return [];
    return rows.map((r) => ({
      uuid: r.uuid,
      activityType: r.activity_type || "Workout",
      start: Number(r.start),
      end: Number(r.end),
      durationS: Number(r.duration_s ?? 0),
      energyKcal: r.energy_kcal == null ? null : Number(r.energy_kcal),
      distanceM: r.distance_m == null ? null : Number(r.distance_m),
    }));
  });
}

// ── one workout + its route ───────────────────────────────────────────────
// React-cached per request on (userId, uuid): generateMetadata and the page
// both ask for the same workout.
export const getWorkoutDetail = cache(async function getWorkoutDetail(userId: string, uuid: string): Promise<WorkoutDetail | null> {
  return liveRead("getWorkoutDetail", async () => {
    // A non-uuid path segment can never match a live row; skip the query (and
    // the cast error) and report not-found.
    if (!isUuid(uuid)) return null;
    return await scoped(userId, async (q) => {
      const rows = await q<{
        uuid: string;
        activity_type: string;
        start: string;
        end: string;
        duration_s: number | null;
        energy_kcal: number | null;
        distance_m: number | null;
        stats: Record<string, number> | null;
        stats_detail: Record<string, WorkoutStat> | null;
        events: WorkoutEvent[] | null;
        activities: RawActivity[] | null;
        metadata: Record<string, unknown> | null;
        source: string | null;
      }>(
        `SELECT w.uuid::text,
                w.activity_type,
                (extract(epoch from w.start_ts) * 1000)::bigint AS start,
                (extract(epoch from w.end_ts) * 1000)::bigint AS end,
                w.duration_s::float8,
                w.energy_kcal::float8,
                w.distance_m::float8,
                w.stats,
                w.stats_detail,
                w.events,
                w.activities,
                w.metadata,
                s.name AS source
           FROM workouts w
           LEFT JOIN sources s ON s.source_id = w.source_id
          WHERE w.uuid = $1::uuid
            AND w.user_id = $2::uuid`,
        [uuid, userId],
      );
      const r = rows[0];
      if (!r) return null;

      const routeRows = await q<{
        t: string;
        lat: number;
        lon: number;
        altitude_m: number | null;
        speed_mps: number | null;
      }>(
        `SELECT (extract(epoch from ts) * 1000)::bigint AS t,
                lat::float8, lon::float8, altitude_m::float8, speed_mps::float8
           FROM workout_route_points
          WHERE workout_uuid = $1::uuid
            AND user_id = $2::uuid
          ORDER BY ts`,
        [uuid, userId],
      );
      const route: RoutePoint[] = routeRows.map((p) => ({
        t: Number(p.t),
        lat: Number(p.lat),
        lon: Number(p.lon),
        altitude: p.altitude_m == null ? null : Number(p.altitude_m),
        speed: p.speed_mps == null ? null : Number(p.speed_mps),
      }));

      const activities: WorkoutActivitySegment[] = (r.activities ?? []).map((a) => ({
        activityType: a.activityType || "Workout",
        start: Number(a.start),
        end: a.end == null ? null : Number(a.end),
        durationS: Number(a.duration ?? 0),
        statistics: a.statistics ?? {},
      }));

      return {
        uuid: r.uuid,
        activityType: r.activity_type || "Workout",
        start: Number(r.start),
        end: Number(r.end),
        durationS: Number(r.duration_s ?? 0),
        energyKcal: r.energy_kcal == null ? null : Number(r.energy_kcal),
        distanceM: r.distance_m == null ? null : Number(r.distance_m),
        stats: r.stats ?? {},
        statsDetail: r.stats_detail ?? {},
        events: r.events ?? [],
        activities,
        metadata: r.metadata ?? {},
        source: r.source,
        route,
      };
    });
  });
});

// Raw activity shape as stored in the workouts.activities jsonb (Swift field
// names: `duration` in seconds).
interface RawActivity {
  activityType: string;
  start: number;
  end: number | null;
  duration: number;
  statistics: Record<string, WorkoutStat> | null;
}

// ── one workout's intra-workout series streams ────────────────────────────
export async function getWorkoutSeries(userId: string, uuid: string): Promise<WorkoutSeries[]> {
  return liveRead("getWorkoutSeries", async () => {
    if (!isUuid(uuid)) return [];
    const rows = await scoped(userId, (q) => q<{ identifier: string; unit: string | null; t: string; value: number }>(
      `SELECT st.identifier,
              st.unit,
              (extract(epoch from p.ts) * 1000)::bigint AS t,
              p.value::float8
         FROM workout_series_points p
         JOIN sample_types st ON st.type_id = p.type_id
        WHERE p.workout_uuid = $1::uuid
          AND p.user_id = $2::uuid
        ORDER BY st.identifier, p.ts`,
      [uuid, userId],
    ));
    const byType = new Map<string, WorkoutSeries>();
    for (const r of rows) {
      let s = byType.get(r.identifier);
      if (!s) {
        s = { type: r.identifier, unit: r.unit, points: [] };
        byType.set(r.identifier, s);
      }
      s.points.push({ t: Number(r.t), value: Number(r.value) });
    }
    return [...byType.values()];
  });
}
