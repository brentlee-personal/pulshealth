// Sign-up with approval against a real, migrated database (016_web_signups.sql),
// through the viewer's own route handlers and store, connected as web_app:
// a request creates nothing; only an administrator's session can approve it;
// approval makes a user and an invite; the person connects their own iPhone
// and nobody else's; disabling cuts the phone off; deleting an account and
// purging a user leave nothing behind, rollups included; web_app still cannot
// write the tables those functions write; and even SQL run as web_app with a
// session it forged cannot disable or purge household users; no administrator's account can be disabled
// through the viewer; a session past the 90-day cap does nothing; requests
// are declined in bulk, kept on the waitlist however long undecided
// (020_waitlist_retention.sql), listed oldest first, and dropped quietly past
// the waitlist's daily intake or ceiling. Skipped without the two connection
// strings.

import { createHash, randomUUID } from "node:crypto";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { NextRequest } from "next/server";
import { Client, DatabaseError } from "pg";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { requireIntegrationDatabases } from "./integrationEnv";

// In CI the job sets both; a missing one there must fail, not skip quietly
// (PULS_CI_REQUIRE_INTEGRATION / PULS_WEB_INTEGRATION, lib/integrationEnv.ts).
const { webUrl: WEB_URL, adminUrl: ADMIN_URL } = requireIntegrationDatabases();

const ADMIN_USER = randomUUID();
const tag = ADMIN_USER.slice(0, 8);
const ADMIN_EMAIL = `admin-${tag}@example.com`;
const PERSON_EMAIL = `person-${tag}@example.com`;
const PASSWORD = "a long enough passphrase";
const COOKIE = "__Host-puls-session";

function post(path: string, fields: Record<string, string>, opts: { cookie?: string; ip?: string } = {}) {
  const body = new FormData();
  for (const [k, v] of Object.entries(fields)) body.set(k, v);
  const headers = new Headers({
    host: "viewer.example",
    origin: "https://viewer.example",
    "x-forwarded-proto": "https",
    "x-forwarded-for": opts.ip ?? "198.51.100.1",
  });
  if (opts.cookie) headers.set("cookie", `${COOKIE}=${opts.cookie}`);
  return new NextRequest(new URL(path, "http://web:3000"), { method: "POST", headers, body });
}

const cookieOf = (res: Response) => /__Host-puls-session=([^;]*)/.exec(res.headers.get("set-cookie") ?? "")?.[1] || undefined;

async function sqlState(p: Promise<unknown>): Promise<string | null> {
  try {
    await p;
    return null;
  } catch (e) {
    return e instanceof DatabaseError ? (e.code ?? "unknown") : `not a database error: ${String(e)}`;
  }
}

describe.skipIf(!WEB_URL || !ADMIN_URL)("sign-up with approval (integration)", () => {
  let admin: Client;
  let ledgerDirectory: string;
  let adminCookie: string;
  let adminSession: Buffer;
  let personCookie: string;
  let personSession: Buffer;
  let tokenHash: typeof import("./accounts/session").tokenHash;
  const createdSources: number[] = [];
  let personUser: string;
  let signups: typeof import("./accounts/signups");
  let loginRoute: typeof import("@/app/api/auth/login/route");
  let inviteRoute: typeof import("@/app/api/auth/invite/route");
  let signupRoute: typeof import("@/app/api/auth/signup/route");
  let adminRoute: typeof import("@/app/api/admin/route");

  beforeAll(async () => {
    ledgerDirectory = await mkdtemp(join(tmpdir(), "puls-deletion-ledger-"));
    process.env.PULS_DELETION_LEDGER_DIR = ledgerDirectory;
    process.env.WEB_ACCOUNTS = "true";
    process.env.WEB_SIGNUPS = "true";
    process.env.TRUST_PROXY_HEADERS = "true";
    process.env.DATABASE_URL = WEB_URL;
    admin = new Client({ connectionString: ADMIN_URL });
    await admin.connect();
    const { hashPassword } = await import("./accounts/password");
    await admin.query("INSERT INTO users (id) VALUES ($1)", [ADMIN_USER]);
    await admin.query("INSERT INTO auth.accounts (user_id, email, password_hash, is_admin) VALUES ($1, $2, $3, true)", [
      ADMIN_USER, ADMIN_EMAIL, await hashPassword(PASSWORD),
    ]);
    signups = await import("./accounts/signups");
    tokenHash = (await import("./accounts/session")).tokenHash;
    loginRoute = await import("@/app/api/auth/login/route");
    inviteRoute = await import("@/app/api/auth/invite/route");
    signupRoute = await import("@/app/api/auth/signup/route");
    adminRoute = await import("@/app/api/admin/route");
    adminCookie = cookieOf(await loginRoute.POST(post("/api/auth/login", { email: ADMIN_EMAIL, password: PASSWORD }, { ip: "198.51.100.200" })))!;
    expect(adminCookie).toBeTruthy();
    adminSession = tokenHash(adminCookie)!;
  }, 60_000);

  afterAll(async () => {
    if (ledgerDirectory) await rm(ledgerDirectory, { recursive: true, force: true });
    await (await import("./db")).getPool()?.end();
    await (await import("./db")).getHealthPool()?.end();
    if (!admin) return;
    const users = (await admin.query<{ id: string }>(
      `SELECT user_id::text AS id FROM auth.signup_requests WHERE email LIKE $1 AND user_id IS NOT NULL
       UNION SELECT user_id::text FROM auth.invites WHERE email LIKE $1`, [`%-${tag}@example.com`],
    )).rows.map((r) => r.id);
    for (const user of [...users, ADMIN_USER]) {
      await admin.query("DELETE FROM workouts WHERE user_id = $1", [user]);
      await admin.query("DELETE FROM device_tokens WHERE user_id = $1", [user]);
      await admin.query("DELETE FROM auth.invites WHERE user_id = $1", [user]);
      await admin.query("DELETE FROM auth.signup_requests WHERE user_id = $1", [user]);
      await admin.query("DELETE FROM auth.sessions WHERE account_id IN (SELECT id FROM auth.accounts WHERE user_id = $1)", [user]);
      await admin.query("DELETE FROM auth.accounts WHERE user_id = $1", [user]);
      await admin.query("DELETE FROM quantity_samples WHERE user_id = $1 AND start_ts > now() - interval '400 days'", [user]);
      await admin.query("DELETE FROM users WHERE id = $1", [user]);
    }
    await admin.query("DELETE FROM auth.signup_requests WHERE email LIKE $1", [`%-${tag}@example.com`]);
    if (createdSources.length) await admin.query("DELETE FROM sources WHERE source_id = ANY($1::smallint[])", [createdSources]);
    await admin.end();
  }, 60_000);

  const requestRow = (email: string) =>
    admin.query<{ id: string; status: string; user_id: string | null }>(
      "SELECT id::text, status, user_id::text FROM auth.signup_requests WHERE email = $1 ORDER BY created_at", [email],
    ).then((r) => r.rows);

  it("records a request and creates nothing else", async () => {
    const res = await signupRoute.POST(post("/api/auth/signup", { name: "Pat", email: ` ${PERSON_EMAIL.toUpperCase()} `, note: "hi", consent: "yes" }, { ip: "198.51.100.10" }));
    expect(res.headers.get("location")).toBe("/signup?notice=received");
    expect(await requestRow(PERSON_EMAIL)).toMatchObject([{ status: "pending", user_id: null }]);
    expect((await admin.query("SELECT 1 FROM auth.accounts WHERE email = $1", [PERSON_EMAIL])).rows).toHaveLength(0);
    expect((await admin.query("SELECT 1 FROM auth.invites WHERE email = $1", [PERSON_EMAIL])).rows).toHaveLength(0);

    // Asking again while pending changes nothing, and answers the same.
    const again = await signupRoute.POST(post("/api/auth/signup", { name: "Pat", email: PERSON_EMAIL, consent: "yes" }, { ip: "198.51.100.11" }));
    expect(again.headers.get("location")).toBe("/signup?notice=received");
    expect(await requestRow(PERSON_EMAIL)).toHaveLength(1);
  });

  it("drops a honeypot, insists on consent, and limits one address to three an hour", async () => {
    const bot = `bot-${tag}@example.com`;
    const res = await signupRoute.POST(post("/api/auth/signup", { email: bot, consent: "yes", website: "http://spam" }, { ip: "198.51.100.20" }));
    expect(res.headers.get("location")).toBe("/signup?notice=received");
    expect(await requestRow(bot)).toHaveLength(0);
    expect((await signupRoute.POST(post("/api/auth/signup", { email: bot }, { ip: "198.51.100.21" }))).headers.get("location")).toBe("/signup?error=consent");

    const outcomes = [];
    for (let i = 0; i < 4; i++) {
      const r = await signupRoute.POST(post("/api/auth/signup", { email: `flood${i}-${tag}@example.com`, consent: "yes" }, { ip: "198.51.100.30" }));
      outcomes.push(r.headers.get("location"));
    }
    expect(outcomes.slice(0, 3)).toEqual(Array(3).fill("/signup?notice=received"));
    expect(outcomes[3]).toBe("/signup?error=throttled");
  });

  it("lets only an administrator's session approve, and approval makes a user and an invite", async () => {
    const [{ id }] = await requestRow(PERSON_EMAIL);
    // A made-up session, and the plaintext cookie instead of its hash, are refused.
    expect(await sqlState(signups.approveSignup(Buffer.alloc(32, 7), "00000000-0000-4000-8000-000000000000", id))).toBe("42501");
    expect(await sqlState(signups.approveSignup(Buffer.from(adminCookie), ADMIN_USER, id))).toBe("42501");

    const approval = await signups.approveSignup(adminSession, (await admin.query("SELECT id::text FROM auth.accounts WHERE email = $1", [ADMIN_EMAIL])).rows[0].id, id);
    personUser = approval.userId;
    expect(approval.email).toBe(PERSON_EMAIL);
    expect(await requestRow(PERSON_EMAIL)).toMatchObject([{ status: "approved", user_id: personUser }]);
    expect((await admin.query("SELECT 1 FROM users WHERE id = $1", [personUser])).rows).toHaveLength(1);
    expect((await admin.query("SELECT 1 FROM auth.self_service_users WHERE user_id = $1", [personUser])).rows).toHaveLength(1);
    expect(await sqlState(signups.approveSignup(adminSession, ADMIN_USER, id))).toBe("P0002"); // decided already

    // The invite works like any other: the person chooses a password and is in.
    const accepted = await inviteRoute.POST(post("/api/auth/invite", { token: approval.inviteToken, password: PASSWORD, confirm: PASSWORD }, { ip: "198.51.100.40" }));
    expect(accepted.headers.get("location")).toBe("/?notice=welcome");
    personCookie = cookieOf(accepted)!;
    expect(personCookie).toBeTruthy();
    personSession = tokenHash(personCookie)!;
  });

  it("connects the person's own iPhone, and nobody else's", async () => {
    const minted = await signups.issueDeviceToken(personSession, "Pat's iPhone");
    expect(minted.token).toMatch(/^[0-9a-f]{64}$/);
    const { rows } = await admin.query<{ user_id: string; token_hash: Buffer; token_prefix: string; status: string }>(
      "SELECT user_id::text, token_hash, token_prefix, status FROM device_tokens WHERE id = $1", [minted.id],
    );
    // Exactly what ingest looks up: SHA-256 over the ASCII hex, bound to the user.
    expect(rows[0]).toMatchObject({ user_id: personUser, token_prefix: minted.token.slice(0, 8), status: "active" });
    expect(rows[0].token_hash.equals(createHash("sha256").update(minted.token, "ascii").digest())).toBe(true);

    expect((await signups.myDevices(personSession)).map((d) => d.id)).toEqual([minted.id]);
    // An invited administrator connects their own phone too, but cannot
    // see or revoke another person's token.
    const own = await signups.issueDeviceToken(adminSession, "Administrator's iPhone");
    expect((await signups.myDevices(adminSession)).map((d) => d.id)).toEqual([own.id]);
    expect(await signups.revokeMyDevice(adminSession, minted.id)).toBe(false);
    expect(await signups.revokeMyDevice(personSession, own.id)).toBe(false);
    expect(await signups.revokeMyDevice(adminSession, own.id)).toBe(true);
    expect(await sqlState(signups.issueDeviceToken(Buffer.alloc(32, 9), "x"))).toBe("42501");

    const link = signups.pairingLink("https://ingest.example/", minted.token, personUser);
    expect(link).toBe(`puls://pair?url=https%3A%2F%2Fingest.example%2F&token=${minted.token}&user=${personUser}`);

    expect(await signups.revokeMyDevice(personSession, minted.id)).toBe(true);
    expect((await admin.query("SELECT status FROM device_tokens WHERE id = $1", [minted.id])).rows[0].status).toBe("revoked");
  });

  it("returns a session-scoped pairing callback directly to the app", async () => {
    process.env.WEB_INGEST_URL = "https://ingest.example.test";
    const { POST } = await import("@/app/api/auth/connect-iphone/route");
    // Caller-supplied identities cannot change whose token is issued.
    const response = await POST(post("/api/auth/connect-iphone", { name: "Connection flow", user: personUser }, { cookie: adminCookie }));
    expect(response.status).toBe(303);
    expect(response.headers.get("cache-control")).toBe("no-store");
    const link = new URL(response.headers.get("location")!);
    expect(link.protocol).toBe("puls:");
    expect(link.hostname).toBe("pair");
    expect(link.searchParams.get("user")).toBe(ADMIN_USER);
    expect(link.searchParams.get("url")).toBe("https://ingest.example.test");
    const token = link.searchParams.get("token")!;
    const row = (await admin.query("SELECT id, user_id::text FROM device_tokens WHERE token_hash = $1", [createHash("sha256").update(token, "ascii").digest()])).rows[0];
    expect(row.user_id).toBe(ADMIN_USER);
    expect(await signups.revokeMyDevice(adminSession, row.id)).toBe(true);
  });

  it("refuses a session signed in more than 90 days ago, in the database too", async () => {
    // The viewer already refuses it (findSession); the definer functions
    // check the same absolute cap themselves (auth.session_owner, 018). A
    // session of the person's own, still inside its sliding 30 days.
    const old = createHash("sha256").update(`old-${tag}`).digest();
    await admin.query(
      `INSERT INTO auth.sessions (id, account_id, created_at, expires_at)
       SELECT $1, id, now() - interval '91 days', now() + interval '1 day' FROM auth.accounts WHERE user_id = $2`,
      [old, personUser],
    );
    try {
      expect(await sqlState(signups.issueDeviceToken(old, "too old"))).toBe("42501");
      expect(await signups.myDevices(old)).toEqual([]);
      expect(await signups.myDevices(personSession)).not.toEqual([]);
    } finally {
      await admin.query("DELETE FROM auth.sessions WHERE id = $1", [old]);
    }
  });

  it("still cannot write users or device tokens directly", async () => {
    const web = new Client({ connectionString: WEB_URL });
    await web.connect();
    try {
      expect(await sqlState(web.query("INSERT INTO public.users (id) VALUES (gen_random_uuid())"))).toBe("42501");
      expect(await sqlState(web.query("SELECT 1 FROM public.device_tokens"))).toBe("42501");
      expect(await sqlState(web.query("SELECT * FROM auth.session_owner('\\x00'::bytea)"))).toBe("42501");
      expect(await sqlState(web.query("INSERT INTO auth.self_service_users (user_id) VALUES ($1)", [ADMIN_USER]))).toBe("42501");
      expect(await sqlState(web.query("DELETE FROM auth.self_service_users"))).toBe("42501");
    } finally {
      await web.end();
    }
  });

  it("keeps the operator's household out of reach of SQL run as web_app, even with a forged session", async () => {
    // web_app writes auth.sessions to sign people in, so it can mint a session
    // for any account. That must not open the household (users that did not
    // come from an approved sign-up) to anything beyond reading.
    const household = randomUUID();
    await admin.query("INSERT INTO users (id) VALUES ($1)", [household]);
    const web = new Client({ connectionString: WEB_URL });
    await web.connect();
    try {
      const forged = createHash("sha256").update(`forged-${tag}`).digest();
      const adminAccount = (await admin.query("SELECT id::text FROM auth.accounts WHERE email = $1", [ADMIN_EMAIL])).rows[0].id;
      await web.query("INSERT INTO auth.sessions (id, account_id, expires_at) VALUES ($1, $2, now() + interval '1 hour')", [forged, adminAccount]);
      const householdAccount = (await web.query<{ id: string }>(
        "INSERT INTO auth.accounts (user_id, email, password_hash) VALUES ($1, $2, 'x') RETURNING id::text", [household, `household-${tag}@example.com`],
      )).rows[0].id;
      const forgedHousehold = createHash("sha256").update(`forged-household-${tag}`).digest();
      await web.query("INSERT INTO auth.sessions (id, account_id, expires_at) VALUES ($1, $2, now() + interval '1 hour')", [forgedHousehold, householdAccount]);

      // An administrator's session (forged or real) cannot touch a household user...
      for (const session of [forged, adminSession]) {
        expect(await sqlState(web.query("SELECT auth.purge_user($1, $2::uuid)", [session, household]))).toBe("42501");
        expect(await sqlState(web.query("SELECT auth.purge_user($1, $2::uuid)", [session, ADMIN_USER]))).toBe("42501");
        expect(await sqlState(web.query("SELECT auth.set_account_disabled($1, $2::uuid, true)", [session, householdAccount]))).toBe("P0002");
        expect(await sqlState(web.query("SELECT auth.set_account_disabled($1, $2::uuid, true)", [session, adminAccount]))).toBe("P0002");
      }
      // ...and an invited personal account can pair itself, but cannot delete itself here.
      const hash = createHash("sha256").update("f".repeat(64), "ascii").digest();
      await web.query("SELECT auth.issue_device_token($1, $2, 'ffffffff', 'x')", [forgedHousehold, hash]);
      expect(await sqlState(web.query("SELECT auth.delete_my_account($1)", [forgedHousehold]))).toBe("42501");
      const phones = (await web.query("SELECT * FROM auth.my_devices($1)", [forgedHousehold])).rows;
      expect(phones).toHaveLength(1);
      expect((await admin.query("SELECT 1 FROM device_tokens WHERE user_id = $1", [household])).rows).toHaveLength(1);
      // The configured public demo remains view-only even through forged SQL.
      const previous = (await admin.query("SELECT demo_user_id FROM auth.device_pairing_policy")).rows[0].demo_user_id;
      await admin.query("UPDATE auth.device_pairing_policy SET demo_user_id = $1", [household]);
      try {
        expect(await sqlState(signups.issueDeviceToken(forgedHousehold, "blocked"))).toBe("42501");
        expect(await signups.myDevices(forgedHousehold)).toEqual([]);
        expect(await sqlState(signups.revokeMyDevice(forgedHousehold, phones[0].id))).toBe("42501");
        expect(await sqlState(web.query("UPDATE auth.device_pairing_policy SET demo_user_id = NULL"))).toBe("42501");
        expect(await sqlState(web.query("DELETE FROM auth.device_pairing_policy"))).toBe("42501");
      } finally {
        await admin.query("UPDATE auth.device_pairing_policy SET demo_user_id = $1", [previous]);
      }
      expect((await admin.query("SELECT disabled_at FROM auth.accounts WHERE id = $1", [adminAccount])).rows[0].disabled_at).toBeNull();
    } finally {
      await web.end();
      await admin.query("DELETE FROM auth.sessions WHERE account_id IN (SELECT id FROM auth.accounts WHERE user_id = $1)", [household]);
      await admin.query("DELETE FROM auth.sessions WHERE id = $1", [createHash("sha256").update(`forged-${tag}`).digest()]);
      await admin.query("DELETE FROM device_tokens WHERE user_id = $1", [household]);
      await admin.query("DELETE FROM auth.accounts WHERE user_id = $1", [household]);
      await admin.query("DELETE FROM users WHERE id = $1", [household]);
    }
  });

  it("disabling cuts the account off and its phones with it; enabling lets it sign in again", async () => {
    const minted = await signups.issueDeviceToken(personSession, "second");
    const accountId = (await admin.query("SELECT id::text FROM auth.accounts WHERE email = $1", [PERSON_EMAIL])).rows[0].id;
    expect(await sqlState(signups.setAccountDisabled(personSession, accountId, true))).toBe("42501");

    await signups.setAccountDisabled(adminSession, accountId, true);
    expect((await admin.query("SELECT status FROM device_tokens WHERE id = $1", [minted.id])).rows[0].status).toBe("revoked");
    const { findSession } = await import("./accounts/session");
    expect(await findSession(personCookie)).toBeNull();
    const refused = await loginRoute.POST(post("/api/auth/login", { email: PERSON_EMAIL, password: PASSWORD }, { ip: "198.51.100.50" }));
    expect(refused.headers.get("location")).toBe("/login?error=invalid");
    // The old session cannot mint a token for the disabled account.
    expect(await sqlState(signups.issueDeviceToken(personSession, "too late"))).toBe("42501");

    await signups.setAccountDisabled(adminSession, accountId, false);
    personCookie = cookieOf(await loginRoute.POST(post("/api/auth/login", { email: PERSON_EMAIL, password: PASSWORD }, { ip: "198.51.100.51" })))!;
    expect(personCookie).toBeTruthy();
    personSession = tokenHash(personCookie)!;
  });

  it("caps active sync tokens at ten, even when minted at once", async () => {
    const results = await Promise.allSettled(Array.from({ length: 14 }, (_, i) => signups.issueDeviceToken(personSession, `phone ${i}`)));
    expect(results.filter((r) => r.status === "fulfilled")).toHaveLength(10);
    for (const r of results) if (r.status === "rejected") expect((r.reason as DatabaseError).code).toBe("54000");
    await admin.query("UPDATE device_tokens SET status = 'revoked', revoked_at = now() WHERE user_id = $1", [personUser]);
  });

  it("purging a disabled personal account leaves nothing of the user", async () => {
    await admin.query(
      `INSERT INTO sample_types (identifier, kind, unit) VALUES ('HKQuantityTypeIdentifierStepCount', 'quantity', 'count') ON CONFLICT DO NOTHING`,
    );
    // Two sources: the person's own phone, and one the household's records use too.
    const source = async (name: string) => {
      const id = (await admin.query<{ id: number }>("INSERT INTO sources (name) VALUES ($1) RETURNING source_id AS id", [name])).rows[0].id;
      createdSources.push(id);
      return id;
    };
    const own = await source(`Pat's iPhone ${tag}`);
    const shared = await source(`Shared ${tag}`);
    await admin.query(
      `INSERT INTO quantity_samples (uuid, type_id, start_ts, end_ts, value, user_id, source_id)
       SELECT gen_random_uuid(), (SELECT type_id FROM sample_types WHERE identifier = 'HKQuantityTypeIdentifierStepCount'),
              now() - make_interval(days => g), now() - make_interval(days => g), g, $1, CASE WHEN g <= 3 THEN $2::smallint ELSE $3::smallint END
         FROM generate_series(1, 5) g`,
      [personUser, own, shared],
    );
    await admin.query(
      `INSERT INTO quantity_samples (uuid, type_id, start_ts, end_ts, value, user_id, source_id)
       VALUES (gen_random_uuid(), (SELECT type_id FROM sample_types WHERE identifier = 'HKQuantityTypeIdentifierStepCount'),
               now() - interval '2 days', now() - interval '2 days', 1, $1, $2)`,
      [ADMIN_USER, shared],
    );
    // A series point whose workout row is not the person's (ingest stores those).
    await admin.query(
      `INSERT INTO workout_series_points (workout_uuid, type_id, ts, value, user_id)
       VALUES (gen_random_uuid(), (SELECT type_id FROM sample_types WHERE identifier = 'HKQuantityTypeIdentifierStepCount'), now(), 1, $1)`,
      [personUser],
    );
    // Materialize the hourly rollups, so purging has to clear them too.
    await admin.query("CALL refresh_continuous_aggregate('quantity_rollups', now() - interval '10 days', now())");
    const mat = (await admin.query<{ name: string }>(
      `SELECT format('%I.%I', materialization_hypertable_schema, materialization_hypertable_name) AS name
         FROM timescaledb_information.continuous_aggregates WHERE view_name = 'quantity_rollups'`,
    )).rows[0].name;
    expect(Number((await admin.query(`SELECT count(*) FROM ${mat} WHERE user_id = $1`, [personUser])).rows[0].count)).toBe(5);
    const minted = await signups.issueDeviceToken(personSession, "third");

    expect(await sqlState(signups.deleteMyAccount(adminSession))).toBe("42501"); // not for admins
    const personAccount = (await admin.query("SELECT id::text FROM auth.accounts WHERE user_id = $1", [personUser])).rows[0].id;
    await signups.setAccountDisabled(adminSession, personAccount, true);
    expect((await admin.query("SELECT status FROM device_tokens WHERE id = $1", [minted.id])).rows[0].status).toBe("revoked");

    expect(await sqlState(signups.purgeUser(personSession, personUser))).toBe("42501");
    expect(await sqlState(signups.purgeUser(adminSession, ADMIN_USER))).toBe("42501"); // household
    // A second purge of the same user while one runs fails at once (NOWAIT),
    // rather than holding a pooled connection while it waits.
    const holder = new Client({ connectionString: ADMIN_URL });
    await holder.connect();
    try {
      await holder.query("BEGIN");
      await holder.query("SELECT 1 FROM users WHERE id = $1 FOR UPDATE", [personUser]);
      expect(await sqlState(signups.purgeUser(adminSession, personUser))).toBe("55P03");
    } finally {
      await holder.query("ROLLBACK");
      await holder.end();
    }
    const counts = await signups.purgeUser(adminSession, personUser);
    expect(counts.quantity_samples).toBe(5);
    expect(counts.quantity_rollups).toBe(5);
    expect(counts.workout_series_points).toBe(1);
    expect(counts.sources_blanked).toBe(1);
    const names = (await admin.query<{ id: number; name: string }>(
      "SELECT source_id AS id, name FROM sources WHERE source_id = ANY($1::smallint[]) ORDER BY source_id", [[own, shared]],
    )).rows;
    expect(names[0].name).toMatch(/^removed-[0-9a-f-]{36}$/);
    expect(names[1]).toEqual({ id: shared, name: `Shared ${tag}` });
    // The household's own record is untouched.
    expect((await admin.query("SELECT 1 FROM quantity_samples WHERE user_id = $1 AND source_id = $2", [ADMIN_USER, shared])).rows).toHaveLength(1);
    expect(Number((await admin.query(`SELECT count(*) FROM ${mat} WHERE user_id = $1`, [personUser])).rows[0].count)).toBe(0);
    for (const [table, where] of [
      ["users", "id"], ["quantity_samples", "user_id"], ["device_tokens", "user_id"], ["auth.accounts", "user_id"], ["auth.invites", "user_id"],
      ["auth.self_service_users", "user_id"], ["workout_series_points", "user_id"],
    ] as const) {
      expect((await admin.query(`SELECT 1 FROM ${table} WHERE ${where} = $1`, [personUser])).rows, table).toHaveLength(0);
    }
  });

  it("refuses to purge a user whose account is still enabled", async () => {
    const email = `flood0-${tag}@example.com`;
    const [{ id }] = await requestRow(email);
    const adminAccount = (await admin.query("SELECT id::text FROM auth.accounts WHERE email = $1", [ADMIN_EMAIL])).rows[0].id;
    const approval = await signups.approveSignup(adminSession, adminAccount, id);
    await inviteRoute.POST(post("/api/auth/invite", { token: approval.inviteToken, password: PASSWORD, confirm: PASSWORD }, { ip: "198.51.100.60" }));
    expect(await sqlState(signups.purgeUser(adminSession, approval.userId))).toBe("55000");
  });

  it("lists unused approvals and creates a fresh member for resend and reapproval", async () => {
    const email = `flood1-${tag}@example.com`;
    const [{ id }] = await requestRow(email);
    const adminAccount = (await admin.query("SELECT id::text FROM auth.accounts WHERE email = $1", [ADMIN_EMAIL])).rows[0].id;
    const first = await signups.approveSignup(adminSession, adminAccount, id);
    expect((await signups.listUnusedApprovals()).map((u) => u.userId)).toContain(first.userId);

    const again = await signups.reinvite(adminSession, first.userId);
    expect(again).not.toBeNull();
    expect(again?.userId).not.toBe(first.userId);
    // The first link stopped working; the new one is the only live invite.
    const live = await admin.query("SELECT 1 FROM auth.invites WHERE email = $1 AND accepted_at IS NULL AND expires_at > now()", [email]);
    expect(live.rows).toHaveLength(1);
    expect(await signups.reinvite(adminSession, ADMIN_USER)).toBeNull(); // not an unused approval

    await signupRoute.POST(post("/api/auth/signup", { email, consent: "yes" }, { ip: "198.51.100.70" }));
    const second = (await requestRow(email)).find((r) => r.status === "pending")!;
    const reapproved = await signups.approveSignup(adminSession, adminAccount, second.id);
    expect([first.userId, again?.userId]).not.toContain(reapproved.userId);
    expect((await admin.query("SELECT user_id::text FROM auth.invites WHERE email=$1 AND accepted_at IS NULL AND expires_at>now()", [email])).rows)
      .toEqual([{ user_id: reapproved.userId }]);

    // Never used, so it can be purged without disabling anything.
    await signups.purgeUser(adminSession, first.userId);
    expect(await signups.listUnusedApprovals()).not.toContainEqual(expect.objectContaining({ userId: first.userId }));
  });

  for (const mode of ["reinvite", "reapprove"]) {
    it(`${mode} cannot turn an existing unclaimed member's health data into a new invitation`, async () => {
      const email = `${mode}-${tag}@example.com`;
      await signupRoute.POST(post("/api/auth/signup", { email, consent: "yes" }, { ip: mode === "reinvite" ? "203.0.113.241" : "203.0.113.242" }));
      const firstRequest = (await requestRow(email)).find((row) => row.status === "pending");
      if (!firstRequest) throw new Error("Synthetic pending request was not created");
      const adminAccount = (await admin.query<{ id: string }>("SELECT id::text FROM auth.accounts WHERE email=$1", [ADMIN_EMAIL])).rows[0].id;
      const first = await signups.approveSignup(adminSession, adminAccount, firstRequest.id);
      const workout = randomUUID();
      await admin.query("INSERT INTO workouts(uuid,activity_type,start_ts,end_ts,user_id) VALUES($1,'running',now()-interval '1 hour',now(),$2)", [workout, first.userId]);
      let issued: import("./accounts/signups").Approval | null;
      if (mode === "reinvite") {
        issued = await signups.reinvite(adminSession, first.userId);
      } else {
        await signupRoute.POST(post("/api/auth/signup", { email, consent: "yes" }, { ip: "203.0.113.243" }));
        const second = (await requestRow(email)).find((row) => row.status === "pending");
        if (!second) throw new Error("Synthetic second request was not created");
        issued = await signups.approveSignup(adminSession, adminAccount, second.id);
      }
      if (!issued) throw new Error("Synthetic invitation was not returned");
      const response = await inviteRoute.POST(post("/api/auth/invite", { token: issued.inviteToken, password: PASSWORD, confirm: PASSWORD }, { ip: "203.0.113.244" }));
      const session = await (await import("./accounts/session")).findSession(cookieOf(response));
      if (!session) throw new Error("Synthetic invited account could not sign in");
      const visible = await (await import("./db")).scoped(session.userId, (q) => q<{ found: boolean }>(
        "SELECT EXISTS(SELECT 1 FROM workouts WHERE uuid=$1) AS found", [workout]));
      expect(visible[0].found, "a fresh invitation must not reveal a pre-existing member workout").toBe(false);
      expect(issued.userId).not.toBe(first.userId);
      expect((await admin.query("SELECT uuid FROM workouts WHERE uuid=$1 AND user_id=$2", [workout, first.userId])).rowCount).toBe(1);
      expect(await sqlState(signups.reinvite(session.id, first.userId))).toBe("42501");
      expect(await sqlState(signups.reinvite(Buffer.alloc(32, 9), first.userId))).toBe("42501");
    });
  }

  it("prunes decided requests and never-used approvals after 30 days, in the database", async () => {
    const email = `flood2-${tag}@example.com`;
    const [{ id }] = await requestRow(email);
    const adminAccount = (await admin.query("SELECT id::text FROM auth.accounts WHERE email = $1", [ADMIN_EMAIL])).rows[0].id;
    const approval = await signups.approveSignup(adminSession, adminAccount, id);
    const userExists = async () => (await admin.query("SELECT 1 FROM users WHERE id = $1", [approval.userId])).rows.length === 1;

    await admin.query("CALL auth.prune_signups(0, NULL)");
    expect(await requestRow(email)).toHaveLength(1); // recent: kept
    expect(await userExists()).toBe(true);

    await admin.query("UPDATE auth.signup_requests SET decided_at = decided_at - interval '31 days' WHERE id = $1", [id]);
    await admin.query("UPDATE auth.self_service_users SET approved_at = approved_at - interval '31 days' WHERE user_id = $1", [approval.userId]);
    await admin.query(
      "UPDATE auth.invites SET created_at = created_at - interval '31 days', expires_at = expires_at - interval '31 days' WHERE user_id = $1",
      [approval.userId],
    );
    await admin.query("CALL auth.prune_signups(0, NULL)");
    expect(await requestRow(email)).toHaveLength(0);
    expect(await userExists()).toBe(false);
    expect((await admin.query("SELECT 1 FROM auth.invites WHERE user_id = $1", [approval.userId])).rows).toHaveLength(0);

    // Scheduled, not left to someone opening /admin.
    const jobs = await admin.query("SELECT schedule_interval::text FROM timescaledb_information.jobs WHERE proc_schema = 'auth' AND proc_name = 'prune_signups'");
    expect(jobs.rows).toEqual([{ schedule_interval: "01:00:00" }]);
  });

  it("never lets an administrator disable or enable an administrator's account, their own included", async () => {
    // A self-service person the operator later made an administrator (SQL).
    const email = `boss-${tag}@example.com`;
    await signupRoute.POST(post("/api/auth/signup", { email, consent: "yes" }, { ip: "198.51.100.110" }));
    const [{ id }] = await requestRow(email);
    const adminAccount = (await admin.query("SELECT id::text FROM auth.accounts WHERE email = $1", [ADMIN_EMAIL])).rows[0].id;
    const approval = await signups.approveSignup(adminSession, adminAccount, id);
    await inviteRoute.POST(post("/api/auth/invite", { token: approval.inviteToken, password: PASSWORD, confirm: PASSWORD }, { ip: "198.51.100.111" }));
    const bossAccount = (await admin.query("UPDATE auth.accounts SET is_admin = true WHERE email = $1 RETURNING id::text", [email])).rows[0].id;
    const bossCookie = cookieOf(await loginRoute.POST(post("/api/auth/login", { email, password: PASSWORD }, { ip: "198.51.100.112" })))!;
    expect(bossCookie).toBeTruthy();
    const bossSession = tokenHash(bossCookie)!;

    // In the database: another administrator's account, and one's own.
    expect(await sqlState(signups.setAccountDisabled(adminSession, bossAccount, true))).toBe("42501");
    expect(await sqlState(signups.setAccountDisabled(bossSession, bossAccount, true))).toBe("42501");
    expect(await sqlState(signups.setAccountDisabled(adminSession, bossAccount, false))).toBe("42501");
    // Through /api/admin, with a clear answer.
    const disable = (cookie: string) => adminRoute.POST(post("/api/admin", { action: "disable", id: bossAccount }, { cookie }));
    expect((await disable(adminCookie)).headers.get("location")).toBe("/admin?error=admin_account");
    expect((await disable(bossCookie)).headers.get("location")).toBe("/admin?error=own_account");
    expect((await admin.query("SELECT disabled_at FROM auth.accounts WHERE id = $1", [bossAccount])).rows[0].disabled_at).toBeNull();
  });

  it("declines pending requests in bulk, for an administrator's session only, and leaves decided ones alone", async () => {
    const emails = [`d1-${tag}@example.com`, `d2-${tag}@example.com`, `d3-${tag}@example.com`];
    for (const [i, email] of emails.entries()) {
      await signupRoute.POST(post("/api/auth/signup", { email, consent: "yes" }, { ip: `198.51.100.${120 + i}` }));
    }
    const ids = (await Promise.all(emails.map(requestRow))).map((rows) => rows[0].id);
    expect(await sqlState(signups.declineSignups(Buffer.alloc(32, 7), ids))).toBe("42501");

    // A decided request in the same batch is not deleted.
    const decided = (await admin.query<{ id: string }>(
      "SELECT id::text FROM auth.signup_requests WHERE email LIKE $1 AND status = 'approved' LIMIT 1", [`boss-${tag}@example.com`],
    )).rows[0].id;
    expect(await signups.declineSignups(adminSession, [ids[0], decided])).toBe(1);
    expect(await requestRow(emails[0])).toHaveLength(0);
    expect((await admin.query("SELECT 1 FROM auth.signup_requests WHERE id = $1", [decided])).rows).toHaveLength(1);

    // /admin's Decline selected: the ticked ones only.
    const form = new FormData();
    form.set("action", "deny_many");
    form.set("scope", "selected");
    form.append("ids", ids[1]);
    const res = await adminRoute.POST(
      new NextRequest(new URL("/api/admin", "http://web:3000"), {
        method: "POST",
        headers: new Headers({
          host: "viewer.example",
          origin: "https://viewer.example",
          "x-forwarded-proto": "https",
          "x-forwarded-for": "198.51.100.200",
          cookie: `${COOKIE}=${adminCookie}`,
        }),
        body: form,
      }),
    );
    expect(res.headers.get("location")).toBe("/admin?notice=denied_many");
    expect(await requestRow(emails[1])).toHaveLength(0);
    expect(await requestRow(emails[2])).toHaveLength(1);
  });

  it("keeps a waitlist entry nobody decided, however old, and lists the waitlist oldest first", async () => {
    const stale = `d3-${tag}@example.com`;
    const fresh = `d4-${tag}@example.com`;
    await signupRoute.POST(post("/api/auth/signup", { email: fresh, consent: "yes" }, { ip: "198.51.100.130" }));
    await admin.query("UPDATE auth.signup_requests SET created_at = now() - interval '400 days' WHERE email = $1", [stale]);
    await admin.query("CALL auth.prune_signups(0, NULL)");
    expect(await requestRow(stale)).toMatchObject([{ status: "pending" }]);
    expect(await requestRow(fresh)).toMatchObject([{ status: "pending" }]);

    const listed = (await signups.listSignupRequests()).filter((r) => r.status === "pending").map((r) => r.email);
    expect(listed.indexOf(stale)).toBeGreaterThanOrEqual(0);
    expect(listed.indexOf(stale)).toBeLessThan(listed.indexOf(fresh));
    const created = (await signups.listSignupRequests()).filter((r) => r.status === "pending").map((r) => r.createdAt);
    expect(created).toEqual([...created].sort((a, b) => a - b));
  });

  it("drops new sign-ups past the daily intake or the waitlist's ceiling, saying the same as ever", async () => {
    const { SIGNUP_DAILY_CAP, SIGNUP_PENDING_CAP } = signups;
    const count = async (where: string) => Number((await admin.query(`SELECT count(*) FROM auth.signup_requests WHERE ${where}`)).rows[0].count);
    const fill = (n: number, prefix: string, age: string) =>
      admin.query(
        `INSERT INTO auth.signup_requests (email, created_at)
         SELECT $3 || g || '-' || $2 || '@example.com', now() - $4::interval FROM generate_series(1, $1::int) g`,
        [Math.max(0, n), tag, prefix, age],
      );
    const signUp = (email: string, ip: string) => signupRoute.POST(post("/api/auth/signup", { email, consent: "yes" }, { ip }));
    const email = `late-${tag}@example.com`;

    // The daily intake: SIGNUP_DAILY_CAP stored in the last 24 hours.
    await fill(SIGNUP_DAILY_CAP - (await count("created_at > now() - interval '1 day'")), "today", "0");
    expect((await signUp(email, "198.51.100.140")).headers.get("location")).toBe("/signup?notice=received");
    expect(await requestRow(email)).toHaveLength(0);
    // A day later they are still waiting, and new sign-ups are taken again.
    await admin.query("UPDATE auth.signup_requests SET created_at = now() - interval '2 days' WHERE email LIKE $1", [`today%-${tag}@example.com`]);
    expect((await signUp(email, "198.51.100.141")).headers.get("location")).toBe("/signup?notice=received");
    expect(await requestRow(email)).toMatchObject([{ status: "pending" }]);

    // The ceiling: SIGNUP_PENDING_CAP waiting, however old.
    const later = `later-${tag}@example.com`;
    await fill(SIGNUP_PENDING_CAP - (await count("status = 'pending'")), "fill", "3 days");
    expect((await signUp(later, "198.51.100.142")).headers.get("location")).toBe("/signup?notice=received");
    expect(await requestRow(later)).toHaveLength(0);
    // The hourly job does not drain it: the waitlist is kept until decided.
    await admin.query("CALL auth.prune_signups(0, NULL)");
    expect(await count("status = 'pending'")).toBeGreaterThanOrEqual(SIGNUP_PENDING_CAP);
    // /admin lists the oldest SIGNUP_LIST_LIMIT, which Decline all shown can clear.
    expect((await signups.listSignupRequests()).filter((r) => r.status === "pending")).toHaveLength(signups.SIGNUP_LIST_LIMIT);

    await admin.query("DELETE FROM auth.signup_requests WHERE email LIKE $1 OR email LIKE $2", [`fill%-${tag}@example.com`, `today%-${tag}@example.com`]);
    expect((await signUp(later, "198.51.100.143")).headers.get("location")).toBe("/signup?notice=received");
    expect(await requestRow(later)).toMatchObject([{ status: "pending" }]);
  });
});
