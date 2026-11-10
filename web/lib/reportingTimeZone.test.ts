import { afterEach, describe, expect, it, vi } from "vitest";
const query = vi.hoisted(() => vi.fn());
const scoped = vi.hoisted(() => vi.fn((_id: string, fn: (q: typeof query) => Promise<unknown>) => fn(query)));
vi.mock("./db", () => ({ scoped }));
import { reportingTimeZone } from "./reportingTimeZone";
afterEach(() => { vi.unstubAllEnvs(); vi.clearAllMocks(); });
describe("account reporting calendar", () => {
  it("keeps concurrent account zones separate and re-reads changed settings", async () => {
    vi.stubEnv("WEB_ACCOUNTS", "true");
    const zones: Record<string, string> = { a: "Pacific/Auckland", b: "America/Los_Angeles" };
    query.mockImplementation(async (_sql, [id]) => [{ zone: zones[id] }]);
    expect(await Promise.all([reportingTimeZone("a"), reportingTimeZone("b")])).toEqual([zones.a, zones.b]);
    zones.a = "Europe/Berlin";
    expect(await reportingTimeZone("a")).toBe("Europe/Berlin");
    expect(scoped.mock.calls.map(([id]) => id)).toEqual(["a", "b", "a"]);
  });
  it("rejects the retired mode without using the global calendar", async () => {
    vi.stubEnv("WEB_ACCOUNTS", "false");
    vi.stubEnv("PULS_TIME_ZONE", "Asia/Tokyo");
    await expect(reportingTimeZone("a")).rejects.toThrow(/WEB_ACCOUNTS/);
    expect(scoped).not.toHaveBeenCalled();
  });
  it("does not silently substitute the global calendar on a failed account lookup", async () => {
    vi.stubEnv("WEB_ACCOUNTS", "true");
    query.mockResolvedValue([]);
    await expect(reportingTimeZone("a")).rejects.toThrow("Reporting time zone unavailable");
  });
});
