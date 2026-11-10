import { NextRequest } from "next/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/accounts/session", () => ({
  findSession: vi.fn(),
  SESSION_COOKIE: "puls-session",
  sessionCookieAttributes: () => ({ httpOnly: true, secure: true, path: "/", maxAge: 0 }),
}));

import proxy from "@/proxy";
import { findSession } from "@/lib/accounts/session";

const lookup = vi.mocked(findSession);
const request = (path = "/", method = "GET", cookie?: string) => new NextRequest(`https://viewer.example${path}`, {
  method,
  headers: { origin: "https://viewer.example", ...(cookie ? { cookie: `puls-session=${cookie}` } : {}) },
});

beforeEach(() => {
  vi.stubEnv("WEB_ACCOUNTS", "true");
  vi.stubEnv("WEB_DEMO_USER", "");
  vi.stubEnv("WEB_PUBLIC_URL", "https://viewer.example");
  vi.stubEnv("TRUST_PROXY_HEADERS", "false");
  lookup.mockResolvedValue(null);
});
afterEach(() => { vi.unstubAllEnvs(); vi.clearAllMocks(); });

describe("signed-out dashboard entry", () => {
  it("requires personal sign-in before opening the dashboard", async () => {
    const response = await proxy(request());
    expect(response.status).toBe(303);
    expect(response.headers.get("location")).toBe("https://viewer.example/login");
    expect(response.headers.get("cache-control")).toBe("no-store");
  });

  it("clears an expired cookie before personal sign-in", async () => {
    const response = await proxy(request("/", "GET", "expired"));
    expect(response.headers.get("location")).toBe("https://viewer.example/login");
    expect(response.cookies.get("puls-session")?.value).toBe("");
  });

  it("keeps authentication for other pages, APIs and writes", async () => {
    expect((await proxy(request("/account"))).headers.get("location")).toBe("https://viewer.example/login?next=%2Faccount");
    expect((await proxy(request("/api/user"))).status).toBe(401);
    expect((await proxy(request("/", "POST"))).status).toBe(401);
    expect((await proxy(request("/login"))).headers.get("x-middleware-next")).toBe("1");
  });

  it("serves a live personal session without switching members", async () => {
    lookup.mockResolvedValue({
      id: Buffer.alloc(32), accountId: "account", userId: "own-user", email: "person@example.invalid",
      isAdmin: false, selfService: false, canDelete: false, refreshed: false,
    });
    const response = await proxy(request("/", "GET", "active"));
    expect(response.headers.get("x-middleware-next")).toBe("1");
    expect(response.headers.get("location")).toBeNull();
    expect(response.cookies.get("puls-session")).toBeUndefined();
  });

});
