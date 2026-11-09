import { randomUUID } from "node:crypto";
import { NextRequest } from "next/server";
import { Client } from "pg";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { requireIntegrationDatabases } from "./integrationEnv";

const { webUrl, adminUrl } = requireIntegrationDatabases();
const member = randomUUID();
const email = `${member}@synthetic.invalid`;
const admin = new Client({ connectionString: adminUrl });

describe.skipIf(!webUrl || !adminUrl)("family account ownership (integration)", () => {
  beforeAll(async () => {
    process.env.WEB_ACCOUNTS = "true";
    process.env.DATABASE_URL = webUrl;
    await admin.connect();
    await admin.query("INSERT INTO users(id) VALUES($1)", [member]);
  });

  afterAll(async () => {
    await (await import("./db")).getPool()?.end();
    await (await import("./db")).getHealthPool()?.end();
    await admin.query("DELETE FROM auth.invites WHERE user_id=$1", [member]);
    await admin.query("DELETE FROM auth.sessions WHERE account_id IN (SELECT id FROM auth.accounts WHERE user_id=$1)", [member]);
    await admin.query("DELETE FROM auth.accounts WHERE user_id=$1", [member]);
    await admin.query("DELETE FROM users WHERE id=$1", [member]);
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
      method: "POST", body: form, headers: { origin: "https://viewer.synthetic.invalid" },
    }));
    const observed = await admin.query<{ password_preserved: boolean; browser_preserved: boolean }>(
      `SELECT password_hash=$2 AS password_preserved,
              EXISTS(SELECT 1 FROM auth.sessions WHERE id=$3 AND account_id=a.id) AS browser_preserved
         FROM auth.accounts a WHERE a.user_id=$1`,
      [member, originalHash, tokenHash(originalBrowser)],
    );
    const owner = observed.rows.at(0);
    if (!owner) throw new Error("Existing synthetic account disappeared");
    expect({
      welcomed: response.headers.get("location") === "/?notice=welcome",
      issuedBrowser: response.headers.has("set-cookie"),
      passwordPreserved: owner.password_preserved,
      browserPreserved: owner.browser_preserved,
    }).toEqual({ welcomed: false, issuedBrowser: false, passwordPreserved: true, browserPreserved: true });
  });
});
