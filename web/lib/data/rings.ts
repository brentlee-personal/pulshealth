// Today's activity rings (HKActivitySummary), one row per local day.

import { scoped } from "../db";
import { reportingTimeZone } from "../reportingTimeZone";
import type { ActivityRingsData } from "../types";
import { liveRead } from "./source";

export async function getActivityRings(userId: string): Promise<ActivityRingsData> {
  // Apple's standard goals stand in for any null goal column.
  const fallback: ActivityRingsData = {
    date: null, moveMode: 0,
    moveKcal: 0, moveGoalKcal: 600,
    exerciseMin: 0, exerciseGoalMin: 30,
    standHours: 0, standGoalHours: 12,
    moveTimeMin: null, moveTimeGoalMin: null,
    hasData: false,
  };
  return liveRead("getActivityRings", async () => {
    const timeZone = await reportingTimeZone(userId);
    const rows = await scoped(userId, (q) => q<{
      date: string;
      move_kcal: number | null;
      move_goal_kcal: number | null;
      exercise_min: number | null;
      exercise_goal_min: number | null;
      stand_hours: number | null;
      stand_goal_hours: number | null;
      move_mode: number | null;
      move_time_min: number | null;
      move_time_goal_min: number | null;
    }>(
      `SELECT (extract(epoch from (date::timestamp AT TIME ZONE $2::text)) * 1000)::bigint AS date,
              move_kcal::float8, move_goal_kcal::float8,
              exercise_min::float8, exercise_goal_min::float8,
              stand_hours::float8, stand_goal_hours::float8,
              move_mode, move_time_min::float8, move_time_goal_min::float8
         FROM activity_summaries
        WHERE user_id = $1::uuid
          AND date = (now() AT TIME ZONE $2::text)::date
        LIMIT 1`,
      [userId, timeZone],
    ));
    if (!rows.length) return fallback;
    const r = rows[0];
    return {
      date: Number(r.date),
      moveMode: r.move_mode === 1 ? 1 : 0,
      moveKcal: Number(r.move_kcal ?? 0),
      moveGoalKcal: Number(r.move_goal_kcal ?? 600),
      exerciseMin: Number(r.exercise_min ?? 0),
      exerciseGoalMin: Number(r.exercise_goal_min ?? 30),
      standHours: Number(r.stand_hours ?? 0),
      standGoalHours: Number(r.stand_goal_hours ?? 12),
      moveTimeMin: r.move_time_min == null ? null : Number(r.move_time_min),
      moveTimeGoalMin: r.move_time_goal_min == null ? null : Number(r.move_time_goal_min),
      hasData: true,
    };
  });
}
