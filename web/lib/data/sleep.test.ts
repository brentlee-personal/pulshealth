import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
const mocks = vi.hoisted(() => ({ q: vi.fn(), scoped: vi.fn(), stats: vi.fn() }));
vi.mock("./stats", () => ({ getStats: mocks.stats }));
vi.mock("../db", () => ({ scoped: mocks.scoped }));
vi.mock("../reportingTimeZone", () => ({ reportingTimeZone: async () => "America/Los_Angeles" }));
vi.mock("./source", () => ({ liveRead: async (_: string, read: () => unknown) => read() }));
import { getSleepSessions } from "./sleep";
beforeEach(() => {
  vi.clearAllMocks();
  mocks.scoped.mockImplementation(async (_user, read) => read(mocks.q));
  mocks.q.mockResolvedValue([]);
  mocks.stats.mockResolvedValue(new Map([["HKCategoryTypeIdentifierSleepAnalysis", { earliest: Date.parse("2020-01-01") }]]));
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-10-30T18:00:00Z"));
});
afterEach(() => vi.useRealTimers());
describe("sleep read", () => {
  it("scopes the user and bounds both timestamps with a type InitPlan", async () => {
    expect(await getSleepSessions("user-a")).toEqual([]);
    expect(mocks.scoped).toHaveBeenCalledWith("user-a", expect.any(Function));
    const [sql, params] = mocks.q.mock.calls[0];
    expect(sql).toContain("c.user_id = $1::uuid");
    expect(sql).toContain("c.type_id = (SELECT type_id FROM sample_types WHERE identifier = $2)");
    expect(sql).toContain("c.start_ts >= $3::timestamptz");
    expect(sql).toContain("c.start_ts < $4::timestamptz");
    expect(params.slice(0, 2)).toEqual(["user-a", "HKCategoryTypeIdentifierSleepAnalysis"]);
    expect(params[2].toISOString()).toBe("2026-09-29T00:00:00.000Z");
  });
  it("decodes known values and ignores unknown future values", async () => {
    const start = Date.now() - 3600_000;
    mocks.q.mockResolvedValue([1, 99].map((value) => ({ start: String(start), end: String(start + 600_000), value, source: "1", source_name: "Watch" })));
    const result = await getSleepSessions("user-a");
    expect(result[0].sources[0].asleep).toBe(10);
    expect(result[0].sources[0].minutes.unspecified).toBe(10);
  });
  it("queries the selected long range rather than capping all charts at 30 days", async () => {
    await getSleepSessions("user-a", "Y");
    const year = mocks.q.mock.calls[0][1][2];
    expect(year.getTime()).toBeLessThan(Date.now() - 364 * 86400_000);
    await getSleepSessions("user-a", "ALL");
    expect(mocks.stats).toHaveBeenCalledWith("user-a");
    expect(mocks.q.mock.calls[1][1][2].toISOString()).toBe("2019-12-29T00:00:00.000Z");
  });
  it("propagates failed reads instead of returning an empty history", async () => {
    mocks.q.mockRejectedValue(new Error("query failed"));
    await expect(getSleepSessions("user-a")).rejects.toThrow("query failed");
  });
});
