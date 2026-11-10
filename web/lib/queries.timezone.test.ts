import { beforeEach, describe, expect, it, vi } from "vitest";

const queryMock = vi.hoisted(() => vi.fn());
// scoped() runs the callback's statements through the same mock; its
// transaction and user setting are lib/db.ts's business, tested there.
vi.mock("./db", () => ({
  query: queryMock,
  healthQuery: (sql: string, params?: unknown[]) => sql.includes("has_table_privilege")
    ? Promise.resolve([{ role: "web_app", direct: false }]) : queryMock(sql, params),
  scoped: (_userId: string, fn: (q: typeof queryMock) => Promise<unknown>) => fn(queryMock),
}));

const USER_ID = "11111111-1111-4111-8111-111111111111";
const STEPS = "HKQuantityTypeIdentifierStepCount";

let warnSpy: ReturnType<typeof vi.spyOn>;

// The database-zone lookup is cached at module scope, so every test imports a
// fresh copy of ./queries.
beforeEach(() => {
  vi.resetModules();
  process.env.DATABASE_URL = "postgres://test";
  process.env.PULS_TIME_ZONE = "Europe/Berlin";
  queryMock.mockReset();
  warnSpy?.mockRestore();
  warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});
});

// metricDailyRows: what metric_daily holds for the window (a covered type).
function mockDatabase(zoneLookup: () => Promise<unknown[]>, metricDailyRows: unknown[] = [{ identifier: STEPS, t: "0", value: 1 }]) {
  queryMock.mockImplementation((text: string) => {
    if (text.includes("puls_time_zone()")) return zoneLookup();
    if (text.includes("FROM metric_daily")) return Promise.resolve(metricDailyRows);
    return Promise.resolve([]);
  });
}

const sqlCalls = () => queryMock.mock.calls.map(([sql]) => sql as string);
const touchedMetricDaily = () => sqlCalls().some((sql) => sql.includes("metric_daily"));
const usedMetricDailySeries = () =>
  sqlCalls().some((sql) => sql.includes("FROM metric_daily") && sql.includes("GROUP BY 1 ORDER BY 1"));
const usedRawBuckets = () => sqlCalls().some((sql) => sql.includes("WITH per_source"));
const zoneLookups = () => sqlCalls().filter((sql) => sql.includes("puls_time_zone()")).length;

describe("metric_daily zone guard", () => {
  it("uses metric_daily when the database's zone equals PULS_TIME_ZONE", async () => {
    mockDatabase(() => Promise.resolve([{ zone: "Europe/Berlin" }]));
    const { getSeries } = await import("./queries");
    await getSeries(USER_ID, STEPS, "Y");

    expect(usedMetricDailySeries()).toBe(true);
    expect(usedRawBuckets()).toBe(false);
    expect(warnSpy).not.toHaveBeenCalled();
  });

  it("reads raw samples for a type metric_daily has no rows for", async () => {
    // Not covered (no daily sum or average series), or nothing in the window
    // yet: the same answer raw samples give, so the chart asks them.
    mockDatabase(() => Promise.resolve([{ zone: "Europe/Berlin" }]), []);
    const { getDailySparklines, getSeries } = await import("./queries");
    await getSeries(USER_ID, STEPS, "Y");
    await getDailySparklines(USER_ID, [STEPS]);

    expect(usedMetricDailySeries()).toBe(true);
    expect(usedRawBuckets()).toBe(true);
    expect(sqlCalls().filter((sql) => sql.includes("FROM quantity_samples"))).toHaveLength(2);
  });

  it("falls back to raw local buckets and warns once on a zone mismatch", async () => {
    mockDatabase(() => Promise.resolve([{ zone: "UTC" }]));
    const { getDailySparklines, getSeries } = await import("./queries");
    await getSeries(USER_ID, STEPS, "Y");
    await getDailySparklines(USER_ID, [STEPS]);

    expect(touchedMetricDaily()).toBe(false);
    expect(usedRawBuckets()).toBe(true);
    expect(warnSpy).toHaveBeenCalledTimes(1);
    expect(String(warnSpy.mock.calls[0][0])).toContain("Europe/Berlin");
    expect(String(warnSpy.mock.calls[0][0])).toContain("UTC");
  });

  it("falls back and warns once when puls_time_zone() does not exist", async () => {
    mockDatabase(() => Promise.reject(new Error("function puls_time_zone() does not exist")));
    const { getSeries } = await import("./queries");
    await getSeries(USER_ID, STEPS, "Y");
    await getSeries(USER_ID, STEPS, "30D");

    expect(touchedMetricDaily()).toBe(false);
    expect(usedRawBuckets()).toBe(true);
    expect(warnSpy).toHaveBeenCalledTimes(1);
  });

  it("looks the database zone up once and reuses it", async () => {
    mockDatabase(() => Promise.resolve([{ zone: "Europe/Berlin" }]));
    const { getDailySparklines, getSeries } = await import("./queries");
    await Promise.all([getSeries(USER_ID, STEPS, "Y"), getSeries(USER_ID, STEPS, "30D")]);
    await getDailySparklines(USER_ID, [STEPS]);

    expect(zoneLookups()).toBe(1);
    expect(usedMetricDailySeries()).toBe(true);
  });
});
