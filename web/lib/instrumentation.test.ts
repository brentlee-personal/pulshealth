import { afterEach, describe, expect, it, vi } from "vitest";

import { register } from "../instrumentation";

// The startup checks (instrumentation.ts): TRUST_PROXY_HEADERS is read as
// the Go services read it, and a value none of them accepts stops the
// viewer; trusted X-Forwarded-For gets a warning in accounts mode.
describe("register", () => {
  const saved = { ...process.env };
  afterEach(() => {
    process.env = { ...saved };
    vi.restoreAllMocks();
  });

  const run = async (env: Record<string, string>) => {
    vi.restoreAllMocks(); // fresh spies per run, not ones carrying earlier calls
    process.env = { ...saved, NEXT_RUNTIME: "nodejs", ...env };
    delete process.env.WEB_CLIENT_IP_HEADER;
    if (env.WEB_CLIENT_IP_HEADER) process.env.WEB_CLIENT_IP_HEADER = env.WEB_CLIENT_IP_HEADER;
    const exit = vi.spyOn(process, "exit").mockImplementation(((code?: number) => {
      throw new Error(`exit ${code}`);
    }) as typeof process.exit);
    const error = vi.spyOn(console, "error").mockImplementation(() => {});
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    vi.spyOn(console, "log").mockImplementation(() => {});
    const outcome = await register().then(() => "ok", (e: Error) => e.message);
    return { outcome, exit, error: error.mock.calls.map(String), warn: warn.mock.calls.map(String) };
  };

  it("stops at startup on a TRUST_PROXY_HEADERS no service accepts", async () => {
    const { outcome, error } = await run({ TRUST_PROXY_HEADERS: "ture", WEB_ACCOUNTS: "true" });
    expect(outcome).toMatch(/TRUST_PROXY_HEADERS must be true or false/);
    expect(error).toEqual([]);
    expect((await run({ TRUST_PROXY_HEADERS: "Yes", WEB_ACCOUNTS: "true" })).outcome).toBe("ok");
    expect((await run({ TRUST_PROXY_HEADERS: "" })).outcome).toBe("ok");
  });

  it("warns once when trusted proxy headers key on X-Forwarded-For in accounts mode", async () => {
    const xff = await run({ TRUST_PROXY_HEADERS: "on", WEB_ACCOUNTS: "true" });
    expect(xff.warn.filter((w) => /X-Forwarded-For/.test(w))).toHaveLength(1);
    const cf = await run({ TRUST_PROXY_HEADERS: "on", WEB_ACCOUNTS: "true", WEB_CLIENT_IP_HEADER: "cf-connecting-ip" });
    expect(cf.warn, cf.warn.join("\n")).toEqual([]);
  });

  it.each(["WEB_AUTH_PASSWORD", "WEB_DEMO_USER", "PULS_USER_ID", "PULS_MULTI_USER", "PULS_API_TOKEN"])(
    "refuses the retired %s setting without revealing its value", async (name) => {
      const result = await run({ [name]: "synthetic-private-value" });
      expect(result.outcome).toContain(name);
      expect(result.outcome).not.toContain("synthetic-private-value");
    },
  );

});
