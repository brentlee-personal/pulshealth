import { randomBytes, randomUUID } from "node:crypto";
import { Client } from "pg";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { requireIntegrationDatabases } from "./integrationEnv";
const { webUrl, adminUrl } = requireIntegrationDatabases();
const a = randomUUID();
const b = randomUUID();
const session = randomBytes(32);
describe.skipIf(!webUrl || !adminUrl)("reporting time zone ownership (integration)", () => {
  let admin: Client;
  let web: Client;
  beforeAll(async () => {
    admin = new Client({ connectionString: adminUrl });
    web = new Client({ connectionString: webUrl });
    await admin.connect(); await web.connect();
    await admin.query("INSERT INTO users (id, time_zone) VALUES ($1, 'Pacific/Auckland'), ($2, 'Asia/Tokyo')", [a, b]);
    const account = (await admin.query("INSERT INTO auth.accounts (user_id,email,password_hash) VALUES ($1,$2,'unused') RETURNING id", [a, `${a}@example.com`])).rows[0].id;
    await admin.query("INSERT INTO auth.sessions (id,account_id,expires_at,purpose) VALUES ($1,$2,now()+interval '1 hour','personal')", [session, account]);
  });
  afterAll(async () => {
    if (admin) {
      await admin.query("DELETE FROM auth.accounts WHERE user_id IN ($1,$2)", [a,b]);
      await admin.query("DELETE FROM users WHERE id IN ($1,$2)", [a,b]);
      await admin.end();
    }
    if (web) await web.end();
  });
  async function zone(user: string, target = user) {
    await web.query("BEGIN READ ONLY");
    try {
      await web.query("SELECT set_config('puls.user_id',$1,true)", [user]);
      return (await web.query("SELECT puls_user_time_zone($1::uuid) AS zone", [target])).rows[0].zone;
    } finally { await web.query("ROLLBACK"); }
  }
  it("reads each account calendar independently without exposing another account's zone", async () => {
    expect(await zone(a)).toBe("Pacific/Auckland");
    expect(await zone(b)).toBe("Asia/Tokyo");
    expect(await zone(a,b)).not.toBe("Asia/Tokyo");
  });
  it("changes only the session owner's calendar and rejects invalid zones and sessions", async () => {
    await web.query("SELECT auth.set_my_time_zone($1,$2)", [session,"Europe/Berlin"]);
    expect(await zone(a)).toBe("Europe/Berlin");
    expect(await zone(b)).toBe("Asia/Tokyo");
    await expect(web.query("SELECT auth.set_my_time_zone($1,$2)", [session,"Bogus/Zone"])).rejects.toThrow();
    await expect(web.query("SELECT auth.set_my_time_zone($1,$2)", [randomBytes(32),"UTC"])).rejects.toThrow();
    expect(await zone(a)).toBe("Europe/Berlin");
  });
  it("rejects expired sessions and disabled accounts", async () => {
    await admin.query("UPDATE auth.sessions SET expires_at=now()-interval '1 second' WHERE id=$1", [session]);
    await expect(web.query("SELECT auth.set_my_time_zone($1,$2)", [session,"UTC"])).rejects.toThrow();
    await admin.query("UPDATE auth.sessions SET expires_at=now()+interval '1 hour' WHERE id=$1", [session]);
    await admin.query("UPDATE auth.accounts SET disabled_at=now() WHERE user_id=$1", [a]);
    await expect(web.query("SELECT auth.set_my_time_zone($1,$2)", [session,"UTC"])).rejects.toThrow();
    expect(await zone(a)).toBe("Europe/Berlin");
  });
});
