import { secretFixture } from "./secretFixture";
import { beforeEach, describe, expect, it, vi } from "vitest";

// A stand-in for pg's Pool: one client whose statements are recorded, and a
// switch to make a given statement fail.
const fake = vi.hoisted(() => ({
  configs: [] as Record<string, unknown>[],
  statements: [] as { text: string; params?: unknown[] }[],
  failOn: null as string | null,
  connects: 0,
  released: [] as (Error | boolean | undefined)[],
}));

vi.mock("pg", () => {
  class Pool {
    constructor(config: Record<string, unknown>) { fake.configs.push(config); }
    on() {}
    async query(text: string, params?: unknown[]) {
      fake.statements.push({ text, params });
      return { rows: [{ pooled: true }] };
    }
    async connect() {
      fake.connects++;
      return {
        query: async (text: string, params?: unknown[]) => {
          fake.statements.push({ text, params });
          if (fake.failOn !== null && text.startsWith(fake.failOn)) throw new Error(`failed: ${text}`);
          return { rows: [{ text }] };
        },
        release: (err?: Error | boolean) => {
          fake.released.push(err);
        },
      };
    }
  }
  return { Pool };
});

const databaseSecret = secretFixture({ DATABASE_URL: "postgres://test" });
const USER = "11111111-1111-4111-8111-111111111111";

beforeEach(() => {
  delete process.env.DATABASE_URL;
  Object.assign(process.env, databaseSecret);
  fake.statements.length = 0;
  fake.released.length = 0;
  fake.failOn = null;
  fake.connects = 0;
});

const texts = () => fake.statements.map((s) => s.text);

describe("poolSize", () => {
  it("reads WEB_DB_POOL_SIZE, defaulting to 4 and clamping to 1–50", async () => {
    const { poolSize } = await import("./db");
    expect(poolSize(undefined)).toBe(4);
    expect(poolSize("")).toBe(4);
    expect(poolSize("many")).toBe(4);
    expect(poolSize(" 12 ")).toBe(12);
    expect(poolSize("0")).toBe(1);
    expect(poolSize("-3")).toBe(1);
    expect(poolSize("500")).toBe(50);
  });
});

describe("scoped", () => {
  it("runs the callback in one read-only transaction with the user set locally", async () => {
    const { scoped } = await import("./db");
    const rows = await scoped(USER, async (q) => {
      await q("SELECT 1");
      return q<{ text: string }>("SELECT 2", [42]);
    });
    expect(rows).toEqual([{ text: "SELECT 2" }]);
    expect(texts()).toEqual([
      "BEGIN READ ONLY",
      "SELECT set_config('puls.user_id', $1, true)",
      "SELECT 1",
      "SELECT 2",
      "COMMIT",
    ]);
    // The user travels as a bind parameter, local to the transaction (`true`).
    expect(fake.statements[1].params).toEqual([USER]);
    expect(fake.statements[3].params).toEqual([42]);
    expect(fake.connects).toBe(1);
    expect(fake.released).toEqual([undefined]);
  });

  it("rolls back, rethrows and returns the connection when the callback fails", async () => {
    const { scoped } = await import("./db");
    await expect(
      scoped(USER, async (q) => {
        await q("SELECT 1");
        throw new Error("boom");
      }),
    ).rejects.toThrow("boom");
    expect(texts()).toEqual([
      "BEGIN READ ONLY",
      "SELECT set_config('puls.user_id', $1, true)",
      "SELECT 1",
      "ROLLBACK",
    ]);
    expect(fake.released).toEqual([undefined]);
  });

  it("rolls back when a statement fails", async () => {
    const { scoped } = await import("./db");
    fake.failOn = "SELECT broken";
    await expect(scoped(USER, (q) => q("SELECT broken"))).rejects.toThrow("failed: SELECT broken");
    expect(texts().at(-1)).toBe("ROLLBACK");
    expect(fake.released).toEqual([undefined]);
  });

  it("throws the connection away when even ROLLBACK fails", async () => {
    // Its transaction state, and so its user setting, is unknown: it must not
    // serve the next request.
    const { scoped } = await import("./db");
    fake.failOn = "ROLLBACK";
    await expect(
      scoped(USER, async () => {
        throw new Error("boom");
      }),
    ).rejects.toThrow("boom");
    expect(fake.released).toHaveLength(1);
    expect(fake.released[0]).toBeInstanceOf(Error);
  });

  it("refuses a user id that is not a UUID before touching the pool", async () => {
    const { scoped } = await import("./db");
    for (const bad of ["", "everyone", "' OR true --", `${USER} `]) {
      await expect(scoped(bad, async () => 1)).rejects.toThrow(/UUID/);
    }
    expect(fake.connects).toBe(0);
    expect(fake.statements).toEqual([]);
  });

  it("leaves query() as a plain pooled statement", async () => {
    const { query } = await import("./db");
    expect(await query("SELECT 1")).toEqual([{ pooled: true }]);
    expect(fake.connects).toBe(0);
    expect(texts()).toEqual(["SELECT 1"]);
  });
});


describe("interactive connection settings", () => {
  it("disables JIT on both pools and reserves a distinct connection for probes", async () => {
    vi.resetModules();
    fake.configs.length = 0;
    const { getPool, getHealthPool, healthQuery } = await import("./db");
    expect(getPool()).not.toBe(getHealthPool());
    expect(fake.configs).toHaveLength(2);
    expect(fake.configs[0]).toMatchObject({ options: "-c jit=off", max: 4 });
    expect(fake.configs[1]).toMatchObject({ options: "-c jit=off", max: 1, statement_timeout: 4000 });
    await healthQuery("SELECT 1");
    expect(fake.configs).toHaveLength(2);
  });
});
