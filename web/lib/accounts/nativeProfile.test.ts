import { createHash } from "node:crypto";
import { NextRequest } from "next/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
const mocks = vi.hoisted(() => ({ query: vi.fn(), scoped: vi.fn(), transaction: vi.fn(), findSession: vi.fn(), findPasswordHash: vi.fn(), verifyPassword: vi.fn() }));
vi.mock("../db", () => ({ query: mocks.query, scoped: mocks.scoped, transaction: mocks.transaction }));
vi.mock("./session", async original => ({ ...(await original<typeof import("./session")>()), findSession: mocks.findSession }));
vi.mock("./store", async original => ({ ...(await original<typeof import("./store")>()), findPasswordHash: mocks.findPasswordHash }));
vi.mock("./password", () => ({ verifyPassword: mocks.verifyPassword }));
import { GET, PUT, POST, DELETE } from "@/app/api/native/profile/route";
import { POST as connect } from "@/app/api/auth/connect-account/route";
import { classifyPath, decideAccounts } from "./policy";
const token = "A".repeat(43);
const session = { id: Buffer.alloc(32, 1), accountId: "account", userId: "own-user", email: "old@example.com" };
function request(method = "GET", body?: unknown, authorization: string | null = `Bearer ${token}`) {
  const headers: Record<string, string> = { "content-type": "application/json", cookie: `__Host-puls-session=${token}` };
  if (authorization) headers.authorization = authorization;
  return new NextRequest("https://viewer.example/api/native/profile", { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
}
const profile = { name: "Person", email: session.email, dateOfBirth: null, biologicalSex: null };
beforeEach(() => {
  vi.stubEnv("WEB_ACCOUNTS", "true");
  Object.values(mocks).forEach(mock => mock.mockReset());
  mocks.findSession.mockResolvedValue(session);
  mocks.query.mockResolvedValue([]);
  mocks.scoped.mockImplementation(async (_user, fn) => fn(vi.fn().mockResolvedValue([{ name: "Person", dateOfBirth: null, biologicalSex: null }])));
  mocks.transaction.mockImplementation(async fn => fn(mocks.query));
});
afterEach(() => vi.unstubAllEnvs());
describe("native account profile", () => {
  it("exempts exactly the machine route and still requires TLS", () => {
    expect(classifyPath("/api/native/profile")).toBe("oauth");
    expect(classifyPath("/api/native/profile/extra")).toBe("protected");
    expect(classifyPath("/api/auth/connect-account")).toBe("protected");
    expect(decideAccounts({ pathname: "/api/native/profile", secure: false, sameOrigin: false, development: false })).toBe("insecure");
  });
  it("never authenticates a cookie at the bearer-only endpoint", async () => {
    expect((await GET(request("GET", undefined, null))).status).toBe(401);
    expect(mocks.findSession).not.toHaveBeenCalled();
    expect(mocks.scoped).not.toHaveBeenCalled();
  });
  it("refuses an expired session before accessing a profile", async () => {
    mocks.findSession.mockResolvedValue(null);
    expect((await PUT(request("PUT", profile))).status).toBe(401);
    expect(mocks.query).not.toHaveBeenCalled();
    expect(mocks.scoped).not.toHaveBeenCalled();
  });
  it("loads only the live session's profile and login email", async () => {
    const response = await GET(request());
    expect(await response.json()).toEqual({ ...profile, userID: "own-user" });
    expect(mocks.scoped).toHaveBeenCalledWith("own-user", expect.any(Function));
    expect(response.headers.get("cache-control")).toBe("no-store");
  });
  it("ignores a caller's target UUID and authorizes writes with the session hash", async () => {
    expect((await PUT(request("PUT", { ...profile, userID: "someone-else" }))).status).toBe(200);
    expect(mocks.query).toHaveBeenCalledWith("SELECT auth.set_my_profile($1, $2, $3, $4, $5, $6)",
      [session.id, "Person", session.email, null, null, null]);
  });
  it("requires a valid current password before changing the login email", async () => {
    mocks.findPasswordHash.mockResolvedValue("stored-hash");
    mocks.verifyPassword.mockResolvedValue(false);
    expect((await PUT(request("PUT", { ...profile, email: "new@example.com", currentPassword: "wrong" }))).status).toBe(403);
    expect(mocks.query).not.toHaveBeenCalled();
    mocks.verifyPassword.mockResolvedValue(true);
    expect((await PUT(request("PUT", { ...profile, email: "new@example.com", currentPassword: "correct" }))).status).toBe(200);
    expect(mocks.query.mock.calls[0][1]).toEqual([session.id, "Person", "new@example.com", null, null, "stored-hash"]);
  });
  it("rejects malformed/oversized profiles and reports failed writes", async () => {
    for (const body of [null, {}, { ...profile, dateOfBirth: Date.now() + 86400000 }, { ...profile, biologicalSex: "bad" }]) {
      expect((await PUT(request("PUT", body))).status).toBe(400);
    }
    expect((await PUT(request("PUT", { ...profile, name: "x".repeat(20000) }))).status).toBe(413);
    mocks.query.mockRejectedValue({ code: "23505" });
    expect((await PUT(request("PUT", profile))).status).toBe(409);
  });
  it("ends only the supplied account session", async () => {
    expect((await DELETE(request("DELETE"))).status).toBe(200);
    expect(mocks.query).toHaveBeenCalledWith("DELETE FROM auth.sessions WHERE id = $1", [session.id]);
  });
  it("exchanges only a matching, live, one-use PKCE code before issuing a session", async () => {
    const verifier = "v".repeat(64);
    mocks.query.mockResolvedValueOnce([{ id: "own-account" }]).mockResolvedValueOnce([{ account_id: "own-account" }]).mockResolvedValueOnce([]);
    const response = await POST(request("POST", { code: token, verifier }));
    expect(response.status).toBe(200);
    const [sql, params] = mocks.query.mock.calls[1];
    expect(mocks.query.mock.calls[0][0]).toMatch(/FOR UPDATE OF a/);
    expect(sql).toMatch(/s.id = h.source_session/);
    expect(sql).toContain("s.purpose = 'personal'");
    expect(sql).toMatch(/DELETE FROM auth.native_handoffs/);
    expect(sql).toMatch(/expires_at > now\(\)/);
    expect(params[1]).toBe(createHash("sha256").update(verifier).digest("base64url"));
    expect(mocks.query.mock.calls[2][1][1]).toBe("own-account");
    expect((await POST(request("POST", { code: token, verifier }))).status).toBe(401);
  });
  it("browser consent returns only a bound one-use code, never a session token", async () => {
    const body = new FormData(); body.set("state", "a".repeat(64)); body.set("challenge", "c".repeat(43));
    const response = await connect(new NextRequest("https://viewer.example/api/auth/connect-account", { method: "POST", body }));
    const callback = new URL(response.headers.get("location")!);
    expect(callback.host).toBe("account");
    expect(callback.searchParams.get("state")).toBe("a".repeat(64));
    expect(callback.searchParams.get("code")).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(callback.searchParams.has("token")).toBe(false);
    expect(mocks.query.mock.calls.at(-1)?.[1].slice(1)).toEqual(["c".repeat(43), "account", session.id]);
  });
});
