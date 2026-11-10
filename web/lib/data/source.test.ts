import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const probe = vi.hoisted(() => vi.fn());
vi.mock("../db", () => ({ healthQuery: probe }));

describe("personal storage readiness", () => {
  afterEach(() => vi.unstubAllEnvs());
  beforeEach(() => {
    vi.resetModules();
    vi.stubEnv("DATABASE_URL", "postgres://synthetic");
    probe.mockReset();
  });

  it("accepts only the restricted application role", async () => {
    probe.mockResolvedValue([{ role: "web_app", direct: false }]);
    const { getDataSource } = await import("./source");
    expect((await getDataSource()).source).toBe("live");
  });

  it.each(["postgres", "grafana", "api_reader"])("refuses the %s role", async (role) => {
    probe.mockResolvedValue([{ role, direct: false }]);
    const { getDataSource } = await import("./source");
    expect((await getDataSource()).source).toBe("error");
  });

  it("refuses an application role with direct health grants", async () => {
    probe.mockResolvedValue([{ role: "web_app", direct: true }]);
    const { getDataSource } = await import("./source");
    expect((await getDataSource()).source).toBe("error");
  });

  it.each(["development", "production"])("never invents health values in %s", async (environment) => {
    vi.stubEnv("NODE_ENV", environment);
    vi.stubEnv("DATABASE_URL", "");
    const { liveRead } = await import("./source");
    const read = vi.fn();
    await expect(liveRead("synthetic", read)).rejects.toThrow(/not configured/);
    expect(read).not.toHaveBeenCalled();
  });
});
