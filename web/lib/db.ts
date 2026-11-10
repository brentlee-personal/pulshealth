// Thin Postgres access. Server-only — never import from a client component.
import { Pool, type PoolClient, type QueryConfig } from "pg";
import { isUuid } from "./uuid";
import { readSecret } from "../scripts/secrets.mjs";

let pool: Pool | null = null;
let initialized = false;
let healthPool: Pool | null = null;

const DEFAULT_POOL_SIZE = 4;
const MAX_POOL_SIZE = 50;

/**
 * Connections this process may hold at once, from WEB_DB_POOL_SIZE (default
 * 4, clamped to 1–50; anything unparseable is the default). A type page runs
 * several `scoped` transactions in parallel, so a viewer with a few people on
 * it at once wants more than the default — and the database's own
 * max_connections (and the ingest and API pools) bound how many.
 */
export function poolSize(value: string | undefined = process.env.WEB_DB_POOL_SIZE): number {
  const n = Number.parseInt((value ?? "").trim(), 10);
  if (!Number.isFinite(n)) return DEFAULT_POOL_SIZE;
  return Math.min(MAX_POOL_SIZE, Math.max(1, n));
}

export function getPool(): Pool | null {
  const connectionString = readSecret("DATABASE_URL");
  if (!connectionString) return null;
  if (!initialized) {
    initialized = true;
    pool = new Pool({
      connectionString,
      max: poolSize(),
      // Expanded Timescale/calendar views can generate thousands of JIT
      // functions. Compilation alone exceeded the request timeout in production;
      // these interactive reads are faster interpreted. Set this at startup so
      // it applies to every connection, including replacements after an error.
      options: "-c jit=off",
      connectionTimeoutMillis: 4000,
      idleTimeoutMillis: 10_000,
      // Don't let a heavy ad-hoc query wedge a request.
      statement_timeout: 20_000,
      query_timeout: 22_000,
    });
    // Swallow background idle-client errors so they don't crash the process.
    pool.on("error", () => {});
  }
  return pool;
}

/** A reserved connection: chart saturation must not turn liveness red. */
export function getHealthPool(): Pool | null {
  const connectionString = readSecret("DATABASE_URL");
  if (!connectionString) return null;
  if (!healthPool) {
    healthPool = new Pool({
      connectionString,
      max: 1,
      options: "-c jit=off",
      connectionTimeoutMillis: 4000,
      idleTimeoutMillis: 10_000,
      statement_timeout: 4000,
      query_timeout: 5000,
    });
    healthPool.on("error", () => {});
  }
  return healthPool;
}

/** Only connectivity and role checks; never health records or page queries. */
export const healthQuery: QueryFn = async <T = Record<string, unknown>>(text: string, params: unknown[] = []) => {
  const p = getHealthPool();
  if (!p) throw new Error("DATABASE_URL not configured");
  const res = await p.query(text, params);
  return res.rows as T[];
};

/** Runs one statement on whichever pooled connection is free. */
export type QueryFn = <T = Record<string, unknown>>(text: string, params?: unknown[]) => Promise<T[]>;

/**
 * One statement outside any user scope. For what is not anyone's health
 * data: the health check, the database's time zone, and Basic mode's list
 * of users. Health data goes through `scoped`.
 */
export const query: QueryFn = async <T = Record<string, unknown>>(text: string, params: unknown[] = []) => {
  const p = getPool();
  if (!p) throw new Error("DATABASE_URL not configured");
  const res = await p.query(text, params);
  return res.rows as T[];
};

/**
 * Runs `fn` in one read-only transaction on one connection, with
 * `puls.user_id` set to `userId` for that transaction only.
 *
 * As the `web_app` role (accounts mode) that setting is what the database
 * filters every health relation on (server/db/migrations/015_web_accounts.sql):
 * the query sees `userId`'s rows and nobody else's, whatever its own WHERE
 * clause says. As `grafana` (Basic mode) the setting is inert and the
 * queries' own `user_id = $n` filters do the work, exactly as before.
 *
 * `fn` must issue its statements through `q` only. Calling `query()` or a
 * nested `scoped()` from inside would check out a second connection while
 * this one is held, and a handful of concurrent requests doing that can
 * exhaust the pool and wait on each other until the connection timeout.
 */
export async function scoped<T>(userId: string, fn: (q: QueryFn) => Promise<T>): Promise<T> {
  if (!isUuid(userId)) throw new Error("scoped(): the user id must be a UUID");
  // `true`: local to this transaction. COMMIT or ROLLBACK clears it, so a
  // pooled connection never carries one request's user into the next.
  return inTransaction("BEGIN READ ONLY", [["SELECT set_config('puls.user_id', $1, true)", [userId]]], fn);
}

/**
 * One statement that may take minutes (purging a user, which unpacks shared
 * compressed batches), in its own transaction with the server's and the
 * client's timeouts raised to `minutes` for it alone.
 */
export async function longStatement<T = Record<string, unknown>>(text: string, params: unknown[], minutes: number, beforeCommit?: () => Promise<void>): Promise<T[]> {
  const ms = Math.round(minutes * 60_000);
  return inTransaction("BEGIN", [[`SET LOCAL statement_timeout = ${ms}`, []]], async (_q, client) => {
    // node-postgres reads a per-query query_timeout (client.js); its types omit it.
    const config: QueryConfig<unknown[]> & { query_timeout: number } = { text, values: params, query_timeout: ms + 5_000 };
    const res = await client.query(config);
    if (beforeCommit) await beforeCommit();
    return res.rows as T[];
  });
}

/**
 * Runs `fn` in one read-write transaction with no user scope. For the
 * viewer's own account store (schema auth) only — health data is read
 * through `scoped`, and web_app cannot write it at all. The same rule about
 * issuing statements through `q` only applies.
 */
export async function transaction<T>(fn: (q: QueryFn) => Promise<T>): Promise<T> {
  return inTransaction("BEGIN", [], fn);
}

async function inTransaction<T>(
  begin: string,
  setup: [string, unknown[]][],
  fn: (q: QueryFn, client: PoolClient) => Promise<T>,
): Promise<T> {
  const p = getPool();
  if (!p) throw new Error("DATABASE_URL not configured");

  const client: PoolClient = await p.connect();
  // A connection whose transaction state is unknown must not go back to the
  // pool, or the next request would inherit it (and any user setting).
  let discard: Error | undefined;
  try {
    await client.query(begin);
    for (const [text, params] of setup) await client.query(text, params);
    const q: QueryFn = async <R = Record<string, unknown>>(text: string, params: unknown[] = []) => {
      const res = await client.query(text, params);
      return res.rows as R[];
    };
    const result = await fn(q, client);
    await client.query("COMMIT");
    return result;
  } catch (e) {
    try {
      await client.query("ROLLBACK");
    } catch (rollbackError) {
      discard = rollbackError instanceof Error ? rollbackError : new Error(String(rollbackError));
    }
    throw e;
  } finally {
    client.release(discard);
  }
}
