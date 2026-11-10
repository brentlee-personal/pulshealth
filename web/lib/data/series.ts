// Chart series, latest readings, Today's totals and sparklines: the
// per-type quantity and category reads. Every one runs inside `scoped()` and
// issues its statements through that callback's `q` only; anything that reads
// through `query()` (the metric_daily zone gate, All Time's stats) is decided
// before the transaction opens.
//
// In accounts mode every table here is a security-barrier view (schema web,
// 015_web_accounts.sql), and Postgres pushes a caller's condition into one
// only when it is leakproof — never a join condition, and never a comparison
// between a timestamptz column and a `timestamp`. A filter that stays outside
// the view reaches neither the chunk exclusion nor the indexes: a read that
// takes milliseconds as grafana scans every chunk as web_app. So a type is
// picked with `type_id = (SELECT … FROM sample_types …)` (an InitPlan, a
// constant by the time the scan starts), not with a join on sample_types,
// and every time bound is a timestamptz.

import { scoped, type QueryFn } from "../db";
import { typeByIdentifier } from "../catalog";
import { reportingTimeZone } from "../reportingTimeZone";
import { defaultAgg, RANGES, resolvePresetWindow } from "../metrics";
import type { Latest, RangeKey, Series, SeriesPoint } from "../types";
import { DAY_MS, metricDailyUsable } from "./metricDaily";
import { liveRead } from "./source";
import { getStats } from "./stats";

// ── per-type time series ─────────────────────────────────────────────────
interface SeriesRow {
  t: string;
  sum: number | null;
  avg: number | null;
  min: number | null;
  max: number | null;
  n: number;
}

const SLEEP_ASLEEP_VALUES = [1, 3, 4, 5]; // asleepUnspecified, core, deep, rem

export type CategoryAggregation =
  | {
      mode: "duration";
      unit: "h" | "min";
      values?: number[];
      /** Shift samples forward by this many hours before day-bucketing, so a
       *  session that straddles midnight lands on one day. */
      dayOffsetHours?: number;
    }
  | { mode: "count"; unit: "count" | "h"; values?: number[] };

// Apple Health attributes a night's sleep to the day you wake up, with the
// day boundary at 6 PM: anything that starts after 18:00 belongs to the next
// calendar day. Bucketing raw stage samples by start_ts instead split every
// night across two days (23:00–00:00 on day N, the rest on N+1), so the
// dashboard headline showed only the post-midnight portion.
const SLEEP_DAY_OFFSET_HOURS = 6;

export function categoryAggregation(identifier: string): CategoryAggregation {
  if (identifier === "HKCategoryTypeIdentifierSleepAnalysis") {
    return {
      mode: "duration", unit: "h", values: SLEEP_ASLEEP_VALUES, dayOffsetHours: SLEEP_DAY_OFFSET_HOURS,
    };
  }
  if (identifier === "HKCategoryTypeIdentifierMindfulSession") {
    return { mode: "duration", unit: "min" };
  }
  if (identifier === "HKCategoryTypeIdentifierAppleStandHour") {
    return { mode: "count", unit: "h", values: [0] }; // stood only; 1 record = 1 hour
  }
  return { mode: "count", unit: "count" };
}


/** One chart series over a preset range (what the selector and `?range=` offer). */
export async function getSeries(userId: string, identifier: string, range: RangeKey): Promise<Series> {
  const type = typeByIdentifier(identifier);
  const agg = defaultAgg(identifier);
  const empty: Series = { identifier, unit: type?.unit ?? null, agg, bucketMs: RANGES[range].bucketMs, points: [] };
  return liveRead("getSeries", async () => {
    // `from` is an instant (now − span). Every query below aligns it down to
    // the start of the bucket that contains it, in the viewer's zone, via
    // time_bucket($1, $from, $tz): otherwise the first day/week bucket held a
    // partial slice (10:37 → midnight), rendered as a low bar, and became the
    // range's "Minimum".
    //
    // All Time starts at the type's earliest sample, from the per-user stats
    // the type page loads anyway (cached, and shared while in flight), so it
    // costs no query of its own there.
    const earliest = range === "ALL" ? (await getStats(userId)).get(identifier)?.earliest ?? null : null;
    const window = resolvePresetWindow(range, new Date(), earliest);
    if (!window) return empty; // All Time with no samples at all
    const { start: from, bucket, bucketMs } = window;
    const timeZone = await reportingTimeZone(userId);
    // Decided before the transaction: it reads the database's zone through
    // query(), which must not run inside a scoped callback.
    const dailyUsable = type?.kind !== "category" && (await metricDailyUsable(userId));

    return await scoped(userId, async (q) => {
      if (type?.kind === "category") {
        const category = categoryAggregation(identifier);
        const valueFilter = category.values ? "AND c.value = ANY($6::int[])" : "";
        const params: unknown[] = category.values
          ? [bucket, identifier, from, userId, timeZone, category.values]
          : [bucket, identifier, from, userId, timeZone];

        if (category.mode === "duration") {
          const divisor = category.unit === "h" ? 3600 : 60;
          // Code constants, not request input.
          const shift = category.dayOffsetHours ? ` + interval '${category.dayOffsetHours} hours'` : "";
          const unshift = category.dayOffsetHours ? ` - interval '${category.dayOffsetHours} hours'` : "";
          // Durations overlap across sources the same way cumulative quantities
          // do (Watch stages alongside a third-party app's asleepUnspecified),
          // so establish each bucket's truth as the highest single-source total
          // rather than summing everything.
          const rows = await q<{ t: string; value: number }>(
            `WITH per_source AS (
               SELECT time_bucket($1::interval, c.start_ts${shift}, $5::text) AS t,
                      c.source_id,
                      (sum(extract(epoch from (c.end_ts - c.start_ts))) / ${divisor}.0)::float8 AS value
                 FROM category_samples c
                WHERE c.type_id = (SELECT type_id FROM sample_types WHERE identifier = $2)
                  AND c.start_ts >= time_bucket($1::interval, $3::timestamptz, $5::text)${unshift}
                  AND c.user_id = $4::uuid
                  ${valueFilter}
                GROUP BY 1, c.source_id
             )
             SELECT (extract(epoch from t) * 1000)::bigint AS t,
                    max(value)::float8 AS value
               FROM per_source
              GROUP BY t ORDER BY t`,
            params,
          );
          const points: SeriesPoint[] = rows.map((r) => ({
            t: Number(r.t), value: Number(r.value), min: null, max: null, count: 0,
          }));
          return { identifier, unit: category.unit, agg: "sum", bucketMs, points };
        }

        const rows = await q<{ t: string; n: number }>(
          `SELECT (extract(epoch from time_bucket($1::interval, c.start_ts, $5::text)) * 1000)::bigint AS t,
                  count(*)::int AS n
             FROM category_samples c
            WHERE c.type_id = (SELECT type_id FROM sample_types WHERE identifier = $2)
              AND c.start_ts >= time_bucket($1::interval, $3::timestamptz, $5::text)
              AND c.user_id = $4::uuid
              ${valueFilter}
            GROUP BY 1 ORDER BY 1`,
          params,
        );
        const points: SeriesPoint[] = rows.map((r) => ({
          t: Number(r.t), value: Number(r.n), min: null, max: null, count: Number(r.n),
        }));
        return { identifier, unit: category.unit, agg: "sum", bucketMs, points };
      }

      // Best-guess-of-truth view for covered types (steps/energy/distance/…) at
      // day-or-coarser buckets: per day, the phone's canonical daily aggregate
      // where one was uploaded, else the raw rollup — so a day the phone has not
      // aggregated yet (today, or anything past its watermark) still shows.
      // metric_daily is daily-grain, so the intraday (Day) view falls through
      // to raw samples below, and so does a type it does not cover (no daily
      // sum or average series configured): it has no rows there, and asking
      // costs about a millisecond (009_metric_daily.sql takes the type filter).
      if (dailyUsable && bucketMs >= DAY_MS) {
        const params: unknown[] = [bucket, identifier, from, userId, timeZone];
        const rows = await q<{ t: string; value: number }>(
          `SELECT (extract(epoch from time_bucket($1::interval, day::timestamp AT TIME ZONE $5::text, $5::text)) * 1000)::bigint AS t,
                  ${agg === "sum" ? "sum(value)" : "avg(value)"}::float8 AS value
             FROM metric_daily
            WHERE identifier = $2
              AND day >= (time_bucket($1::interval, $3::timestamptz, $5::text) AT TIME ZONE $5::text)::date
              AND user_id = $4::uuid
            GROUP BY 1 ORDER BY 1`,
          params,
        );
        if (rows.length) {
          const points: SeriesPoint[] = rows.map((r) => ({
            t: Number(r.t),
            value: Number(r.value) || 0,
            min: null,
            max: null,
            count: 0,
          }));
          return { identifier, unit: type?.unit ?? null, agg, bucketMs, points };
        }
      }

      // Raw cumulative samples often overlap across iPhone and Watch. Establish
      // truth at the requested intraday grain, or at local-day grain for longer
      // charts, by choosing the highest source total. Only then roll those truth
      // values into the requested bucket, so a week can use a different winning
      // source on each day.
      const truthBucket = bucketMs < DAY_MS ? bucket : "1 day";
      let rows: SeriesRow[];
      if (agg === "sum") {
        const params: unknown[] = [bucket, truthBucket, identifier, from, userId, timeZone];
        rows = await q<SeriesRow>(
          `WITH per_source AS (
             SELECT time_bucket($2::interval, q.start_ts, $6::text) AS truth_bucket,
                    q.source_id,
                    sum(q.value)::float8 AS value
               FROM quantity_samples q
              WHERE q.type_id = (SELECT type_id FROM sample_types WHERE identifier = $3)
                AND q.start_ts >= time_bucket($1::interval, $4::timestamptz, $6::text)
                AND q.user_id = $5::uuid
              GROUP BY 1, q.source_id
           ), truth AS (
             SELECT truth_bucket, max(value)::float8 AS value
               FROM per_source
              GROUP BY truth_bucket
           )
           SELECT (extract(epoch from time_bucket($1::interval, truth_bucket, $6::text)) * 1000)::bigint AS t,
                  sum(value)::float8 AS sum,
                  NULL::float8 AS avg,
                  NULL::float8 AS min,
                  NULL::float8 AS max,
                  count(*)::int AS n
             FROM truth
            GROUP BY time_bucket($1::interval, truth_bucket, $6::text)
            ORDER BY time_bucket($1::interval, truth_bucket, $6::text)`,
          params,
        );
      } else {
        const params: unknown[] = [bucket, identifier, from, userId, timeZone];
        rows = await q<SeriesRow>(
          `SELECT (extract(epoch from time_bucket($1::interval, q.start_ts, $5::text)) * 1000)::bigint AS t,
                  sum(q.value)::float8 AS sum,
                  avg(q.value)::float8 AS avg,
                  min(q.value)::float8 AS min,
                  max(q.value)::float8 AS max,
                  count(*)::int AS n
             FROM quantity_samples q
            WHERE q.type_id = (SELECT type_id FROM sample_types WHERE identifier = $2)
              AND q.start_ts >= time_bucket($1::interval, $3::timestamptz, $5::text)
              AND q.user_id = $4::uuid
            GROUP BY 1 ORDER BY 1`,
          params,
        );
      }
      const points: SeriesPoint[] = rows.map((r) => ({
        t: Number(r.t),
        value: Number(agg === "sum" ? r.sum : r.avg) || 0,
        min: r.min == null ? null : Number(r.min),
        max: r.max == null ? null : Number(r.max),
        count: Number(r.n),
      }));
      return { identifier, unit: type?.unit ?? null, agg, bucketMs, points };
    });
  });
}

// ── latest reading per type ──────────────────────────────────────────────
// Through a security-barrier view Postgres will not read a hypertable newest
// chunk first for ORDER BY … LIMIT 1, so an open-ended probe sorts every
// sample of the type (1.2 M heart-rate readings: 0.1–0.4 s). A probe bounded
// to the last fortnight touches only the newest chunks; a type with nothing
// that recent is sparse, and gets the open-ended probe cheaply.
const LATEST_RECENT_DAYS = 14;

export async function getLatestMany(userId: string, identifiers: string[]): Promise<Map<string, Latest>> {
  const out = new Map<string, Latest>();
  if (!identifiers.length) return out;
  return liveRead("getLatestMany", async () => {
    // One probe per type: the type reaches the scan as an outer reference of
    // the LATERAL, which a security-barrier view accepts (a DISTINCT ON over
    // a join read every sample there is).
    const latest = (q: QueryFn, ids: string[], recentDays: number | null) =>
      q<{ identifier: string; value: number; t: string }>(
        `SELECT st.identifier, l.value, l.t
           FROM sample_types st
          CROSS JOIN LATERAL (
            SELECT q.value::float8 AS value,
                   (extract(epoch from q.start_ts) * 1000)::bigint AS t
              FROM quantity_samples q
             WHERE q.type_id = st.type_id
               AND q.user_id = $2::uuid
               ${recentDays == null ? "" : "AND q.start_ts >= now() - make_interval(days => $3::int)"}
             ORDER BY q.start_ts DESC
             LIMIT 1
          ) l
          WHERE st.identifier = ANY($1::text[])`,
        recentDays == null ? [ids, userId] : [ids, userId, recentDays],
      );
    const rows = await scoped(userId, async (q) => {
      const recent = await latest(q, identifiers, LATEST_RECENT_DAYS);
      const found = new Set(recent.map((r) => r.identifier));
      const older = identifiers.filter((id) => !found.has(id));
      return older.length ? [...recent, ...(await latest(q, older, null))] : recent;
    });
    for (const r of rows) {
      out.set(r.identifier, {
        identifier: r.identifier,
        value: Number(r.value),
        unit: typeByIdentifier(r.identifier)?.unit ?? null,
        t: Number(r.t),
      });
    }
    return out;
  });
}

// ── today's cumulative totals (for activity rings / summary) ──────────────
export async function getTodayTotals(userId: string, identifiers: string[]): Promise<Map<string, number>> {
  const out = new Map<string, number>();
  if (!identifiers.length) return out;
  return liveRead("getTodayTotals", async () => {
    for (const id of identifiers) out.set(id, 0);
    const timeZone = await reportingTimeZone(userId);

    // Read Today directly from raw local-day samples so the live headline does
    // not depend on aggregate refresh or bucket-settlement timing. Choose one
    // source per type to avoid overlapping Watch/phone totals.
    //
    // The bounds are local midnights as timestamptz: `date AT TIME ZONE`
    // alone yields a `timestamp`, which the comparison reads in the session's
    // zone (UTC on the server), so Today ran 10:00 to 10:00 in California.
    const rows = await scoped(userId, (q) => q<{ identifier: string; total: number }>(
      `WITH per_source AS (
         SELECT q.type_id, q.source_id, sum(q.value)::float8 AS total
           FROM quantity_samples q
          WHERE q.type_id = ANY (ARRAY(SELECT type_id FROM sample_types WHERE identifier = ANY($1::text[])))
            AND q.user_id = $2::uuid
            AND q.start_ts >= ((now() AT TIME ZONE $3::text)::date::timestamp AT TIME ZONE $3::text)
            AND q.start_ts < (((now() AT TIME ZONE $3::text)::date + 1)::timestamp AT TIME ZONE $3::text)
          GROUP BY q.type_id, q.source_id
       )
       SELECT st.identifier, max(p.total)::float8 AS total
         FROM per_source p
         JOIN sample_types st ON st.type_id = p.type_id
        GROUP BY st.identifier`,
      [identifiers, userId, timeZone],
    ));
    for (const r of rows) out.set(r.identifier, Number(r.total));
    return out;
  });
}

// ── batched daily sparklines for a set of quantity types (one round-trip) ──
export async function getDailySparklines(userId: string, identifiers: string[], days = 21): Promise<Map<string, number[]>> {
  const out = new Map<string, number[]>();
  if (!identifiers.length) return out;
  return liveRead("getDailySparklines", async () => {
    const byId = new Map<string, number[]>();
    // Decided before the transaction (it reads through query()).
    const dailyUsable = await metricDailyUsable(userId);
    const timeZone = await reportingTimeZone(userId);
    return await scoped(userId, async (q) => {

      // Covered types: daily best-guess-of-truth. A type metric_daily has no
      // rows for (not covered, or nothing in the window) reads raw samples.
      if (dailyUsable) {
        const rows = await q<{ identifier: string; value: number }>(
          `SELECT identifier,
                  (extract(epoch from (day::timestamp AT TIME ZONE $4::text)) * 1000)::bigint AS t,
                  value::float8 AS value
             FROM metric_daily
            WHERE identifier = ANY($1::text[])
              AND user_id = $3::uuid
              AND day >= (now() AT TIME ZONE $4::text)::date - $2::int
            ORDER BY identifier, day`,
          [identifiers, days, userId, timeZone],
        );
        for (const r of rows) {
          const arr = byId.get(r.identifier) ?? [];
          arr.push(Number(r.value) || 0);
          byId.set(r.identifier, arr);
        }
      }
      const rawIds = identifiers.filter((id) => !byId.has(id));
      const rawCumIds = rawIds.filter((id) => defaultAgg(id) === "sum");
      const rawDiscIds = rawIds.filter((id) => defaultAgg(id) === "avg");

      // Cumulative raw data: one source per local day to avoid Watch + phone
      // double counts. Discrete readings remain a cross-source average.
      if (rawCumIds.length) {
        const rows = await q<{ identifier: string; value: number }>(
          `WITH per_source AS (
             SELECT q.type_id,
                    time_bucket('1 day', q.start_ts, $4::text) AS day,
                    q.source_id,
                    sum(q.value)::float8 AS value
               FROM quantity_samples q
              WHERE q.type_id = ANY (ARRAY(SELECT type_id FROM sample_types WHERE identifier = ANY($1::text[])))
                AND q.user_id = $3::uuid
                AND q.start_ts >= (((now() AT TIME ZONE $4::text)::date - $2::int)::timestamp AT TIME ZONE $4::text)
              GROUP BY q.type_id, day, q.source_id
           )
           SELECT st.identifier, max(p.value)::float8 AS value
             FROM per_source p
             JOIN sample_types st ON st.type_id = p.type_id
            GROUP BY st.identifier, p.day
            ORDER BY st.identifier, p.day`,
          [rawCumIds, days, userId, timeZone],
        );
        for (const r of rows) {
          const arr = byId.get(r.identifier) ?? [];
          arr.push(Number(r.value) || 0);
          byId.set(r.identifier, arr);
        }
      }

      if (rawDiscIds.length) {
        const rows = await q<{ identifier: string; value: number }>(
          `SELECT st.identifier, d.value
             FROM (
               SELECT q.type_id, time_bucket('1 day', q.start_ts, $4::text) AS day, avg(q.value)::float8 AS value
                 FROM quantity_samples q
                WHERE q.type_id = ANY (ARRAY(SELECT type_id FROM sample_types WHERE identifier = ANY($1::text[])))
                  AND q.user_id = $3::uuid
                  AND q.start_ts >= (((now() AT TIME ZONE $4::text)::date - $2::int)::timestamp AT TIME ZONE $4::text)
                GROUP BY q.type_id, day
             ) d
             JOIN sample_types st ON st.type_id = d.type_id
            ORDER BY st.identifier, d.day`,
          [rawDiscIds, days, userId, timeZone],
        );
        for (const r of rows) {
          const arr = byId.get(r.identifier) ?? [];
          arr.push(Number(r.value) || 0);
          byId.set(r.identifier, arr);
        }
      }

      for (const id of identifiers) out.set(id, byId.get(id) ?? []);
      return out;
    });
  });
}
