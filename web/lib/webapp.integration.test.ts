// The web_app role against a real, migrated TimescaleDB (015_web_accounts.sql
// and 099_read_roles.sh): what the database lets the viewer's accounts mode
// read. Skipped unless both connection strings are set, so `npm test` never
// needs a database; CI's db-integration job runs it after the Go suites:
//
//   WEB_APP_DATABASE_URL=postgres://web_app:<WEB_DB_PASSWORD>@127.0.0.1:5432/postgres
//   ADMIN_DATABASE_URL=postgres://postgres:<POSTGRES_PASSWORD>@127.0.0.1:5432/postgres
//   npm run test:integration   # every lib/*.integration.test.ts
//
// Fixtures go in as the superuser for two fresh users, with the old half of
// each hypertable's rows compressed, and are read back as web_app. The claim
// under test is the one the viewer relies on: with puls.user_id set in the
// transaction, every relation the viewer reads returns that user's rows and
// no one else's; without it, nothing; and nothing beneath the views is
// reachable at all.

import { randomUUID } from "node:crypto";
import { Client, DatabaseError } from "pg";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { configuredTimeZone } from "./config";
import { requireIntegrationDatabases } from "./integrationEnv";

// In CI the job sets both; a missing one there must fail, not skip quietly
// (PULS_CI_REQUIRE_INTEGRATION / PULS_WEB_INTEGRATION, lib/integrationEnv.ts).
const { webUrl: WEB_URL, adminUrl: ADMIN_URL } = requireIntegrationDatabases();

const A = randomUUID();
const B = randomUUID();
const STEPS = "HKQuantityTypeIdentifierStepCount";
const SLEEP = "HKCategoryTypeIdentifierSleepAnalysis";
const HEART = "HKQuantityTypeIdentifierHeartRate";
// Old enough that its 30-day chunk ends before the compression cut below.
const OLD_DAYS = 150;

// Every relation the viewer's SQL names, as web_app resolves it.
const VIEWS = [
  "users",
  "quantity_samples",
  "category_samples",
  "workouts",
  "sources",
  "workout_route_points",
  "workout_series_points",
  "activity_summaries",
  "metric_daily",
] as const;

async function connect(url: string): Promise<Client> {
  const client = new Client({ connectionString: url });
  await client.connect();
  return client;
}

/** The SQLSTATE a statement fails with, or null when it succeeds. */
async function sqlState(client: Client, text: string, params: unknown[] = []): Promise<string | null> {
  try {
    await client.query("BEGIN");
    await client.query(text, params);
    return null;
  } catch (e) {
    return e instanceof DatabaseError ? (e.code ?? "unknown") : "unknown";
  } finally {
    await client.query("ROLLBACK");
  }
}

/** Runs `fn` in a transaction scoped to `userId`, the way the viewer does. */
async function asUser<T>(client: Client, userId: string, fn: () => Promise<T>): Promise<T> {
  await client.query("BEGIN");
  try {
    await client.query("SELECT set_config('puls.user_id', $1, true)", [userId]);
    return await fn();
  } finally {
    await client.query("COMMIT");
  }
}

async function count(client: Client, relation: string, where = "true", params: unknown[] = []): Promise<number> {
  const { rows } = await client.query<{ n: string }>(`SELECT count(*) AS n FROM ${relation} WHERE ${where}`, params);
  return Number(rows[0].n);
}

describe.skipIf(!WEB_URL || !ADMIN_URL)("web_app role (integration)", () => {
  let admin: Client;
  let web: Client;
  const sourceIds: number[] = [];
  const workoutIds = { [A]: randomUUID(), [B]: randomUUID() };

  beforeAll(async () => {
    admin = await connect(ADMIN_URL!);
    web = await connect(WEB_URL!);

    const typeId = async (identifier: string, kind: string, unit: string) => {
      await admin.query(
        "INSERT INTO sample_types (identifier, kind, unit) VALUES ($1, $2, $3) ON CONFLICT (identifier) DO NOTHING",
        [identifier, kind, unit],
      );
      const { rows } = await admin.query<{ type_id: number }>(
        "SELECT type_id FROM sample_types WHERE identifier = $1", [identifier],
      );
      return rows[0].type_id;
    };
    const steps = await typeId(STEPS, "quantity", "count");
    const sleep = await typeId(SLEEP, "category", "");
    const heart = await typeId(HEART, "quantity", "count/min");
    // metric_daily covers a type only once it has a sum/average series.
    await admin.query(
      `INSERT INTO aggregate_series (type_id, agg_func, interval_value, interval_unit, device_filter, unit)
       VALUES ($1, 'sum', 1, 'day', 'all', 'count') ON CONFLICT DO NOTHING`,
      [steps],
    );

    for (const user of [A, B]) {
      await admin.query("INSERT INTO users (id, name, email) VALUES ($1, $2, $3)", [
        user, `it-${user.slice(0, 8)}`, `${user.slice(0, 8)}@example.com`,
      ]);
      const { rows } = await admin.query<{ source_id: number }>(
        "INSERT INTO sources (name, bundle_id) VALUES ($1, 'com.example.it') RETURNING source_id",
        [`it-watch-${user}`],
      );
      const source = rows[0].source_id;
      sourceIds.push(source);
      const workout = workoutIds[user];
      await admin.query(
        `INSERT INTO quantity_samples (uuid, type_id, start_ts, end_ts, value, source_id, user_id) VALUES
           (gen_random_uuid(), $1, now() - make_interval(days => $3), now() - make_interval(days => $3), 100, $4, $2),
           (gen_random_uuid(), $1, now() - interval '1 day', now() - interval '1 day', 200, $4, $2)`,
        [steps, user, OLD_DAYS, source],
      );
      await admin.query(
        `INSERT INTO category_samples (uuid, type_id, start_ts, end_ts, value, source_id, user_id)
         VALUES (gen_random_uuid(), $1, now() - interval '8 hours', now() - interval '1 hour', 3, $3, $2)`,
        [sleep, user, source],
      );
      await admin.query(
        `INSERT INTO workouts (uuid, activity_type, start_ts, end_ts, duration_s, source_id, user_id)
         VALUES ($1, 'running', now() - make_interval(days => $3), now() - make_interval(days => $3) + interval '30 minutes', 1800, $4, $2)`,
        [workout, user, OLD_DAYS, source],
      );
      await admin.query(
        `INSERT INTO workout_route_points (workout_uuid, ts, lat, lon, user_id)
         SELECT $1, now() - make_interval(days => $3) + make_interval(mins => g), 52.5 + g / 1000.0, 13.4, $2
           FROM generate_series(1, 3) g`,
        [workout, user, OLD_DAYS],
      );
      await admin.query(
        `INSERT INTO workout_series_points (workout_uuid, type_id, ts, value, user_id)
         SELECT $1, $4, now() - make_interval(days => $3) + make_interval(mins => g), 140 + g, $2
           FROM generate_series(1, 3) g`,
        [workout, user, OLD_DAYS, heart],
      );
      // "Today" in the zone getActivityRings reads it in, not the database
      // session's (UTC on CI): from midnight UTC until the zone's own
      // midnight, the two are different dates.
      await admin.query(
        "INSERT INTO activity_summaries (date, user_id, move_kcal) VALUES ((now() AT TIME ZONE $2::text)::date, $1, 400)",
        [user, configuredTimeZone()],
      );
    }

    // Materialize these fixtures explicitly: yesterday can be below the
    // real-time rollup watermark after another suite refreshed the view.
    // The test must not depend on the background refresh job's timing.
    await admin.query("CALL refresh_continuous_aggregate('quantity_rollups', now() - interval '400 days', now())");

    // The old halves go into the columnstore, so the views are proven over
    // compressed chunks too, not only over plain heap rows.
    for (const hypertable of ["quantity_samples", "workout_series_points"]) {
      await admin.query(
        `SELECT compress_chunk(c, if_not_compressed => true)
           FROM show_chunks($1::regclass, older_than => interval '90 days') c`,
        [hypertable],
      );
    }
  }, 60_000);

  afterAll(async () => {
    // The viewer's own pool, opened by the tests that drive lib/queries.ts.
    await (await import("./db")).getPool()?.end();
    await (await import("./db")).getHealthPool()?.end();
    if (admin) {
      const users = [A, B];
      // Compressed chunks: a prunable predicate, and no decompression cap.
      await admin.query("BEGIN");
      await admin.query("SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0");
      await admin.query(
        "DELETE FROM quantity_samples WHERE user_id = ANY($1) AND start_ts > now() - interval '400 days'", [users],
      );
      await admin.query(
        "DELETE FROM workout_series_points WHERE user_id = ANY($1) AND ts > now() - interval '400 days'", [users],
      );
      await admin.query(
        "DELETE FROM workout_route_points WHERE user_id = ANY($1) AND ts > now() - interval '400 days'", [users],
      );
      for (const table of ["category_samples", "workouts", "activity_summaries"]) {
        await admin.query(`DELETE FROM ${table} WHERE user_id = ANY($1)`, [users]);
      }
      await admin.query("DELETE FROM auth.accounts WHERE user_id = ANY($1)", [users]);
      await admin.query("DELETE FROM users WHERE id = ANY($1)", [users]);
      await admin.query("DELETE FROM sources WHERE source_id = ANY($1)", [sourceIds]);
      await admin.query("COMMIT");
      await admin.end();
    }
    if (web) await web.end();
  }, 60_000);

  it("connects with the per-user views first on its search_path", async () => {
    const { rows } = await web.query<{ path: string; role: string }>(
      "SELECT current_setting('search_path') AS path, current_user AS role",
    );
    expect(rows[0]).toEqual({ path: "web, public", role: "web_app" });
  });

  it("really did compress the old rows", async () => {
    for (const hypertable of ["quantity_samples", "workout_series_points"]) {
      const { rows } = await admin.query<{ compressed: boolean }>(
        `SELECT bool_or(is_compressed) AS compressed FROM timescaledb_information.chunks
          WHERE hypertable_name = $1 AND range_end < now() - interval '90 days'`,
        [hypertable],
      );
      expect(rows[0].compressed, hypertable).toBe(true);
    }
  });

  it("reads nothing at all without puls.user_id", async () => {
    for (const view of VIEWS) {
      expect(await count(web, view), view).toBe(0);
    }
  });

  it("reads exactly the scoped user's rows, compressed chunks included", async () => {
    for (const [user, other] of [[A, B], [B, A]]) {
      await asUser(web, user, async () => {
        // A query with no user filter of its own: the view supplies it.
        expect(await count(web, "quantity_samples")).toBe(2);
        expect(await count(web, "quantity_samples", "start_ts < now() - interval '90 days'")).toBe(1);
        expect(await count(web, "category_samples")).toBe(1);
        expect(await count(web, "workouts")).toBe(1);
        expect(await count(web, "workout_route_points")).toBe(3);
        expect(await count(web, "workout_series_points")).toBe(3);
        expect(await count(web, "activity_summaries")).toBe(1);
        expect(await count(web, "metric_daily")).toBeGreaterThan(0);
        expect(await count(web, "users")).toBe(1);
        expect(await count(web, "users", "id = $1", [user])).toBe(1);

        for (const view of VIEWS.filter((v) => v !== "users" && v !== "sources")) {
          expect(await count(web, view, "user_id <> $1", [user]), view).toBe(0);
          // Asking for the other user by name gets nothing either.
          expect(await count(web, view, "user_id = $1", [other]), view).toBe(0);
        }
        const { rows } = await web.query<{ name: string }>("SELECT name FROM sources");
        expect(rows.map((r) => r.name)).toEqual([`it-watch-${user}`]);
      });
    }
  });

  it("joins the way the viewer's queries do", async () => {
    await asUser(web, A, async () => {
      const { rows } = await web.query<{ n: string }>(
        `SELECT count(*) AS n FROM quantity_samples q
           JOIN sample_types st ON st.type_id = q.type_id
          WHERE st.identifier = $1 AND q.user_id = $2::uuid`,
        [STEPS, A],
      );
      expect(Number(rows[0].n)).toBe(2);
      const detail = await web.query<{ source: string | null }>(
        `SELECT s.name AS source FROM workouts w LEFT JOIN sources s ON s.source_id = w.source_id
          WHERE w.uuid = $1::uuid`,
        [workoutIds[A]],
      );
      expect(detail.rows).toEqual([{ source: `it-watch-${A}` }]);
    });
  });

  it("forgets the user when the transaction ends", async () => {
    await asUser(web, A, async () => {
      expect(await count(web, "workouts")).toBe(1);
    });
    expect(await count(web, "workouts")).toBe(0);
  });

  it("rejects a setting that is not a UUID rather than reading anything", async () => {
    await web.query("BEGIN");
    try {
      await web.query("SELECT set_config('puls.user_id', 'everyone', true)");
      await expect(web.query("SELECT count(*) FROM workouts")).rejects.toMatchObject({ code: "22P02" });
    } finally {
      await web.query("ROLLBACK");
    }
  });

  it("cannot reach anything beneath the views", async () => {
    for (const table of [
      "public.users",
      "public.sources",
      "public.quantity_samples",
      "public.category_samples",
      "public.workouts",
      "public.workout_route_points",
      "public.workout_series_points",
      "public.activity_summaries",
      "public.aggregate_samples",
      "public.metric_daily",
      "public.quantity_rollups",
      "public.heartbeat_series",
      "public.ecg_samples",
      "public.state_of_mind",
      "public.medication_dose_events",
      "public.batches",
      "public.device_tokens",
    ]) {
      expect(await sqlState(web, `SELECT 1 FROM ${table} LIMIT 1`), table).toBe("42501");
    }

    // Chunks, compressed or not, are no side door.
    const { rows } = await admin.query<{ chunk: string }>(
      `SELECT format('%I.%I', chunk_schema, chunk_name) AS chunk FROM timescaledb_information.chunks
        WHERE hypertable_name IN ('quantity_samples', 'workout_series_points')`,
    );
    expect(rows.length).toBeGreaterThan(0);
    for (const { chunk } of rows) {
      expect(await sqlState(web, `SELECT 1 FROM ${chunk} LIMIT 1`), chunk).toBe("42501");
    }
  });

  it("cannot write health data, even through the views", async () => {
    await web.query("BEGIN");
    await web.query("SELECT set_config('puls.user_id', $1, true)", [A]);
    await expect(web.query("UPDATE users SET name = 'x'")).rejects.toMatchObject({ code: "42501" });
    await web.query("ROLLBACK");
    expect(await sqlState(web, "DELETE FROM workouts")).toBe("42501");
    expect(
      await sqlState(
        web,
        `INSERT INTO quantity_samples (uuid, type_id, start_ts, end_ts, user_id)
         VALUES (gen_random_uuid(), 1, now(), now(), $1)`,
        [A],
      ),
    ).toBe("42501");
  });

  // The viewer's real code over the views: lib/db.ts scoped() and every
  // health query in lib/queries.ts, connected as web_app. PULS_TIME_ZONE must
  // match the database's zone for the metric_daily path (CI sets both).
  describe("the viewer's queries", () => {
    type Queries = typeof import("./queries");
    let queries: Queries;

    beforeAll(async () => {
      process.env.DATABASE_URL = WEB_URL;
      queries = await import("./queries");
      expect((await queries.getDataSource()).source).toBe("live");
    });

    it("overrides a WHERE clause that names someone else", async () => {
      const { scoped } = await import("./db");
      const rows = await scoped(A, (q) => q("SELECT uuid FROM workouts WHERE user_id = $1::uuid", [B]));
      expect(rows).toEqual([]);
      const own = await scoped(A, (q) => q<{ n: string }>("SELECT count(*) AS n FROM workouts"));
      expect(Number(own[0].n)).toBe(1);
    });

    it("answers every page's questions for the scoped user", async () => {
      const stats = await queries.getStats(A);
      expect(stats.get(STEPS)?.rows).toBe(2);
      expect(stats.get(SLEEP)?.rows).toBe(1);
      expect(stats.get("HKWorkoutTypeIdentifier")?.rows).toBe(1);

      const workouts = await queries.getWorkouts(A);
      expect(workouts.map((w) => w.uuid)).toEqual([workoutIds[A]]);
      const detail = await queries.getWorkoutDetail(A, workoutIds[A]);
      expect(detail?.route).toHaveLength(3);
      expect(detail?.source).toBe(`it-watch-${A}`);
      expect(await queries.getWorkoutSeries(A, workoutIds[A])).toHaveLength(1);

      expect((await queries.getActivityRings(A)).hasData).toBe(true);
      expect((await queries.getLatestMany(A, [STEPS])).get(STEPS)?.value).toBe(200);
      const series = await queries.getSeries(A, STEPS, "ALL");
      expect(series.points.reduce((sum, p) => sum + p.value, 0)).toBe(300);
      const sleep = await queries.getSeries(A, SLEEP, "30D");
      expect(sleep.points.length).toBeGreaterThan(0);
      const sessions = await queries.getSleepSessions(A);
      expect(sessions).toHaveLength(1);
      expect(sessions[0].sources).toHaveLength(1);
      expect(sessions[0].sources[0].id).toBe(String(sourceIds[0]));
      expect(sessions[0].sources[0].minutes.core).toBeCloseTo(420, 0);
      const spark = await queries.getDailySparklines(A, [STEPS]);
      expect(spark.get(STEPS)?.length).toBeGreaterThan(0);
      expect((await queries.getProfile(A)).dob).toBeNull();
    });

    it("finds nothing of another user's, even by id", async () => {
      expect(await queries.getWorkoutDetail(A, workoutIds[B])).toBeNull();
      expect(await queries.getWorkoutSeries(A, workoutIds[B])).toEqual([]);
    });
  });

  it("keeps its own account store", async () => {
    await web.query(
      `INSERT INTO auth.accounts (user_id, email, password_hash) VALUES ($1, $2, 'scrypt$test')`,
      [A, `${A.slice(0, 8)}@example.com`],
    );
    const { rows } = await web.query<{ id: string }>("SELECT id FROM auth.accounts WHERE user_id = $1", [A]);
    expect(rows).toHaveLength(1);
    await web.query(
      `INSERT INTO auth.sessions (id, account_id, expires_at, purpose)
       VALUES (sha256('it-session'::bytea), $1, now() + interval '1 day', 'personal')`,
      [rows[0].id],
    );
    await web.query("DELETE FROM auth.accounts WHERE id = $1", [rows[0].id]);
    expect(await count(web, "auth.sessions", "id = sha256('it-session'::bytea)")).toBe(0);
    // An email is stored normalised or not at all.
    expect(
      await sqlState(web, "INSERT INTO auth.accounts (user_id, email) VALUES ($1, 'Mixed@Example.com')", [A]),
    ).toBe("23514");
  });
});
