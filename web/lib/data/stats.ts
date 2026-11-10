// Catalog-wide stats: rows and date range per type, for one user.

import { scoped } from "../db";
import type { TypeStat } from "../types";
import { staleWhileRevalidate } from "./cache";
import { liveRead } from "./source";

// The heaviest read here: a count over every sample the person has, most of
// it in compressed chunks (seconds for years of Watch data). Counts and date
// ranges drift slowly, so a cached answer is served while it is refreshed in
// the background, and only a process's first read per user waits for it.
const STATS_FRESH_MS = 60_000;

export const getStats: (userId: string) => Promise<Map<string, TypeStat>> = staleWhileRevalidate(STATS_FRESH_MS, loadStats);

async function loadStats(userId: string): Promise<Map<string, TypeStat>> {
  const out = new Map<string, TypeStat>();
  return liveRead("getStats", async () => {
    return await scoped(userId, async (q) => {
      // One statement per table, never a UNION ALL of the two: as web_app
      // (accounts mode) that ran as a Parallel Append whose quantity half
      // returned no rows at all, so every quantity type read as empty.
      for (const table of ["quantity_samples", "category_samples"] as const) {
        const rows = await q<{ identifier: string; rows: string; earliest: string | null; latest: string | null }>(
          `SELECT st.identifier, x.rows, x.earliest, x.latest
             FROM (
               SELECT type_id,
                      count(*)::bigint AS rows,
                      (extract(epoch from min(start_ts)) * 1000)::bigint AS earliest,
                      (extract(epoch from max(start_ts)) * 1000)::bigint AS latest
                 FROM ${table}
                WHERE user_id = $1::uuid
                GROUP BY type_id
             ) x
             JOIN sample_types st ON st.type_id = x.type_id`,
          [userId],
        );
        for (const r of rows) {
          out.set(r.identifier, {
            identifier: r.identifier,
            rows: Number(r.rows),
            earliest: r.earliest == null ? null : Number(r.earliest),
            latest: r.latest == null ? null : Number(r.latest),
          });
        }
      }
      // Workouts live in their own table.
      const wk = await q<{ rows: string; earliest: string | null; latest: string | null }>(
        `SELECT count(*)::bigint AS rows,
                (extract(epoch from min(start_ts)) * 1000)::bigint AS earliest,
                (extract(epoch from max(start_ts)) * 1000)::bigint AS latest
           FROM workouts
          WHERE user_id = $1::uuid`,
        [userId],
      );
      if (wk[0] && Number(wk[0].rows) > 0) {
        out.set("HKWorkoutTypeIdentifier", {
          identifier: "HKWorkoutTypeIdentifier",
          rows: Number(wk[0].rows),
          earliest: wk[0].earliest == null ? null : Number(wk[0].earliest),
          latest: wk[0].latest == null ? null : Number(wk[0].latest),
        });
      }
      return out;
    });
  });
}
