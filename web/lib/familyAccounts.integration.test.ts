import { randomUUID } from "node:crypto";
import { spawnSync } from "node:child_process";
import { NextRequest } from "next/server";
import { Client } from "pg";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { requireIntegrationDatabases } from "./integrationEnv";

const { webUrl, adminUrl } = requireIntegrationDatabases();
const member = randomUUID();
const unclaimedMember = randomUUID();
const unclaimedWorkout = randomUUID();
const email = `${member}@synthetic.invalid`;
const unclaimedEmail = `${unclaimedMember}@synthetic.invalid`;
const admin = new Client({ connectionString: adminUrl });

describe.skipIf(!webUrl || !adminUrl)("family account ownership (integration)", () => {
  beforeAll(async () => {
    process.env.WEB_ACCOUNTS = "true";
    process.env.TRUST_PROXY_HEADERS = "true";
    process.env.DATABASE_URL = webUrl;
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
    await admin.query("DELETE FROM auth.invites WHERE user_id=$1", [member]);
    await admin.query("DELETE FROM auth.sessions WHERE account_id IN (SELECT id FROM auth.accounts WHERE user_id=$1)", [member]);
    await admin.query("DELETE FROM auth.accounts WHERE user_id=$1", [member]);
    await admin.query("DELETE FROM users WHERE id=$1", [member]);
    await admin.query("DELETE FROM workouts WHERE user_id=$1", [unclaimedMember]);
    await admin.query("DELETE FROM auth.invites WHERE user_id=$1", [unclaimedMember]);
    await admin.query("DELETE FROM auth.sessions WHERE account_id IN (SELECT id FROM auth.accounts WHERE user_id=$1)", [unclaimedMember]);
    await admin.query("DELETE FROM auth.accounts WHERE user_id=$1", [unclaimedMember]);
    await admin.query("DELETE FROM users WHERE id=$1", [unclaimedMember]);
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
      cliRefused: cli.status === 1 && cli.stderr.includes("already has an account"),
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
});
