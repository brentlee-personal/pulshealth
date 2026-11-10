import { randomUUID } from "node:crypto";
import { spawnSync } from "node:child_process";
import { NextRequest } from "next/server";
import { Client } from "pg";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { requireIntegrationDatabases } from "./integrationEnv";
import { secretFixture } from "./secretFixture";

const { webUrl, adminUrl } = requireIntegrationDatabases();
const databaseSecret = secretFixture({ DATABASE_URL: webUrl });
const member = randomUUID();
const unclaimedMember = randomUUID();
const unclaimedWorkout = randomUUID();
const email = `${member}@synthetic.invalid`;
const unclaimedEmail = `${unclaimedMember}@synthetic.invalid`;
const newEmail = `${randomUUID()}@synthetic.invalid`;
const createdMembers: string[] = [];
const admin = new Client({ connectionString: adminUrl });

describe.skipIf(!webUrl || !adminUrl)("family account ownership (integration)", () => {
  beforeAll(async () => {
    process.env.WEB_ACCOUNTS = "true";
    process.env.TRUST_PROXY_HEADERS = "true";
    delete process.env.DATABASE_URL;
    Object.assign(process.env, databaseSecret);
    await admin.connect();
    await admin.query("INSERT INTO users(id) VALUES($1)", [member]);
    await admin.query("INSERT INTO users(id) VALUES($1)", [unclaimedMember]);
    await admin.query(
      "INSERT INTO workouts(uuid,activity_type,start_ts,end_ts,user_id) VALUES($1,'running',now()-interval '1 hour',now(),$2)",
      [unclaimedWorkout, unclaimedMember],
    );
  });

  afterAll(async () => {
    await (await import("./db")).getPool()?.end();
    await (await import("./db")).getHealthPool()?.end();
    for (const id of [member, unclaimedMember, ...createdMembers]) {
      await admin.query("DELETE FROM workouts WHERE user_id=$1", [id]);
      await admin.query("DELETE FROM auth.invites WHERE user_id=$1", [id]);
      await admin.query("DELETE FROM auth.sessions WHERE account_id IN (SELECT id FROM auth.accounts WHERE user_id=$1)", [id]);
      await admin.query("DELETE FROM auth.accounts WHERE user_id=$1", [id]);
      await admin.query("DELETE FROM users WHERE id=$1", [id]);
    }
    await admin.end();
  });

  it("an invitation cannot replace an existing member's password or browser", async () => {
    const { hashPassword } = await import("./accounts/password");
    const { createSession, newToken, tokenHash } = await import("./accounts/session");
    const originalHash = await hashPassword("synthetic original member passphrase");
    const inserted = await admin.query<{ id: string }>(
      "INSERT INTO auth.accounts(user_id,email,password_hash) VALUES($1,$2,$3) RETURNING id::text",
      [member, email, originalHash],
    );
    const account = inserted.rows.at(0);
    if (!account) throw new Error("Synthetic account fixture was not created");
    const originalBrowser = await createSession(account.id, { userAgent: "synthetic browser", ip: "198.51.100.229" });
    const invitation = newToken();
    await admin.query(
      "INSERT INTO auth.invites(token_hash,user_id,email,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')",
      [tokenHash(invitation), member, email],
    );
    const form = new FormData();
    form.set("token", invitation);
    form.set("password", "synthetic replacement passphrase");
    form.set("confirm", "synthetic replacement passphrase");
    const { POST } = await import("@/app/api/auth/invite/route");
    const response = await POST(new NextRequest("https://viewer.synthetic.invalid/api/auth/invite", {
      method: "POST", body: form, headers: {
        host: "viewer.synthetic.invalid", origin: "https://viewer.synthetic.invalid",
        "x-forwarded-proto": "https", "x-forwarded-for": "198.51.100.229",
      },
    }));
    const observed = await admin.query<{ password_preserved: boolean; browser_preserved: boolean }>(
      `SELECT password_hash=$2 AS password_preserved,
              EXISTS(SELECT 1 FROM auth.sessions WHERE id=$3 AND account_id=a.id) AS browser_preserved
         FROM auth.accounts a WHERE a.user_id=$1`,
      [member, originalHash, tokenHash(originalBrowser)],
    );
    const owner = observed.rows.at(0);
    if (!owner) throw new Error("Existing synthetic account disappeared");
    const cli = spawnSync(process.execPath, ["scripts/invite.mjs", "--user", member, "--email", email], {
      cwd: process.cwd(), env: process.env, encoding: "utf8", timeout: 10_000,
    });
    expect({
      refused: response.headers.get("location") === `/invite/${invitation}?error=invalid`,
      issuedBrowser: /__Host-puls-session=[^;\s]+/.test(response.headers.get("set-cookie") ?? ""),
      passwordPreserved: owner.password_preserved,
      browserPreserved: owner.browser_preserved,
      cliRefused: cli.status === 2 && cli.stderr.includes("--user"),
      cliPrintedLink: cli.stdout.includes("/invite/"),
    }).toEqual({ refused: true, issuedBrowser: false, passwordPreserved: true, browserPreserved: true,
      cliRefused: true, cliPrintedLink: false });
  });

  it("an operator invitation cannot claim an existing member's health data before account creation", async () => {
    const cli = spawnSync(process.execPath, ["scripts/invite.mjs", "--user", unclaimedMember, "--email", unclaimedEmail], {
      cwd: process.cwd(), env: process.env, encoding: "utf8", timeout: 10_000,
    });
    const token = /\/invite\/([A-Za-z0-9_-]{43})(?:\s|$)/.exec(cli.stdout)?.at(1);
    let exposed = false;
    if (token) {
      const form = new FormData();
      form.set("token", token);
      form.set("password", "synthetic unclaimed member passphrase");
      form.set("confirm", "synthetic unclaimed member passphrase");
      const { POST } = await import("@/app/api/auth/invite/route");
      const response = await POST(new NextRequest("https://viewer.synthetic.invalid/api/auth/invite", {
        method: "POST", body: form, headers: {
          host: "viewer.synthetic.invalid", origin: "https://viewer.synthetic.invalid",
          "x-forwarded-proto": "https", "x-forwarded-for": "198.51.100.230",
        },
      }));
      const cookie = /__Host-puls-session=([^;\s]+)/.exec(response.headers.get("set-cookie") ?? "")?.at(1);
      const { findSession } = await import("./accounts/session");
      const signedIn = cookie ? await findSession(cookie) : null;
      if (signedIn) {
        const { scoped } = await import("./db");
        const rows = await scoped(signedIn.userId, (q) => q<{ count: number }>(
          "SELECT count(*)::int AS count FROM workouts WHERE uuid=$1", [unclaimedWorkout],
        ));
        exposed = rows.at(0)?.count === 1;
      }
    }
    expect(exposed, "an invitation must not expose a pre-existing member's workout").toBe(false);
    expect({ rejected: cli.status === 2, issuedLink: Boolean(token), exposed }).toEqual({
      rejected: true, issuedLink: false, exposed: false,
    });
  });

  it("the invitation tool allocates a fresh member, signs it in and leaves old data sealed", async () => {
    const cli = spawnSync(process.execPath, ["scripts/invite.mjs", "--email", newEmail], {
      cwd: process.cwd(), env: process.env, encoding: "utf8", timeout: 10_000,
    });
    const pending = await admin.query<{ user_id: string; self_service: boolean; auto_zone: boolean }>(
      `SELECT i.user_id::text, EXISTS(SELECT 1 FROM auth.self_service_users s WHERE s.user_id=i.user_id) AS self_service,
              u.time_zone_auto_initialize AS auto_zone
         FROM auth.invites i JOIN public.users u ON u.id=i.user_id WHERE i.email=$1`, [newEmail],
    );
    for (const row of pending.rows) createdMembers.push(row.user_id);
    expect(cli.status).toBe(0);
    expect(pending.rows).toHaveLength(1);
    const identity = pending.rows.at(0);
    if (!identity) throw new Error("New synthetic member was not created");
    expect([member, unclaimedMember]).not.toContain(identity.user_id);
    expect(identity.self_service).toBe(true);
    expect(identity.auto_zone).toBe(true);
    const token = /\/invite\/([A-Za-z0-9_-]{43})(?:\s|$)/.exec(cli.stdout)?.at(1);
    if (!token) throw new Error("New member invitation link was not returned");
    const { acceptInvite } = await import("./accounts/store");
    const { hashPassword } = await import("./accounts/password");
    const accepted = await acceptInvite(token, await hashPassword("synthetic fresh member passphrase"), { userAgent: null, ip: null }, null);
    expect(accepted.ok).toBe(true);
    if (!accepted.ok) return;
    const { findSession } = await import("./accounts/session");
    const signedIn = await findSession(accepted.sessionToken);
    expect(signedIn?.userId).toBe(identity.user_id);
    const { scoped } = await import("./db");
    const visible = await scoped(identity.user_id, (q) => q<{ count: number }>("SELECT count(*)::int AS count FROM workouts"));
    expect(visible.at(0)?.count).toBe(0);
    const preserved = await admin.query<{ count: number }>("SELECT count(*)::int AS count FROM workouts WHERE uuid=$1 AND user_id=$2", [unclaimedWorkout, unclaimedMember]);
    expect(preserved.rows.at(0)?.count).toBe(1);
  });

  it("rejects invalid new-member input without leaving a partial identity", async () => {
    const { query } = await import("./db");
    const { tokenHash, newToken } = await import("./accounts/session");
    const duplicate = tokenHash(newToken());
    const before = await admin.query<{ count: string }>("SELECT count(*) AS count FROM public.users");
    for (const [hash, address, adminFlag, hours] of [
      [null, newEmail, false, 48], [Buffer.alloc(1), newEmail, false, 48],
      [duplicate, "UPPER@synthetic.invalid", false, 48], [duplicate, "invalid", false, 48],
      [duplicate, newEmail, null, 48], [duplicate, newEmail, false, 0], [duplicate, newEmail, false, 337],
    ]) {
      await expect(query("SELECT * FROM auth.invite_new_member($1,$2,$3,$4)", [hash, address, adminFlag, hours]))
        .rejects.toMatchObject({ code: "22023" });
    }
    await expect(query("SELECT * FROM auth.invite_new_member($1,$2,false,48)", [duplicate, email]))
      .rejects.toMatchObject({ code: "23505" });
    const after = await admin.query<{ count: string }>("SELECT count(*) AS count FROM public.users");
    expect(after.rows).toEqual(before.rows);
  });
});
