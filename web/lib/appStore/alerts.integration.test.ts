// Scratch migrated database only, with the same explicit URLs as the other web suites.
// Email is mocked; this proves real row locking, rollback and scoped-role grants.
import { randomBytes } from "node:crypto";
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";
import { Client } from "pg";
import { requireIntegrationDatabases } from "../integrationEnv";
import { secretFixture } from "../secretFixture";
const mail = vi.hoisted(() => ({ send: vi.fn() }));
vi.mock("../email", () => ({ mailConfig: () => ({ from: "alerts@example.com" }), sendMail: mail.send }));
const dbs = requireIntegrationDatabases();
const databaseSecret = secretFixture({ DATABASE_URL: dbs.webUrl });

describe.skipIf(!dbs.available)("App Store delivery receipts in Postgres", () => {
  const key = randomBytes(32);
  let admin: Client;
  let deliver: typeof import("./alerts").deliverAlert;
  beforeAll(async () => {
    vi.stubEnv("DATABASE_URL_FILE", databaseSecret.DATABASE_URL_FILE);
    vi.stubEnv("WEB_APP_STORE_ALERT_TO", "operator@example.com");
    admin = new Client({ connectionString: dbs.adminUrl });
    await admin.connect();
    deliver = (await import("./alerts")).deliverAlert;
  });
  afterAll(async () => {
    if (admin) { await admin.query("DELETE FROM auth.app_store_alerts WHERE event_hash = $1", [key]); await admin.end(); }
    const pool = (await import("../db")).getPool();
    if (pool) await pool.end();
    vi.unstubAllEnvs();
  });
  it("rolls back failed delivery, retries once and serializes simultaneous notifications", async () => {
    const alert = { key, subject: "synthetic", text: "No real email" };
    mail.send.mockResolvedValueOnce(false);
    await expect(deliver(alert)).rejects.toThrow("send failed");
    expect((await admin.query("SELECT * FROM auth.app_store_alerts WHERE event_hash = $1", [key])).rowCount).toBe(0);
    mail.send.mockClear().mockResolvedValue(true);
    await Promise.all([deliver(alert), deliver(alert), deliver(alert)]);
    expect(mail.send).toHaveBeenCalledTimes(1);
    const result = await admin.query("SELECT sent_at FROM auth.app_store_alerts WHERE event_hash = $1", [key]);
    expect(result.rowCount).toBe(1);
    expect(result.rows[0].sent_at).not.toBeNull();
    for (const role of ["api_reader", "ingest", "grafana"]) {
      const privileges = await admin.query("SELECT has_table_privilege($1, 'auth.app_store_alerts', 'SELECT') AS allowed", [role]);
      expect(privileges.rows[0].allowed).toBe(false);
    }
  });
});
