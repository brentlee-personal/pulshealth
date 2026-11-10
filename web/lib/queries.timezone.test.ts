import { secretFixture } from "./secretFixture";
import { beforeEach, describe, expect, it, vi } from "vitest";

const queryMock = vi.hoisted(() => vi.fn());
const calendar = vi.hoisted(() => vi.fn());
vi.mock("./reportingTimeZone", () => ({ reportingTimeZone: calendar }));
// SQL-math fixtures; actual personal view isolation is tested against PostgreSQL.
vi.mock("./db", () => ({
  query: queryMock,
  healthQuery: async () => [{ role: "web_app", direct: false }],
  scoped: (_userId: string, fn: (q: typeof queryMock) => Promise<unknown>) => fn(queryMock),
}));

const databaseSecret = secretFixture({ DATABASE_URL: "postgres://test" });
const USER_ID = "11111111-1111-4111-8111-111111111111";
const STEPS = "HKQuantityTypeIdentifierStepCount";
beforeEach(() => {
  vi.resetModules();
  delete process.env.DATABASE_URL;
  Object.assign(process.env, databaseSecret);
  process.env.PULS_TIME_ZONE = "UTC";
  calendar.mockReset().mockResolvedValue("Europe/Berlin");
  queryMock.mockReset();
});

function mockDatabase(rows: unknown[] = [{ identifier: STEPS, t: "0", value: 1 }]) {
  queryMock.mockImplementation(async (sql: string) => sql.includes("FROM metric_daily") ? rows : []);
}
const calls = () => queryMock.mock.calls.map(([sql]) => sql as string);
const usedDaily = () => calls().some(sql => sql.includes("FROM metric_daily") && sql.includes("GROUP BY 1 ORDER BY 1"));

describe("personal reporting calendar", () => {
  it("uses personal daily rows even when the global calendar differs", async () => {
    mockDatabase();
    const { getSeries } = await import("./queries");
    await getSeries(USER_ID, STEPS, "Y");
    expect(usedDaily()).toBe(true);
    expect(calls().some(sql => sql.includes("puls_time_zone()"))).toBe(false);
    expect(calendar).toHaveBeenCalledWith(USER_ID);
  });

  it("reads raw personal samples when the daily view has no rows", async () => {
    mockDatabase([]);
    const { getDailySparklines, getSeries } = await import("./queries");
    await getSeries(USER_ID, STEPS, "Y");
    await getDailySparklines(USER_ID, [STEPS]);
    expect(usedDaily()).toBe(true);
    expect(calls().filter(sql => sql.includes("FROM quantity_samples"))).toHaveLength(2);
  });

  it("uses the changed account calendar on the next chart", async () => {
    mockDatabase();
    const { getSeries } = await import("./queries");
    await getSeries(USER_ID, STEPS, "Y");
    calendar.mockResolvedValue("Pacific/Auckland");
    queryMock.mockClear();
    await getSeries(USER_ID, STEPS, "Y");
    const chart = queryMock.mock.calls.find(([sql]) => sql.includes("GROUP BY 1 ORDER BY 1"));
    expect(chart?.[1]).toContain("Pacific/Auckland");
    expect(chart?.[1]).not.toContain("Europe/Berlin");
  });

  it("refuses an unavailable personal calendar without using the global one", async () => {
    mockDatabase();
    calendar.mockRejectedValue(new Error("Reporting time zone unavailable"));
    const { getSeries } = await import("./queries");
    await expect(getSeries(USER_ID, STEPS, "Y")).rejects.toThrow(/unavailable/i);
    expect(usedDaily()).toBe(false);
    expect(calls().some(sql => sql.includes("puls_time_zone()"))).toBe(false);
  });
});
