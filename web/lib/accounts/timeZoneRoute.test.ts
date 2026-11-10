import { NextRequest } from "next/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
const db = vi.hoisted(() => ({ query: vi.fn() }));
const sessions = vi.hoisted(() => ({ findSession: vi.fn() }));
vi.mock("../db", () => db);
vi.mock("./session", async (original) => ({ ...(await original<typeof import("./session")>()), findSession: sessions.findSession }));
import { POST } from "@/app/api/auth/time-zone/route";
const id = Buffer.alloc(32, 4);
function request(zone: string) {
  const body = new FormData(); body.set("time_zone", zone);
  return new NextRequest("https://viewer.example/api/auth/time-zone", { method: "POST", body });
}
beforeEach(() => { vi.stubEnv("WEB_ACCOUNTS", "true"); db.query.mockReset().mockResolvedValue([]); sessions.findSession.mockResolvedValue({ id }); });
afterEach(() => vi.unstubAllEnvs());
describe("reporting zone form", () => {
  it("saves a valid selection using only the authenticated session", async () => {
    const res = await POST(request("Pacific/Auckland"));
    expect(res.headers.get("location")).toBe("/account?notice=time_zone");
    expect(db.query).toHaveBeenCalledWith("SELECT auth.set_my_time_zone($1, $2)", [id, "Pacific/Auckland"]);
  });
  it("refuses malformed zones without reaching the database", async () => {
    expect((await POST(request("Invalid/Zone"))).headers.get("location")).toBe("/account?error=time_zone");
    expect(db.query).not.toHaveBeenCalled();
  });
  it("refuses an expired session before any mutation", async () => {
    sessions.findSession.mockResolvedValue(null);
    expect((await POST(request("UTC"))).headers.get("location")).toBe("/login?next=%2Faccount");
    expect(db.query).not.toHaveBeenCalled();
  });
});
