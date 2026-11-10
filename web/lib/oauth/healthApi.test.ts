import { oauthKeyFixture } from "./keyFixture";
import { NextRequest } from "next/server";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { issueAccessToken } from "./jwt";
import { GET } from "@/app/api/health/v1/[...path]/route";
import { query } from "../db";
vi.mock("../db", () => ({ query: vi.fn() }));
const oauthKeys = oauthKeyFixture();
const issuer = "https://viewer.example";
const user = "5ea4d000-0000-4000-8000-000000000001";
const other = "5ea4d000-0000-4000-8000-000000000002";
const grant = "5ea4d000-0000-4000-8000-000000000003";
const permissions = { operations: ["metrics:read"], record_types: ["HKQuantityTypeIdentifierStepCount", "HKWorkoutTypeIdentifier"], time_range: { from: "2020-01-01T00:00:00Z", until: "2030-01-01T00:00:00Z" } } satisfies import("./permissions").ReadPermissions;
const authorization = { grantId: grant, permissions, authorizationVersion: 1 };
const config = { issuer, resource: `${issuer}/api/health`, ...oauthKeys };
const request = (path: string, token: string | null = issueAccessToken(config, user, "pc_test", undefined, authorization).token) => GET(
  new NextRequest(`${issuer}/api/health/v1/${path}`, { headers: token ? { Authorization: `Bearer ${token}` } : {} }),
  { params: Promise.resolve({ path: path.split("?")[0].split("/") }) },
);
beforeEach(() => {
  vi.restoreAllMocks(); vi.mocked(query).mockReset();
  Object.assign(process.env, { WEB_ACCOUNTS: "true", WEB_PUBLIC_URL: issuer, PULS_MCP_URL: "https://mcp.example/mcp", ...oauthKeys.env,
    PULS_API_URL: "http://api:8081", PULS_API_TOKEN: "internal-only" });
  vi.mocked(query).mockResolvedValue([{ id: grant, time_zone: "UTC" }]);
});
describe("public account-scoped health API", () => {
  it("returns 503 for unavailable keys without consulting health storage or another credential", async () => {
    process.env.PULS_AUTH_PUBLIC_KEYS_FILE = "synthetic-missing-keys";
    const response = await request("users");
    expect(response.status).toBe(503);
    expect(await response.text()).not.toContain("synthetic-missing-keys");
    expect(query).not.toHaveBeenCalled();
  });
  it("never trusts cookies, MCP tokens, expired tokens, or altered signatures", async () => {
    const t = issueAccessToken(config, user, "pc_test", undefined, authorization).token;
    for (const token of [null, issueAccessToken({ ...config, resource: "https://mcp.example/mcp" }, user, "pc_test").token,
      issueAccessToken(config, user, "pc_test", 100, authorization).token, `${t.slice(0, -5)}abcde`]) {
      expect((await request("users", token)).status).toBe(401);
    }
    expect(query).not.toHaveBeenCalled();
  });
  it("returns only personal identity metadata without fetching household profiles", async () => {
    const fetcher = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json({ users: [{ userID: user, timeZone: "UTC" }, { userID: other, name: "Private" }], default: other }));
    const response = await request("users");
    expect(await response.json()).toEqual({ users: [{ userID: user, timeZone: "UTC" }], default: user, multiUser: false });
    expect(fetcher).not.toHaveBeenCalled();
    expect(query).toHaveBeenCalledWith(expect.stringContaining("g.authorization_version = $5 AND g.permissions = $6::jsonb"),
      [user, "pc_test", config.resource, grant, 1, JSON.stringify(permissions)]);
  });
  it("forwards explicit permitted types and times with the verified member", async () => {
    const fetcher = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json({ metrics: [] }));
    const target = "metrics/daily?types=HKQuantityTypeIdentifierStepCount&start=1767571200000&end=1767657600000";
    expect((await request(target)).status).toBe(200);
    const [url, options] = fetcher.mock.calls[0];
    expect(String(url)).toBe(`http://api:8081/v1/${target}&user=${user}`);
    expect(options).toMatchObject({ redirect: "error", cache: "no-store", headers: { Authorization: "Bearer internal-only" } });
  });
  it("never returns health counts from its permitted-type metadata list", async () => {
    const metadata = [{ identifier: permissions.record_types[0], kind: "quantity", unit: "count" },
      { identifier: "HKWorkoutTypeIdentifier", kind: "workout", unit: null }];
    const fetcher = vi.spyOn(globalThis, "fetch");
    expect(await (await request("catalog/types")).json()).toEqual({ types: metadata });
    expect(query).toHaveBeenCalledTimes(1);
    expect(fetcher).not.toHaveBeenCalled();
  });
  it("refuses operations, types and times beyond the signed scope before touching storage", async () => {
    const target = "metrics/daily?types=HKQuantityTypeIdentifierStepCount&start=1767571200000&end=1767657600000";
    const rawOnly = { ...authorization, permissions: { ...permissions, operations: ["samples:read"] } } satisfies import("./jwt").GrantAuthorization;
    expect((await request(target, issueAccessToken(config, user, "pc_test", undefined, rawOnly).token)).status).toBe(403);
    for (const path of [
      target.replace("StepCount", "HeartRate"),
      `metrics/daily?types=HKQuantityTypeIdentifierStepCount&start=${Date.UTC(2019, 0, 1)}&end=${Date.UTC(2019, 0, 2)}`,
      `metrics/daily?types=HKQuantityTypeIdentifierStepCount&start=${Date.UTC(2030, 0, 1)}&end=${Date.UTC(2030, 0, 2)}`,
      `users?user=${user}`,
    ]) expect((await request(path)).status).toBe(403);
    expect(query).not.toHaveBeenCalled();
  });
  it("bounds the default workout request by both the grant and a 31-day window", async () => {
    const now = Date.UTC(2026, 0, 15);
    vi.spyOn(Date, "now").mockReturnValue(now);
    const fetcher = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json({ workouts: [] }));
    expect((await request("workouts")).status).toBe(200);
    const url = new URL(String(fetcher.mock.calls[0][0]));
    expect(url.searchParams.get("start")).toBe(String(now - 31 * 86400_000));
    expect(url.searchParams.get("end")).toBe(String(now));
    expect(url.searchParams.get("user")).toBe(user);
  });
  it("rejects cross-account, duplicated, unknown, and unbounded queries", async () => {
    for (const path of [`users?user=${other}`, `users?user=${user}&user=${user}`, "users?debug=true", "workouts?limit=500", "metrics/daily?start=1&end=9999999999999", "sleep/daily", "export", "constructor", "toString", "__proto__"]) {
      expect((await request(path)).status).toBeGreaterThanOrEqual(400);
    }
    expect(query).not.toHaveBeenCalled();
  });
  it("revoked or disabled accounts cannot read even with an unexpired token", async () => {
    vi.mocked(query).mockResolvedValue([]);
    expect((await request("users")).status).toBe(401);
  });
  it("returns 503 when the live authorization database is unavailable", async () => {
    vi.mocked(query).mockRejectedValue(new Error("synthetic authorization outage"));
    const fetcher = vi.spyOn(globalThis, "fetch");
    const response = await request("users");
    expect(response.status).toBe(503);
    expect(await response.text()).not.toContain("synthetic authorization outage");
    expect(fetcher).not.toHaveBeenCalled();
  });
  it("does not leak upstream errors or the deployment token", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("private internal-only", { status: 401 }));
    const response = await request("workouts");
    expect(response.status).toBe(503);
    expect(await response.text()).not.toContain("internal-only");
  });
});
