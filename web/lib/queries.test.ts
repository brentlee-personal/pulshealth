import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

const queryMock = vi.hoisted(() => vi.fn());
// Every statement a scoped() callback issues, with the user it was scoped to.
const scopedStatements = vi.hoisted(() => [] as { userId: string; sql: string }[]);
// query() or scoped() called while a scoped() callback is running: each would
// take a second pooled connection while the first is held (see lib/db.ts).
const nested = vi.hoisted(() => ({ depth: 0, calls: [] as string[] }));
vi.mock("./db", () => ({
  healthQuery: (sql: string, params?: unknown[]) => sql.includes("has_table_privilege")
    ? Promise.resolve([{ role: "web_app", direct: false }]) : queryMock(sql, params),
  query: (sql: string, params?: unknown[]) => {
    if (nested.depth > 0) nested.calls.push(sql);
    return queryMock(sql, params);
  },
  scoped: async (userId: string, fn: (q: (sql: string, params?: unknown[]) => unknown) => Promise<unknown>) => {
    if (nested.depth > 0) nested.calls.push(`scoped(${userId})`);
    nested.depth++;
    try {
      return await fn((sql: string, params?: unknown[]) => {
        scopedStatements.push({ userId, sql });
        return queryMock(sql, params);
      });
    } finally {
      nested.depth--;
    }
  },
}));

// Passed to every query explicitly: the user is an argument, never read from
// the environment or a cookie inside lib/queries.ts.
const USER_ID = "11111111-1111-4111-8111-111111111111";

beforeAll(() => {
  process.env.DATABASE_URL = "postgres://test";
  process.env.PULS_TIME_ZONE = "America/Los_Angeles";
});

beforeEach(() => {
  queryMock.mockReset();
  scopedStatements.length = 0;
  nested.calls.length = 0;
  queryMock.mockImplementation((text: string) => {
    // Same zone as PULS_TIME_ZONE above, so the metric_daily gate stays open
    // (its dedicated coverage lives in queries.timezone.test.ts).
    if (text.includes("puls_time_zone()")) {
      return Promise.resolve([{ zone: "America/Los_Angeles" }]);
    }
    if (text.includes("SELECT count(*)::bigint AS rows")) {
      return Promise.resolve([{ rows: "0", earliest: null, latest: null }]);
    }
    if (text.includes("FROM workouts w")) {
      return Promise.resolve([{
        uuid: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
        activity_type: "running",
        start: "0",
        end: "1",
        duration_s: 1,
        energy_kcal: null,
        distance_m: null,
        stats: {},
        stats_detail: {},
        events: [],
        activities: [],
        metadata: {},
        source: null,
      }]);
    }
    return Promise.resolve([]);
  });
});

describe("query semantics", () => {
  it("maps category types to duration and stood-only semantics", async () => {
    const { categoryAggregation } = await import("./queries");
    expect(categoryAggregation("HKCategoryTypeIdentifierSleepAnalysis")).toEqual({
      mode: "duration", unit: "h", values: [1, 3, 4, 5], dayOffsetHours: 6,
    });
    expect(categoryAggregation("HKCategoryTypeIdentifierMindfulSession")).toEqual({
      mode: "duration", unit: "min",
    });
    expect(categoryAggregation("HKCategoryTypeIdentifierAppleStandHour")).toEqual({
      mode: "count", unit: "h", values: [0],
    });
    expect(categoryAggregation("event")).toEqual({ mode: "count", unit: "count" });
  });

  it("preserves profile fields when date of birth is absent", async () => {
    const { DEFAULT_MAX_HR, profileFromStoredValues } = await import("./queries");
    expect(profileFromStoredValues(null, "female", 58)).toEqual({
      dob: null,
      biologicalSex: "female",
      age: null,
      maxHr: DEFAULT_MAX_HR,
      restingHr: 58,
    });
  });

  it("computes Today from raw local-day source totals", async () => {
    const { getTodayTotals } = await import("./queries");
    await getTodayTotals(USER_ID, ["HKQuantityTypeIdentifierStepCount"]);

    const calls = queryMock.mock.calls.filter(([sql]) => sql !== "SELECT 1");
    expect(calls.some(([sql]) => sql.includes("metric_daily"))).toBe(false);
    const [sql, params] = calls.find(([text]) => text.includes("FROM quantity_samples")) ?? [];
    expect(sql).toContain("max(p.total)");
    // Local midnights as timestamptz, not the `timestamp` that
    // `date AT TIME ZONE` gives (read in the session's zone, UTC).
    expect(sql).toContain("((now() AT TIME ZONE $3::text)::date::timestamp AT TIME ZONE $3::text)");
    expect(sql).toContain("(((now() AT TIME ZONE $3::text)::date + 1)::timestamp AT TIME ZONE $3::text)");
    expect(params).toEqual([
      ["HKQuantityTypeIdentifierStepCount"], USER_ID, "America/Los_Angeles",
    ]);
  });

  it("deduplicates cumulative sources before coarser chart rollups", async () => {
    const { getSeries } = await import("./queries");
    await getSeries(USER_ID, "HKQuantityTypeIdentifierStepCount", "D");
    await getSeries(USER_ID, "HKQuantityTypeIdentifierStepCount", "Y");

    const calls = queryMock.mock.calls.filter(([sql]) =>
      sql.includes("WITH per_source") && sql.includes("truth AS"),
    );
    expect(calls).toHaveLength(2);
    const [intradaySql, intradayParams] = calls[0];
    expect(intradayParams.slice(0, 2)).toEqual(["1 hour", "1 hour"]);
    expect(intradayParams[4]).toBe(USER_ID);
    expect(intradaySql).toContain("GROUP BY truth_bucket");
    expect(intradaySql).toContain("sum(value)::float8 AS sum");

    const [yearSql, yearParams] = calls[1];
    expect(yearParams.slice(0, 2)).toEqual(["1 week", "1 day"]);
    expect(yearSql).toContain("time_bucket($2::interval");
    expect(yearSql).toContain("time_bucket($1::interval");
  });

  it("reads charts from metric_daily or raw samples, never aggregate_samples directly", async () => {
    // metric_daily already prefers the phone's daily aggregate day by day and
    // falls back to raw rollups; reading aggregate_samples directly lost today
    // and charted null buckets as zero.
    const { getSeries } = await import("./queries");
    await getSeries(USER_ID, "HKQuantityTypeIdentifierStepCount", "30D");
    await getSeries(USER_ID, "HKQuantityTypeIdentifierHeartRate", "D");
    const sql = queryMock.mock.calls.map(([text]) => text as string);
    expect(sql.some((text) => /aggregate_(series|samples)/.test(text))).toBe(false);
  });

  it("aligns chart windows to the bucket grain in the viewer's zone", async () => {
    const { getSeries } = await import("./queries");
    await getSeries(USER_ID, "HKQuantityTypeIdentifierHeartRate", "7D");
    await getSeries(USER_ID, "HKQuantityTypeIdentifierStepCount", "Y");
    await getSeries(USER_ID, "HKCategoryTypeIdentifierAppleStandHour", "30D");

    const windows = queryMock.mock.calls
      .map(([sql, params]) => ({ sql: sql as string, params }))
      .filter(({ sql }) => /FROM (quantity|category)_samples/.test(sql));

    expect(windows).toHaveLength(3);

    for (const { sql } of windows) {
      // Never a bare `start_ts >= $n`: that made the first day/week bucket a
      // partial slice from "now − span" to the next boundary.
      expect(sql, sql).not.toMatch(/start_ts >= \$\d\b/);
      expect(sql, sql).toMatch(/start_ts >= time_bucket\(\$1::interval, \$\d::timestamptz, \$\d::text\)/);
    }

    expect(windows[0].params).toContain("1 day");
    expect(windows[1].params).toContain("1 week");
    expect(windows[2].params).toContain("1 day");
  });

  it("starts All Time at the type's earliest sample", async () => {
    const { getSeries } = await import("./queries");
    const user = "33333333-3333-4333-8333-333333333333"; // own stats cache entry
    const earliest = Date.now() - 200 * 86_400_000;
    queryMock.mockImplementation((text: string) => {
      if (text.includes("SELECT st.identifier,")) {
        return Promise.resolve([{
          identifier: "HKQuantityTypeIdentifierHeartRate", rows: "10", earliest: String(earliest), latest: String(Date.now()),
        }]);
      }
      return Promise.resolve([]);
    });
    const series = await getSeries(user, "HKQuantityTypeIdentifierHeartRate", "ALL");
    const [, params] = queryMock.mock.calls.find(([sql]) => /time_bucket[\s\S]*FROM quantity_samples/.test(sql)) ?? [];
    expect(params?.[0]).toBe("1 week");
    expect(params?.[2]).toEqual(new Date(earliest));
    expect(series.bucketMs).toBe(7 * 86_400_000);

    // A type with no samples has no All Time window, and no chart query runs.
    queryMock.mockClear();
    const none = await getSeries(user, "HKQuantityTypeIdentifierBodyMass", "ALL");
    expect(none.points).toEqual([]);
    expect(queryMock.mock.calls.some(([sql]) => /time_bucket/.test(sql))).toBe(false);
  });

  it("binds the time zone wherever a query expects one", async () => {
    // Every `$n::text` a query hands to time_bucket or AT TIME ZONE must be
    // the zone: a renumbered parameter list that left a SELECT list behind
    // once bucketed by the user id, which Postgres rejected as a time zone
    // ("time zone \"<uuid>\" not recognized").
    const { getSeries } = await import("./queries");
    for (const id of [
      "HKCategoryTypeIdentifierSleepAnalysis",
      "HKCategoryTypeIdentifierMindfulSession",
      "HKCategoryTypeIdentifierAppleStandHour",
      "HKQuantityTypeIdentifierStepCount",
      "HKQuantityTypeIdentifierHeartRate",
    ]) {
      await getSeries(USER_ID, id, "30D");
      await getSeries(USER_ID, id, "D");
      await getSeries(USER_ID, id, "ALL");
    }
    const charts = queryMock.mock.calls.filter(([sql]) => /time_bucket/.test(sql as string));
    expect(charts.length).toBeGreaterThanOrEqual(10);
    for (const [sql, params] of charts as [string, unknown[]][]) {
      const zones = [
        ...sql.matchAll(/time_bucket\([^;]*?, *\$(\d+)::text\)/g),
        ...sql.matchAll(/AT TIME ZONE \$(\d+)::text/g),
      ].map((m) => Number(m[1]));
      expect(zones.length, sql).toBeGreaterThan(0);
      for (const n of zones) expect(params[n - 1], `$${n} in ${sql}`).toBe("America/Los_Angeles");
    }
  });

  it("attributes sleep to the wake day and dedups duration sources", async () => {
    const { getSeries } = await import("./queries");
    await getSeries(USER_ID, "HKCategoryTypeIdentifierSleepAnalysis", "30D");
    await getSeries(USER_ID, "HKCategoryTypeIdentifierMindfulSession", "30D");

    const [sleep, mindful] = queryMock.mock.calls
      .map(([sql]) => sql as string)
      .filter((sql) => sql.includes("FROM category_samples"));
    // 6 PM boundary: samples are shifted forward before day-bucketing, and the
    // window is widened by the same amount so the first night is whole.
    expect(sleep).toContain("c.start_ts + interval '6 hours'");
    expect(sleep).toContain("$5::text) - interval '6 hours'");
    expect(sleep).toContain("GROUP BY 1, c.source_id");
    expect(sleep).toContain("max(value)::float8 AS value");
    // Other durations are not shifted.
    expect(mindful).not.toContain("interval '6 hours'");
    expect(mindful).toContain("max(value)::float8 AS value");
  });

  it("scopes all health-data SQL and uses local Today boundaries", async () => {
    const queries = await import("./queries");
    await queries.getSeries(USER_ID, "HKQuantityTypeIdentifierStepCount", "D");
    await queries.getSeries(USER_ID, "HKCategoryTypeIdentifierAppleStandHour", "30D");
    await queries.getLatestMany(USER_ID, ["HKQuantityTypeIdentifierHeartRate"]);
    await queries.getTodayTotals(USER_ID, ["HKQuantityTypeIdentifierStepCount"]);
    await queries.getActivityRings(USER_ID);
    await queries.getStats(USER_ID);
    await queries.getDailySparklines(USER_ID, ["HKQuantityTypeIdentifierStepCount"]);
    await queries.getWorkouts(USER_ID, 3);
    await queries.getWorkoutDetail(USER_ID, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa");
    await queries.getWorkoutSeries(USER_ID, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa");
    await queries.getProfile(USER_ID);

    const healthCalls = queryMock.mock.calls.filter(([sql]) =>
      /(metric_daily|quantity_samples|category_samples|activity_summaries|workouts|workout_route_points|workout_series_points|FROM users)/.test(sql),
    );
    expect(healthCalls.length).toBeGreaterThan(0);
    for (const [sql, params] of healthCalls) {
      expect(sql, sql).toMatch(/user_id|u\.id =/);
      expect(params, sql).toContain(USER_ID);
      // …and every one of them inside a transaction scoped to that user, so
      // in accounts mode the database filters it too (lib/db.ts scoped()).
      expect(scopedStatements.filter((s) => s.sql === sql).map((s) => s.userId), sql).toContain(USER_ID);
    }
    expect(new Set(scopedStatements.map((s) => s.userId))).toEqual(new Set([USER_ID]));

    const activitySql = healthCalls.find(([sql]) => sql.includes("FROM activity_summaries"))?.[0];
    expect(activitySql).toContain("date = (now() AT TIME ZONE $2::text)::date");

    const cumulativeSql = healthCalls.find(([sql]) => sql.includes("WITH per_source") && sql.includes("truth AS"))?.[0];
    expect(cumulativeSql).toBeTruthy();
  });

  it("filters the sample tables only in ways a security-barrier view takes in", async () => {
    // As web_app every sample table is a security-barrier view: a join
    // condition, or a timestamptz compared with a `timestamp`, stays outside
    // it, and the read scans every chunk (seconds instead of milliseconds).
    const queries = await import("./queries");
    for (const range of ["D", "30D", "Y"] as const) {
      await queries.getSeries(USER_ID, "HKQuantityTypeIdentifierStepCount", range);
      await queries.getSeries(USER_ID, "HKQuantityTypeIdentifierHeartRate", range);
      await queries.getSeries(USER_ID, "HKCategoryTypeIdentifierSleepAnalysis", range);
      await queries.getSeries(USER_ID, "HKCategoryTypeIdentifierAppleStandHour", range);
    }
    await queries.getLatestMany(USER_ID, ["HKQuantityTypeIdentifierHeartRate"]);
    await queries.getTodayTotals(USER_ID, ["HKQuantityTypeIdentifierStepCount"]);
    await queries.getDailySparklines(USER_ID, ["HKQuantityTypeIdentifierStepCount", "HKQuantityTypeIdentifierHeartRate"]);
    vi.resetModules();
    await (await import("./queries")).getStats(USER_ID);

    const sampleSql = scopedStatements.map((s) => s.sql).filter((sql) => /(quantity|category)_samples/.test(sql));
    expect(sampleSql.length).toBeGreaterThan(10);
    for (const sql of sampleSql) {
      expect(sql, sql).not.toMatch(/JOIN sample_types \w+ ON \w+\.type_id = [qc]\.type_id/);
      // `date AT TIME ZONE` (also after `+ 1` or `- n`) is a `timestamp`.
      expect(sql, sql).not.toMatch(/::date(\s*[-+]\s*[$\w:]+\))?\s+AT TIME ZONE/);
      expect(sql, sql).not.toContain("UNION ALL");
    }
  });

  it("never opens a second connection from inside a scoped transaction", async () => {
    // All Time pulls the per-user stats and every quantity chart consults the
    // database zone: both must happen before the chart's own transaction.
    // A fresh module per call, so the zone and stats caches are cold and each
    // function has to do its own lookups. Every data function lib/data/
    // exports must be listed here (checked below), so a new one cannot skip it.
    type Queries = typeof import("./queries");
    const WORKOUT = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
    const calls: [string, string, (queries: Queries) => Promise<unknown>][] = [];
    for (const range of ["D", "30D", "ALL"] as const) {
      calls.push(["getSeries", `steps ${range}`, (x) => x.getSeries(USER_ID, "HKQuantityTypeIdentifierStepCount", range)]);
      calls.push(["getSeries", `sleep ${range}`, (x) => x.getSeries(USER_ID, "HKCategoryTypeIdentifierSleepAnalysis", range)]);
    }
    calls.push(["getDailySparklines", "sparklines", (x) => x.getDailySparklines(USER_ID, ["HKQuantityTypeIdentifierStepCount", "HKQuantityTypeIdentifierHeartRate"])]);
    calls.push(["getLatestMany", "latest", (x) => x.getLatestMany(USER_ID, ["HKQuantityTypeIdentifierHeartRate"])]);
    calls.push(["getTodayTotals", "today", (x) => x.getTodayTotals(USER_ID, ["HKQuantityTypeIdentifierStepCount"])]);
    calls.push(["getActivityRings", "rings", (x) => x.getActivityRings(USER_ID)]);
    calls.push(["getStats", "stats", (x) => x.getStats(USER_ID)]);
    calls.push(["getWorkouts", "workouts", (x) => x.getWorkouts(USER_ID, 3)]);
    calls.push(["getWorkoutDetail", "workout", (x) => x.getWorkoutDetail(USER_ID, WORKOUT)]);
    calls.push(["getWorkoutSeries", "workout series", (x) => x.getWorkoutSeries(USER_ID, WORKOUT)]);
    calls.push(["getUser", "user", (x) => x.getUser(USER_ID)]);
    calls.push(["getSleepSessions", "sleep all time", (x) => x.getSleepSessions(USER_ID, "ALL")]);
    calls.push(["getSleepSessions", "sleep sessions", (x) => x.getSleepSessions(USER_ID)]);
    calls.push(["getProfile", "profile", (x) => x.getProfile(USER_ID)]);
    // The source probe carries no personal health values.
    calls.push(["getDataSource", "source", (x) => x.getDataSource()]);
    const UNSCOPED = new Set(["getDataSource"]);
    for (const [fn, name, call] of calls) {
      vi.resetModules();
      nested.calls.length = 0;
      scopedStatements.length = 0;
      await call(await import("./queries"));
      if (!UNSCOPED.has(fn)) expect(scopedStatements.length, name).toBeGreaterThan(0);
      expect(nested.calls, name).toEqual([]);
    }

    // Every read in lib/data/, found by reading the files, is both exported
    // through ./queries and exercised above.
    const dir = path.join(path.dirname(fileURLToPath(import.meta.url)), "data");
    const scopedReads = new Set<string>();
    for (const file of readdirSync(dir).filter((f) => f.endsWith(".ts") && !f.endsWith(".test.ts"))) {
      const text = readFileSync(path.join(dir, file), "utf8");
      for (const m of text.matchAll(/^export (?:async function|const) (get\w+)/gm)) scopedReads.add(m[1]);
    }
    expect(scopedReads.size).toBeGreaterThan(5);
    const exported = await import("./queries");
    const covered = new Set(calls.map(([fn]) => fn));
    for (const fn of scopedReads) {
      expect(typeof (exported as Record<string, unknown>)[fn], `${fn} is not exported from ./queries`).toBe("function");
      expect(covered.has(fn), `${fn} is missing from the nested-connection check`).toBe(true);
    }
  });

  it("keeps the per-user caches apart when users alternate", async () => {
    const OTHER = "22222222-2222-4222-8222-222222222222";
    // The caches are module-scoped and earlier tests already warmed USER_ID's.
    vi.resetModules();
    const { getStats } = await import("./queries");
    const first = await getStats(USER_ID);
    const second = await getStats(OTHER);
    const firstAgain = await getStats(USER_ID);

    // Both users' scans ran, each bound to its own id, and the first user's
    // entry survived the second user's — same-instance from the TTL cache.
    const scans = queryMock.mock.calls.filter(([sql]) => sql.includes("FROM quantity_samples") && sql.includes("GROUP BY type_id"));
    expect(scans.map(([, params]) => params[0])).toEqual([USER_ID, OTHER]);
    expect(firstAgain).toBe(first);
    expect(second).not.toBe(first);
  });
});

describe("a failed read", () => {
  it("throws DataUnavailableError on the page that hit it and re-checks the database", async () => {
    vi.stubEnv("NODE_ENV", "production"); // no demo fallback
    vi.resetModules();
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const { DataUnavailableError, getDataSource, getWorkouts, isDataUnavailable } = await import("./queries");
      const probes = () => queryMock.mock.calls.filter(([sql]) => sql === "SELECT 1").length;
      expect((await getDataSource()).source).toBe("live");
      expect((await getDataSource()).source).toBe("live");
      expect(probes()).toBe(1); // cached between reads that succeed

      // One statement fails on its own: the page that ran it says so instead
      // of charting nothing, and the probe still answers, so the viewer
      // stays live for everyone else.
      queryMock.mockImplementation((sql: string) =>
        sql === "SELECT 1" ? Promise.resolve([]) : Promise.reject(new Error("canceling statement due to statement timeout")),
      );
      const failure = await getWorkouts(USER_ID).then(() => null, (e: unknown) => e);
      expect(failure).toBeInstanceOf(DataUnavailableError);
      // The digest is what survives Next.js's production error scrubbing
      // and what app/error.tsx recognises.
      expect(isDataUnavailable(failure)).toBe(true);
      expect(isDataUnavailable({ digest: (failure as { digest: string }).digest })).toBe(true);
      expect(isDataUnavailable(new Error("boom"))).toBe(false);
      expect(isDataUnavailable({ digest: "1234567890" })).toBe(false);
      expect((await getDataSource()).source).toBe("live");
      expect(probes()).toBe(2);

      // The pool times out: so does the probe, and the source says so.
      queryMock.mockRejectedValue(new Error("timeout exceeded when trying to connect"));
      await expect(getWorkouts(USER_ID)).rejects.toBeInstanceOf(DataUnavailableError);
      expect(await getDataSource()).toEqual({ source: "error", detail: "Database unreachable" });
      // …and while it does, every read throws without trying the database.
      queryMock.mockClear();
      await expect(getWorkouts(USER_ID)).rejects.toThrow("Database unreachable");
      expect(queryMock).not.toHaveBeenCalled();
      // A transient outage does not poison every page for the live-cache TTL.
      queryMock.mockResolvedValue([]);
      const clock = vi.spyOn(Date, "now").mockReturnValue(Date.now() + 1001);
      try {
        expect((await getDataSource()).source).toBe("live");
      } finally {
        clock.mockRestore();
      }
    } finally {
      errorSpy.mockRestore();
      vi.unstubAllEnvs();
      vi.resetModules();
    }
  });

  it("passes a nested read's failure through unchanged (All Time's stats)", async () => {
    vi.stubEnv("NODE_ENV", "production");
    vi.resetModules();
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const { DataUnavailableError, getSeries } = await import("./queries");
      queryMock.mockImplementation((sql: string) =>
        sql.includes("GROUP BY type_id") ? Promise.reject(new Error("statement timeout")) : Promise.resolve([]),
      );
      await expect(getSeries(USER_ID, "HKQuantityTypeIdentifierStepCount", "ALL")).rejects.toBeInstanceOf(DataUnavailableError);
      // Logged once, by the stats read that failed, not again by the chart.
      expect(errorSpy.mock.calls.filter(([msg]) => String(msg).includes("failed"))).toHaveLength(1);
    } finally {
      errorSpy.mockRestore();
      vi.unstubAllEnvs();
      vi.resetModules();
    }
  });
});

describe("with no live database", () => {
  type Queries = typeof import("./queries");
  // Every page-level read, with what it must not return in production.
  const reads: [string, (x: Queries) => Promise<unknown>][] = [
    ["getSeries", (x) => x.getSeries(USER_ID, "HKQuantityTypeIdentifierStepCount", "30D")],
    ["getLatestMany", (x) => x.getLatestMany(USER_ID, ["HKQuantityTypeIdentifierHeartRate"])],
    ["getTodayTotals", (x) => x.getTodayTotals(USER_ID, ["HKQuantityTypeIdentifierStepCount"])],
    ["getActivityRings", (x) => x.getActivityRings(USER_ID)],
    ["getStats", (x) => x.getStats(USER_ID)],
    ["getDailySparklines", (x) => x.getDailySparklines(USER_ID, ["HKQuantityTypeIdentifierStepCount"])],
    ["getWorkouts", (x) => x.getWorkouts(USER_ID)],
    ["getWorkoutDetail", (x) => x.getWorkoutDetail(USER_ID, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")],
    ["getWorkoutSeries", (x) => x.getWorkoutSeries(USER_ID, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")],
    ["getUser", (x) => x.getUser(USER_ID)],
    ["getProfile", (x) => x.getProfile(USER_ID)],
  ];

  async function withoutDatabase(nodeEnv: string, fn: (x: Queries) => Promise<void>) {
    vi.stubEnv("NODE_ENV", nodeEnv);
    vi.stubEnv("DATABASE_URL", "");
    vi.resetModules();
    try {
      await fn(await import("./queries"));
    } finally {
      vi.unstubAllEnvs();
      vi.resetModules();
    }
  }

  it("throws the error source in production, never demo data or an empty chart", async () => {
    await withoutDatabase("production", async (x) => {
      expect(await x.getDataSource()).toEqual({ source: "error", detail: "Personal storage is not configured" });
      for (const [name, read] of reads) {
        const failure = await read(x).then(() => null, (e: unknown) => e);
        expect(x.isDataUnavailable(failure), name).toBe(true);
      }
      expect(queryMock).not.toHaveBeenCalled();
    });
  });

  it("keeps development reads unavailable without storage", async () => {
    await withoutDatabase("development", async (x) => {
      expect((await x.getDataSource()).source).toBe("error");
      for (const [name, read] of reads) {
        const failure = await read(x).then(() => null, (error: unknown) => error);
        expect(x.isDataUnavailable(failure), name).toBe(true);
      }
      expect(queryMock).not.toHaveBeenCalled();
    });
  });
});
