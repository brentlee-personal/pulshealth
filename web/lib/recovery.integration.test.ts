import { createHash, randomUUID } from "node:crypto";
import { Client } from "pg";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { requireIntegrationDatabases } from "./integrationEnv";
import { requestPasswordReset, completePasswordReset, usablePasswordReset } from "./accounts/recovery";
import { createSession, findSession, tokenHash } from "./accounts/session";
import { findAccountForLogin, finishPasswordLogin } from "./accounts/store";
const { webUrl, adminUrl } = requireIntegrationDatabases();
const user = randomUUID();
const email = `${user}@example.com`;
describe.skipIf(!webUrl || !adminUrl)("password recovery (integration)", () => {
  let admin: Client;
  let accountId: string;
  beforeAll(async () => {
    process.env.DATABASE_URL = webUrl;
    process.env.WEB_ACCOUNTS = "true";
    admin = new Client({ connectionString: adminUrl });
    await admin.connect();
    await admin.query("INSERT INTO users(id) VALUES ($1)", [user]);
    accountId = (await admin.query("INSERT INTO auth.accounts(user_id,email,password_hash,password_changed_at) VALUES ($1,$2,'original',now()) RETURNING id", [user,email])).rows[0].id;
  });
  afterAll(async () => {
    if (!admin) return;
    await admin.query("DELETE FROM auth.accounts WHERE id=$1", [accountId]);
    await admin.query("DELETE FROM users WHERE id=$1", [user]);
    await admin.end();
  });
  it("uses a token once under concurrent attempts, revokes sessions and rejects stale credentials", async () => {
    const reset = await requestPasswordReset(email, `test:${user}`);
    expect(reset).not.toBeNull();
    const session = await createSession(accountId, { ip: null, userAgent: null });
    const token = reset!.token;
    expect(await usablePasswordReset(token)).toBe(true);
    const stored = (await admin.query("SELECT token_hash FROM auth.password_resets WHERE account_id=$1", [accountId])).rows;
    expect(stored[0].token_hash).toEqual(tokenHash(token));
    const results = await Promise.all([completePasswordReset(token, "replacement"), completePasswordReset(token, "loser")]);
    expect(results.filter(Boolean)).toHaveLength(1);
    expect(await usablePasswordReset(token)).toBe(false);
    expect(await findSession(session)).toBeNull();
    const stale = await requestPasswordReset(email, `test:${user}`);
    await admin.query("UPDATE auth.accounts SET password_changed_at=clock_timestamp() WHERE id=$1", [accountId]);
    expect(await completePasswordReset(stale!.token, "stale")).toBe(false);
  });
  it("expires tokens and bounds requests per email across different IPs", async () => {
    const reset = await requestPasswordReset(email, `other:${user}`);
    expect(reset).not.toBeNull();
    await admin.query("UPDATE auth.password_resets SET expires_at=now()-interval '1 second' WHERE token_hash=$1", [tokenHash(reset!.token)]);
    expect(await completePasswordReset(reset!.token, "expired")).toBe(false);
    expect(await requestPasswordReset(email, `another:${user}`)).toBeNull();
  });
  it("never issues a link for unknown, disabled or stored legacy shared accounts", async () => {
    expect(await requestPasswordReset(`missing-${email}`, `test:${user}`)).toBeNull();
    await admin.query("DELETE FROM auth.password_reset_limits WHERE key=$1", [createHash("sha256").update(`email:${email}`).digest()]);
    await admin.query("UPDATE auth.accounts SET disabled_at=now() WHERE id=$1", [accountId]);
    expect(await requestPasswordReset(email, `test:${user}`)).toBeNull();
    await admin.query("UPDATE auth.accounts SET disabled_at=NULL WHERE id=$1", [accountId]);
    const account = await findAccountForLogin(email);
    if (!account) throw new Error("Synthetic personal account is unavailable");
    const reset = await requestPasswordReset(email, `test:${user}`);
    if (!reset) throw new Error("Synthetic personal reset was not issued");
    const previous = (await admin.query("SELECT demo_user_id FROM auth.device_pairing_policy WHERE singleton")).rows[0].demo_user_id;
    await admin.query("UPDATE auth.device_pairing_policy SET demo_user_id=$1 WHERE singleton", [user]);
    try {
      expect(await requestPasswordReset(email, `test:${user}`)).toBeNull();
      expect(await findAccountForLogin(email)).toBeNull();
      expect(await finishPasswordLogin(account, null, { ip: null, userAgent: null }, null)).toBeNull();
      expect(await usablePasswordReset(reset.token)).toBe(false);
      expect(await completePasswordReset(reset.token, "not-personal")).toBe(false);
      expect((await admin.query("SELECT password_hash FROM auth.accounts WHERE id=$1", [accountId])).rows[0].password_hash).toBe(account.passwordHash);
    } finally {
      await admin.query("UPDATE auth.device_pairing_policy SET demo_user_id=$1 WHERE singleton", [previous]);
    }
  });
});
