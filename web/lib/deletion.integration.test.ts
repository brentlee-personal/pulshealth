import { randomUUID } from "node:crypto";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Client } from "pg";
import { beforeAll, afterAll, describe, expect, it } from "vitest";
import { requireIntegrationDatabases } from "./integrationEnv";
import { secretFixture } from "./secretFixture";

const { webUrl, adminUrl } = requireIntegrationDatabases();
const databaseSecret = secretFixture({ DATABASE_URL: webUrl });
describe.skipIf(!webUrl || !adminUrl)("automatic deletion (integration)", () => {
  let admin: Client;
  let directory: string;
  let deletion: typeof import("./accounts/deletion");
  let session: typeof import("./accounts/session");
  const users: string[] = [];
  let deletionJobs: { job_id: number; scheduled: boolean }[] = [];
  const saved = { ...process.env };

  beforeAll(async () => {
    process.env.WEB_ACCOUNTS = "true";
    delete process.env.DATABASE_URL;
    Object.assign(process.env, databaseSecret);
    directory = await mkdtemp(join(tmpdir(), "puls-erasure-integration-"));
    process.env.PULS_DELETION_LEDGER_DIR = directory;
    admin = new Client({ connectionString: adminUrl });
    await admin.connect();
    // These tests install FK guard tables and call the worker explicitly.
    // A scheduled copy can race the assertions or deadlock with guard-table
    // teardown. Pause only this job while fixtures exist, then restore it.
    deletionJobs = (await admin.query<{ job_id: number; scheduled: boolean }>(
      "SELECT job_id, scheduled FROM timescaledb_information.jobs WHERE proc_schema = 'auth' AND proc_name = 'process_account_deletions'",
    )).rows;
    for (const job of deletionJobs) {
      await admin.query("SELECT alter_job($1, scheduled => false)", [job.job_id]);
    }
    deletion = await import("./accounts/deletion");
    session = await import("./accounts/session");
  });
  afterAll(async () => {
    try {
      if (admin) {
        for (const user of users) {
          await admin.query("DELETE FROM auth.account_deletions WHERE user_id = $1", [user]);
          await admin.query("DELETE FROM auth.accounts WHERE user_id = $1", [user]);
          await admin.query("DELETE FROM users WHERE id = $1", [user]);
          await admin.query("DELETE FROM auth.deletion_tombstones WHERE user_id = $1", [user]);
        }
      }
    } finally {
      if (admin) {
        try {
          for (const job of deletionJobs) {
            await admin.query("SELECT alter_job($1, scheduled => $2)", [job.job_id, job.scheduled]);
          }
        } finally {
          await admin.end();
        }
      }
      await (await import("./db")).getPool()?.end();
      await (await import("./db")).getHealthPool()?.end();
      if (directory) await rm(directory, { recursive: true, force: true });
      process.env = saved;
    }
  });

  async function person(eligible = true, isAdmin = false) {
    const user = randomUUID();
    users.push(user);
    await admin.query("INSERT INTO users(id) VALUES ($1)", [user]);
    const account = (await admin.query<{ id: string }>(
      "INSERT INTO auth.accounts(user_id,email,password_hash,is_admin) VALUES ($1,$2,'test-hash',$3) RETURNING id::text",
      [user, `${user}@example.com`, isAdmin],
    )).rows[0].id;
    if (eligible) await admin.query("INSERT INTO auth.personal_users(user_id) VALUES ($1)", [user]);
    const token = await session.createSession(account, { userAgent: null, ip: null });
    return { user, account, token, hash: session.tokenHash(token)! };
  }

  it("deletes an explicitly personal invited account and leaves a private completed receipt", async () => {
    const p = await person();
    expect((await session.findSession(p.token))?.selfService).toBe(false);
    expect((await session.findSession(p.token))?.canDelete).toBe(true);
    const request = await deletion.requestDeletion(p.hash);
    expect(await session.findSession(p.token)).toBeNull();
    expect(await deletion.deletionStatus(request.receipt)).toBe("pending");
    const ledger = JSON.parse(await readFile(join(directory, `${p.user}.json`), "utf8"));
    expect(ledger.user_id).toBe(p.user);
    await deletion.completeDeletion(request.userId);
    expect(await deletion.deletionStatus(request.receipt)).toBe("completed");
    expect((await admin.query("SELECT 1 FROM users WHERE id=$1", [p.user])).rows).toHaveLength(0);
    expect((await admin.query("SELECT user_id FROM auth.account_deletions WHERE receipt_hash=$1", [session.tokenHash(request.receipt)])).rows[0].user_id).toBeNull();
    expect(await deletion.deletionStatus(session.newToken())).toBeNull();
  });

  it("erases compressed health samples while preserving another person in the same chunk", async () => {
    const p = await person();
    const other = await person();
    await admin.query("INSERT INTO sample_types(identifier,kind,unit) VALUES ('HKQuantityTypeIdentifierStepCount','quantity','count') ON CONFLICT DO NOTHING");
    await admin.query(`INSERT INTO quantity_samples(uuid,type_id,start_ts,end_ts,value,user_id)
      SELECT gen_random_uuid(), (SELECT type_id FROM sample_types WHERE identifier='HKQuantityTypeIdentifierStepCount'),
        '2001-01-01'::timestamptz + g * interval '1 minute', '2001-01-01'::timestamptz + g * interval '1 minute', g, $1
      FROM generate_series(1,2000) g`, [p.user]);
    await admin.query(`INSERT INTO quantity_samples(uuid,type_id,start_ts,end_ts,value,user_id)
      SELECT gen_random_uuid(), type_id, '2001-01-01'::timestamptz, '2001-01-01'::timestamptz, 7, $1
      FROM sample_types WHERE identifier='HKQuantityTypeIdentifierStepCount'`, [other.user]);
    await admin.query(`SELECT compress_chunk(format('%I.%I',chunk_schema,chunk_name)::regclass, true)
      FROM timescaledb_information.chunks WHERE hypertable_name='quantity_samples'
        AND range_start <= '2001-01-01'::timestamptz AND range_end > '2001-01-01'::timestamptz`);
    const request = await deletion.requestDeletion(p.hash);
    await deletion.completeDeletion(p.user);
    expect(await deletion.deletionStatus(request.receipt)).toBe("completed");
    expect((await admin.query("SELECT 1 FROM quantity_samples WHERE user_id=$1", [p.user])).rows).toHaveLength(0);
    expect((await admin.query("SELECT value FROM quantity_samples WHERE user_id=$1", [other.user])).rows).toEqual([{ value: 7 }]);
    await deletion.requestDeletion(other.hash);
    await deletion.completeDeletion(other.user);
  });

  it("does not accept deletion when its independent restore ledger is unavailable", async () => {
    const p = await person();
    delete process.env.PULS_DELETION_LEDGER_DIR;
    try {
      await expect(deletion.requestDeletion(p.hash)).rejects.toThrow("must be configured");
      expect(await session.findSession(p.token)).not.toBeNull();
      expect((await admin.query("SELECT 1 FROM auth.account_deletions WHERE user_id=$1", [p.user])).rows).toHaveLength(0);
    } finally {
      process.env.PULS_DELETION_LEDGER_DIR = directory;
    }
  });

  it("retries a failed purge automatically and reports completion only after every dependent row is gone", async () => {
    const p = await person();
    const guard = `deletion_guard_${p.user.replaceAll("-", "")}`;
    await admin.query(`CREATE TABLE public.${guard}(user_id uuid REFERENCES users(id))`);
    await admin.query(`INSERT INTO public.${guard} VALUES ($1)`, [p.user]);
    try {
      const request = await deletion.requestDeletion(p.hash);
      await expect(deletion.completeDeletion(p.user)).rejects.toMatchObject({ code: "23503" });
      await admin.query("CALL auth.process_account_deletions(NULL,$1::jsonb)", [JSON.stringify({ user_id: p.user })]);
      expect(await deletion.deletionStatus(request.receipt)).toBe("pending");
      expect((await admin.query("SELECT attempts,last_error_code FROM auth.account_deletions WHERE user_id=$1", [p.user])).rows[0])
        .toMatchObject({ attempts: 1, last_error_code: "23503" });
      await admin.query(`DROP TABLE public.${guard}`);
      await admin.query("CALL auth.process_account_deletions(NULL,$1::jsonb)", [JSON.stringify({ user_id: p.user })]);
      expect(await deletion.deletionStatus(request.receipt)).toBe("completed");
    } finally {
      await admin.query(`DROP TABLE IF EXISTS public.${guard}`);
    }
  });

  it("keeps household and administrator accounts protected even with forged eligibility", async () => {
    for (const p of [await person(false), await person(true, true)]) {
      expect((await session.findSession(p.token))?.canDelete).toBe(false);
      await expect(deletion.requestDeletion(p.hash)).rejects.toMatchObject({ code: "42501" });
      expect(await session.findSession(p.token)).not.toBeNull();
    }
    const { query } = await import("./db");
    await expect(query("SELECT auth.purge_user_data($1::uuid)", [users[0]])).rejects.toMatchObject({ code: "42501" });
    await expect(query("INSERT INTO auth.personal_users(user_id) VALUES ($1)", [users[0]])).rejects.toMatchObject({ code: "42501" });
    await expect(query("SELECT auth.replay_account_deletion($1::uuid,now())", [users[0]])).rejects.toMatchObject({ code: "42501" });
  });

  it("protects a custom deployment default even if explicitly marked personal", async () => {
    const p = await person();
    const previous = (await admin.query("SELECT default_user_id FROM auth.deletion_policy")).rows[0].default_user_id;
    try {
      await admin.query("UPDATE auth.deletion_policy SET default_user_id=$1", [p.user]);
      expect((await session.findSession(p.token))?.canDelete).toBe(false);
      await expect(deletion.requestDeletion(p.hash)).rejects.toMatchObject({ code: "42501" });
      await expect(admin.query("SELECT auth.replay_account_deletion($1,now())", [p.user])).rejects.toMatchObject({ code: "42501" });
    } finally {
      await admin.query("UPDATE auth.deletion_policy SET default_user_id=$1", [previous]);
    }
  });

  it("does not let twenty failed deletions starve the next valid request", async () => {
    const guard = `deletion_queue_guard_${randomUUID().replaceAll("-", "")}`;
    await admin.query(`CREATE TABLE public.${guard}(user_id uuid REFERENCES users(id))`);
    const blocked: string[] = [];
    try {
      for (let i = 0; i < 20; i++) {
        const p = await person();
        blocked.push(p.user);
        await admin.query(`INSERT INTO public.${guard} VALUES ($1)`, [p.user]);
        await deletion.requestDeletion(p.hash);
      }
      const valid = await person();
      const receipt = await deletion.requestDeletion(valid.hash);
      await admin.query("CALL auth.process_account_deletions(NULL,NULL)");
      await admin.query("CALL auth.process_account_deletions(NULL,NULL)");
      expect(await deletion.deletionStatus(receipt.receipt)).toBe("completed");
    } finally {
      await admin.query(`DROP TABLE public.${guard}`);
      for (const user of blocked) await deletion.completeDeletion(user);
    }
  });
});
