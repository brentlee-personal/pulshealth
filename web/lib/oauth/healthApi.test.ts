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
const config = { issuer, resource: `${issuer}/api/health`, ...oauthKeys };
const request = (path: string, token: string | null = issueAccessToken(config, user, "pc_test", undefined, grant).token) => GET(
  new NextRequest(`${issuer}/api/health/v1/${path}`, { headers: token ? { Authorization: `Bearer ${token}` } : {} }),
  { params: Promise.resolve({ path: path.split("?")[0].split("/") }) },
);
beforeEach(() => {
  vi.restoreAllMocks(); vi.mocked(query).mockReset();
  Object.assign(process.env, { WEB_ACCOUNTS: "true", WEB_PUBLIC_URL: issuer, PULS_MCP_URL: "https://mcp.example/mcp", ...oauthKeys.env,
    PULS_API_URL: "http://api:8081", PULS_API_TOKEN: "internal-only" });
  vi.mocked(query).mockResolvedValue([{ id: grant }]);
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
    const t = issueAccessToken(config, user, "pc_test", undefined, grant).token;
    for (const token of [null, issueAccessToken({ ...config, resource: "https://mcp.example/mcp" }, user, "pc_test").token,
      issueAccessToken(config, user, "pc_test", 100, grant).token, `${t.slice(0, -5)}abcde`]) {
      expect((await request("users", token)).status).toBe(401);
    }
    expect(query).not.toHaveBeenCalled();
  });
  it("filters the user list and forces the token subject upstream", async () => {
    const fetcher = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json({ users: [{ userID: user, timeZone: "UTC" }, { userID: other, name: "Private" }], default: other }));
    const response = await request("users");
    expect(await response.json()).toEqual({ users: [{ userID: user, timeZone: "UTC" }], default: user, multiUser: false });
    const [url, options] = fetcher.mock.calls[0];
    expect(String(url)).toBe(`http://api:8081/v1/users?user=${user}`);
    expect(options).toMatchObject({ redirect: "error", cache: "no-store", headers: { Authorization: "Bearer internal-only" } });
    expect(query).toHaveBeenCalledWith(expect.any(String), [user, "pc_test", config.resource, grant]);
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
  it("does not leak upstream errors or the deployment token", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("private internal-only", { status: 401 }));
    const response = await request("users");
    expect(response.status).toBe(503);
    expect(await response.text()).not.toContain("internal-only");
  });
});
