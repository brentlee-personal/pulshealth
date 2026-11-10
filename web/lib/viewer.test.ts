import { beforeEach, describe, expect, it, vi } from "vitest";

const auth = vi.hoisted(() => ({ findSession: vi.fn(), cookies: vi.fn() }));
vi.mock("next/headers", () => ({ cookies: auth.cookies }));
vi.mock("next/navigation", () => ({ redirect: (path: string) => { throw new Error(`REDIRECT ${path}`); } }));
vi.mock("./accounts/session", () => ({ findSession: auth.findSession, SESSION_COOKIE: "__Host-puls-session" }));

describe("personal viewer identity", () => {
  beforeEach(() => {
    auth.findSession.mockReset();
    auth.cookies.mockReset().mockResolvedValue({
      get: (name: string) => ({ value: name === "__Host-puls-session" ? "synthetic-owner-session" : "another-member" }),
    });
  });

  it("uses the verified member and ignores a forged chooser cookie", async () => {
    auth.findSession.mockResolvedValue({ userId: "own-member" });
    const { viewerUser } = await import("./viewer");
    expect(await viewerUser()).toBe("own-member");
  });

  it("does not fall back to a member when the session is missing", async () => {
    auth.findSession.mockResolvedValue(null);
    const { viewerUser } = await import("./viewer");
    await expect(viewerUser()).rejects.toThrow("REDIRECT /login");
  });
});

describe("safeReturnPath", () => {
  it("keeps same-origin paths and refuses everything else", async () => {
    const { safeReturnPath } = await import("./viewer");
    expect(safeReturnPath("/workouts?range=W")).toBe("/workouts?range=W");
    expect(safeReturnPath("/")).toBe("/");
    expect(safeReturnPath(undefined)).toBe("/");
    expect(safeReturnPath("")).toBe("/");
    expect(safeReturnPath("workouts")).toBe("/");
    expect(safeReturnPath("https://example.com/")).toBe("/");
    expect(safeReturnPath("//example.com/")).toBe("/");
    // Browsers strip tabs and newlines from URLs before parsing them, so these
    // would arrive as //evil.example.
    expect(safeReturnPath("/\t/evil.example")).toBe("/");
    expect(safeReturnPath("/\n/evil.example")).toBe("/");
    expect(safeReturnPath("/\r\n/evil.example")).toBe("/");
    expect(safeReturnPath("/x\\y")).toBe("/");
    expect(safeReturnPath("/workouts/a?b=1#c")).toBe("/workouts/a?b=1#c");
    // Dot segments that parsing collapses into a scheme-relative URL.
    for (const sneaky of ["/.//evil.example", "/..//evil.example", "/%2e//evil.example", "/a/..//evil.example", "/././/evil.example/x"]) {
      expect(safeReturnPath(sneaky), sneaky).toBe("/");
    }
    expect(safeReturnPath("/a/../workouts")).toBe("/workouts");
    expect(safeReturnPath("/\\example.com")).toBe("/");
  });
});
