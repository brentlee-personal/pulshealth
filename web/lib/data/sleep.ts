import { scoped } from "../db";
import { reportingTimeZone } from "../reportingTimeZone";
import { sleepSessions, type SleepSample, type SleepSession, type SleepStage } from "../sleep";
import { sleepRangeDates } from "../sleepTimeline";
import type { RangeKey } from "../types";
import { getStats } from "./stats";
import { liveRead } from "./source";

const STAGES: Record<number, SleepStage> = { 0: "inBed", 1: "unspecified", 2: "awake", 3: "core", 4: "deep", 5: "rem" };
const DAY = 86400_000;

// Range bounds follow the metric page selector, with padding for complete nights.
// Keep a start_ts bound for chunk pruning through accounts security barriers.
export async function getSleepSessions(userId: string, range: RangeKey = "30D", now = Date.now()): Promise<SleepSession[]> {
  return liveRead("getSleepSessions", async () => {
    const timeZone = await reportingTimeZone(userId);
    const earliest = range === "ALL" ? (await getStats(userId)).get("HKCategoryTypeIdentifierSleepAnalysis")?.earliest : null;
    if (range === "ALL" && earliest == null) return [];
    const { first, last } = sleepRangeDates(range, now, timeZone, earliest);
    const rows = await scoped(userId, (q) => q<{
      start: string; end: string; value: number; source: string;
    }>(`SELECT (extract(epoch from c.start_ts) * 1000)::bigint AS start,
               (extract(epoch from c.end_ts) * 1000)::bigint AS end,
               c.value, COALESCE(c.source_id, 0)::text AS source
          FROM category_samples c
         WHERE c.user_id = $1::uuid
           AND c.type_id = (SELECT type_id FROM sample_types WHERE identifier = $2)
           AND c.start_ts >= $3::timestamptz
           AND c.start_ts < $4::timestamptz
         ORDER BY c.start_ts, c.end_ts`,
    [userId, "HKCategoryTypeIdentifierSleepAnalysis", new Date(Date.parse(first) - 2 * DAY), new Date(now)]));
    const names = new Map([...new Set(rows.map((r) => r.source))].sort().map((id, index) => [id, `Recording ${index + 1}`]));
    const samples: SleepSample[] = rows.flatMap((r) => STAGES[r.value] ? [{
      start: Number(r.start), end: Math.min(Number(r.end), now), stage: STAGES[r.value], source: r.source, sourceName: names.get(r.source)!,
    }] : []);
    return sleepSessions(samples, timeZone).filter((s) => s.date >= first && s.date <= last);
  });
}
