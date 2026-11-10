// Accounts mode end to end against a real, migrated database, through the
// viewer's own route handlers and proxy.ts, connected as web_app: an invite
// creates an account and signs in, sign-in and sign-out work and rotate
// sessions, the signed-in user's reads are scoped to them, a password change
// signs other browsers out, failed attempts are throttled — but never keep
// the owner out of an address they signed in from — and expired sessions are
// left to the database's hourly job. Skipped
// without the same two connection strings as webapp.integration.test.ts.

import { randomBytes, randomUUID } from "node:crypto";
import { NextRequest } from "next/server";
import { Client } from "pg";
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";

import { newInviteToken } from "../scripts/invite.mjs";
import { requireIntegrationDatabases } from "./integrationEnv";

// In CI the job sets both; a missing one there must fail, not skip quietly
// (PULS_CI_REQUIRE_INTEGRATION / PULS_WEB_INTEGRATION, lib/integrationEnv.ts).
const { webUrl: WEB_URL, adminUrl: ADMIN_URL } = requireIntegrationDatabases();

const A = randomUUID();
const B = randomUUID();
const EMAIL = `${A.slice(0, 8)}@example.com`;
const OTHER_EMAIL = `${B.slice(0, 8)}@example.com`;
const OTHER_PASSWORD = "the other person's passphrase";
const PASSWORD = "a long enough passphrase";
const COOKIE = "__Host-puls-session";
const ORIGIN = "https://viewer.example";

// A browser behind the TLS proxy: one client address per test, so the shared
// failure limiter's buckets do not leak between tests.
function post(path: string, fields: Record<string, string>, opts: { cookie?: string; ip?: string } = {}) {
  const body = new FormData();
  for (const [k, v] of Object.entries(fields)) body.set(k, v);
  const headers = new Headers({
    host: "viewer.example",
    origin: ORIGIN,
    "x-forwarded-proto": "https",
    "x-forwarded-for": opts.ip ?? "198.51.100.1",
    "user-agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Safari/605.1.15",
  });
  if (opts.cookie) headers.set("cookie", `${COOKIE}=${opts.cookie}`);
  return new NextRequest(new URL(path, "http://web:3000"), { method: "POST", headers, body });
}

function sessionCookie(res: Response): string | undefined {
  const match = /__Host-puls-session=([^;]*)/.exec(res.headers.get("set-cookie") ?? "");
  return match?.[1] || undefined;
}

// Token-bucket assertions must not depend on password hashing or CI speed.
// Fix Date.now only; database I/O and timers remain real. Refill behavior is
// covered with explicit timestamps in accounts/ratelimit.test.ts.
async function withFixedTime(run: () => Promise<void>): Promise<void> {
  const clock = vi.spyOn(Date, "now").mockReturnValue(Date.now());
  try {
    await run();
  } finally {
    clock.mockRestore();
  }
}

describe.skipIf(!WEB_URL || !ADMIN_URL)("accounts mode (integration)", () => {
  let admin: Client;
  const workoutA = randomUUID();
  const workoutB = randomUUID();
  let inviteToken: string;
  let loginRoute: typeof import("@/app/api/auth/login/route");
  let logoutRoute: typeof import("@/app/api/auth/logout/route");
  let inviteRoute: typeof import("@/app/api/auth/invite/route");
  let passwordRoute: typeof import("@/app/api/auth/password/route");
  let sessionsRoute: typeof import("@/app/api/auth/sessions/route");
  let session: typeof import("./accounts/session");

  beforeAll(async () => {
    process.env.WEB_ACCOUNTS = "true";
    process.env.TRUST_PROXY_HEADERS = "true";
    process.env.DATABASE_URL = WEB_URL;

    admin = new Client({ connectionString: ADMIN_URL });
    await admin.connect();
    for (const [user, workout] of [[A, workoutA], [B, workoutB]]) {
      await admin.query("INSERT INTO users (id) VALUES ($1)", [user]);
      await admin.query(
        `INSERT INTO workouts (uuid, activity_type, start_ts, end_ts, user_id)
         VALUES ($1, 'running', now() - interval '2 hours', now() - interval '1 hour', $2)`,
        [workout, user],
      );
    }
    const invite = newInviteToken();
    inviteToken = invite.token;
    await admin.query(
      `INSERT INTO auth.invites (token_hash, user_id, email, expires_at) VALUES ($1, $2, $3, now() + interval '1 hour')`,
      [invite.hash, A, EMAIL],
    );

    // User B gets an account straight away, for the tests that need a second one.
    const { hashPassword } = await import("./accounts/password");
    await admin.query("INSERT INTO auth.accounts (user_id, email, password_hash) VALUES ($1, $2, $3)", [
      B, OTHER_EMAIL, await hashPassword(OTHER_PASSWORD),
    ]);

    loginRoute = await import("@/app/api/auth/login/route");
    logoutRoute = await import("@/app/api/auth/logout/route");
    inviteRoute = await import("@/app/api/auth/invite/route");
    passwordRoute = await import("@/app/api/auth/password/route");
    sessionsRoute = await import("@/app/api/auth/sessions/route");
    session = await import("./accounts/session");
  });

  afterAll(async () => {
    await (await import("./db")).getPool()?.end();
    await (await import("./db")).getHealthPool()?.end();
    if (!admin) return;
    await admin.query("DELETE FROM auth.invites WHERE user_id = ANY($1)", [[A, B]]);
    await admin.query("DELETE FROM auth.sessions WHERE account_id IN (SELECT id FROM auth.accounts WHERE user_id = ANY($1))", [[A, B]]);
    await admin.query("DELETE FROM auth.accounts WHERE user_id = ANY($1)", [[A, B]]);
    await admin.query("DELETE FROM workouts WHERE user_id = ANY($1)", [[A, B]]);
    await admin.query("DELETE FROM users WHERE id = ANY($1)", [[A, B]]);
    await admin.end();
  });

  let browser: string | undefined;

  it("allows only one concurrent password change authorized by the same credentials", async () => {
    const store = await import("./accounts/store");
    const account = (await store.findAccountForLogin(OTHER_EMAIL))!;
    const meta = { userAgent: null, ip: null };
    const held = await session.createSession(account.id, meta);
    const id = session.tokenHash(held)!;
    try {
      const brokenMeta = {
        get userAgent(): string | null { throw new Error("session creation failed"); },
        ip: null,
      };
      await expect(store.changePasswordAndSession(account.id, account.passwordHash, id, "rolled-back-hash", brokenMeta))
        .rejects.toThrow("session creation failed");
      expect(await store.findPasswordHash(account.id)).toBe(account.passwordHash);
      expect(await session.findSession(held)).not.toBeNull();

      const results = await Promise.all([
        store.changePasswordAndSession(account.id, account.passwordHash, id, "new-hash-one", meta),
        store.changePasswordAndSession(account.id, account.passwordHash, id, "new-hash-two", meta),
      ]);
      expect(results.filter(Boolean)).toHaveLength(1);
      expect(await session.findSession(held)).toBeNull();
      const replacement = results.find((token) => token !== null)!;
      expect(await session.findSession(replacement)).not.toBeNull();
      const currentHash = (await store.findPasswordHash(account.id))!;
      // Even a verified current password cannot revive a revoked session.
      expect(await store.changePasswordAndSession(account.id, currentHash, id, "stale-session-change", meta)).toBeNull();
      await store.changePassword(account.id, account.passwordHash);
      expect(await session.findSession(replacement)).toBeNull();
    } finally {
      await store.changePassword(account.id, account.passwordHash);
    }
  });

  it("rolls back a new account and invite if its first session cannot be created", async () => {
    const store = await import("./accounts/store");
    const account = (await store.findAccountForLogin(OTHER_EMAIL))!;
    const meta = { userAgent: null, ip: null };
    const held = await session.createSession(account.id, meta);
    const pending = randomUUID();
    await admin.query("INSERT INTO users(id) VALUES($1)", [pending]);
    const invite = newInviteToken();
    await admin.query(
      "INSERT INTO auth.invites (token_hash, user_id, email, expires_at) VALUES ($1, $2, $3, now() + interval '1 hour')",
      [invite.hash, pending, `${pending}@synthetic.invalid`],
    );
    try {
      const brokenMeta = {
        get userAgent(): string | null { throw new Error("session creation failed"); },
        ip: null,
      };
      await expect(store.acceptInvite(invite.token, "synthetic-first-password-hash", brokenMeta, session.tokenHash(held)))
        .rejects.toThrow("session creation failed");
      expect(await store.findPasswordHash(account.id)).toBe(account.passwordHash);
      expect(await store.findInvite(invite.token)).not.toBeNull();
      expect(await session.findSession(held)).not.toBeNull();
      expect((await admin.query("SELECT 1 FROM auth.accounts WHERE user_id=$1", [pending])).rows).toEqual([]);

      const result = await store.acceptInvite(invite.token, "synthetic-first-password-hash", meta, session.tokenHash(held));
      expect(result.ok).toBe(true);
      if (!result.ok) return;
      expect(await session.findSession(result.sessionToken)).not.toBeNull();
      expect(await session.findSession(held)).toBeNull();
    } finally {
      await admin.query("DELETE FROM auth.invites WHERE user_id=$1", [pending]);
      await admin.query("DELETE FROM auth.sessions WHERE id=$1 OR account_id IN (SELECT id FROM auth.accounts WHERE user_id=$2)", [session.tokenHash(held), pending]);
      await admin.query("DELETE FROM auth.accounts WHERE user_id=$1", [pending]);
      await admin.query("DELETE FROM users WHERE id=$1", [pending]);
    }
  });

  it("rejects stale password verification and rehashes without undoing a password change", async () => {
    const store = await import("./accounts/store");
    const account = await store.findAccountForLogin(OTHER_EMAIL);
    expect(account).not.toBeNull();
    if (!account) return;
    const meta = { userAgent: null, ip: null };
    try {
      // A login already read and verified this hash when another browser
      // changes the password. Neither its session nor its legacy rehash may
      // commit afterward using the stale credentials.
      await store.changePassword(account.id, "replacement-password-hash");
      expect(await store.finishPasswordLogin(account, null, meta, null)).toBeNull();
      expect(await store.finishPasswordLogin(account, "rehash-of-old-password", meta, null)).toBeNull();
      expect(await store.findPasswordHash(account.id)).toBe("replacement-password-hash");
      expect((await admin.query("SELECT 1 FROM auth.sessions WHERE account_id = $1", [account.id])).rows).toHaveLength(0);

      // The opposite order also preserves revocation: a completed login's
      // session must be removed by the following password change.
      const current = await store.findAccountForLogin(OTHER_EMAIL);
      const token = await store.finishPasswordLogin(current!, "upgraded-password-hash", meta, null);
      expect(await session.findSession(token)).not.toBeNull();
      await store.changePassword(account.id, account.passwordHash);
      expect(await session.findSession(token)).toBeNull();
    } finally {
      await store.changePassword(account.id, account.passwordHash);
    }
  });

  it("accepts an invite: creates the account and signs the browser in", async () => {
    const short = await inviteRoute.POST(post("/api/auth/invite", { token: inviteToken, password: "short", confirm: "short" }, { ip: "198.51.100.10" }));
    expect(short.headers.get("location")).toBe(`/invite/${inviteToken}?error=short`);

    const res = await inviteRoute.POST(
      post("/api/auth/invite", { token: inviteToken, password: PASSWORD, confirm: PASSWORD }, { ip: "198.51.100.10" }),
    );
    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe("/?notice=welcome");
    browser = sessionCookie(res);
    expect(browser).toBeTruthy();
    expect(res.headers.get("set-cookie")).toMatch(/HttpOnly/i);
    expect(res.headers.get("set-cookie")).toMatch(/Secure/i);
    expect(res.headers.get("set-cookie")).toMatch(/SameSite=lax/i);

    const found = await session.findSession(browser);
    expect(found).toMatchObject({ userId: A, email: EMAIL });

    // Spent: the same link does nothing a second time.
    const again = await inviteRoute.POST(
      post("/api/auth/invite", { token: inviteToken, password: PASSWORD, confirm: PASSWORD }, { ip: "198.51.100.10" }),
    );
    expect(again.headers.get("location")).toBe(`/invite/${inviteToken}?error=invalid`);
  });

  it("reads only the signed-in user's rows, whatever the query asks", async () => {
    const { scoped } = await import("./db");
    const found = await session.findSession(browser);
    const rows = await scoped(found!.userId, (q) => q<{ uuid: string }>("SELECT uuid::text FROM workouts"));
    expect(rows.map((r) => r.uuid)).toEqual([workoutA]);
    const asked = await scoped(found!.userId, (q) => q("SELECT 1 FROM workouts WHERE uuid = $1::uuid", [workoutB]));
    expect(asked).toEqual([]);
  });

  it("lets proxy.ts through with the cookie, and not without", async () => {
    const { default: proxy } = await import("@/proxy");
    const page = (cookie?: string) => {
      const headers = new Headers({ host: "viewer.example", "x-forwarded-proto": "https" });
      if (cookie) headers.set("cookie", `${COOKIE}=${cookie}`);
      return new NextRequest(new URL("/workouts", "http://web:3000"), { headers });
    };
    expect((await proxy(page(browser))).headers.get("x-middleware-next")).toBe("1");
    expect((await proxy(page())).headers.get("location")).toBe("https://viewer.example/login?next=%2Fworkouts");
  });

  it("signs in with the right password only, and never says which half was wrong", async () => {
    const wrong = await loginRoute.POST(post("/api/auth/login", { email: EMAIL, password: "not the password", next: "/data" }, { ip: "198.51.100.20" }));
    expect(wrong.headers.get("location")).toBe("/login?error=invalid&next=%2Fdata");
    expect(sessionCookie(wrong)).toBeUndefined();
    const nobody = await loginRoute.POST(post("/api/auth/login", { email: "nobody@example.com", password: PASSWORD }, { ip: "198.51.100.20" }));
    expect(nobody.headers.get("location")).toBe("/login?error=invalid");

    // Mixed case and spaces in the address are the same account; `next` is kept
    // to this origin.
    const ok = await loginRoute.POST(
      post("/api/auth/login", { email: ` ${EMAIL.toUpperCase()} `, password: PASSWORD, next: "https://evil.example/" }, { ip: "198.51.100.20" }),
    );
    expect(ok.headers.get("location")).toBe("/");
    expect(sessionCookie(ok)).toBeTruthy();
  });

  it("gives a new session on sign-in and retires the one the browser held", async () => {
    const res = await loginRoute.POST(post("/api/auth/login", { email: EMAIL, password: PASSWORD }, { cookie: browser, ip: "198.51.100.21" }));
    const fresh = sessionCookie(res);
    expect(fresh).toBeTruthy();
    expect(fresh).not.toBe(browser);
    expect(await session.findSession(browser)).toBeNull();
    browser = fresh;
  });

  it("changes the password only with the current one, and signs other browsers out", async () => {
    const other = sessionCookie(await loginRoute.POST(post("/api/auth/login", { email: EMAIL, password: PASSWORD }, { ip: "198.51.100.30" })));
    expect(await session.findSession(other)).not.toBeNull();

    const next = "another long passphrase";
    const wrong = await passwordRoute.POST(
      post("/api/auth/password", { current: "nope nope nope", password: next, confirm: next }, { cookie: browser, ip: "198.51.100.31" }),
    );
    expect(wrong.headers.get("location")).toBe("/account?error=current");

    const ok = await passwordRoute.POST(
      post("/api/auth/password", { current: PASSWORD, password: next, confirm: next }, { cookie: browser, ip: "198.51.100.31" }),
    );
    expect(ok.headers.get("location")).toBe("/account?notice=password");
    expect(await session.findSession(other)).toBeNull();
    // This browser's session is replaced too: a copy of its old cookie is dead.
    const rotated = sessionCookie(ok);
    expect(rotated).toBeTruthy();
    expect(await session.findSession(browser)).toBeNull();
    expect(await session.findSession(rotated)).not.toBeNull();
    browser = rotated;

    const old = await loginRoute.POST(post("/api/auth/login", { email: EMAIL, password: PASSWORD }, { ip: "198.51.100.32" }));
    expect(old.headers.get("location")).toBe("/login?error=invalid");
    const fresh = await loginRoute.POST(post("/api/auth/login", { email: EMAIL, password: next }, { ip: "198.51.100.32" }));
    expect(sessionCookie(fresh)).toBeTruthy();
  });

  it("signs other browsers out from the session list", async () => {
    const found = await session.findSession(browser);
    const before = await session.listSessions(found!.accountId, found!.id);
    expect(before.length).toBeGreaterThan(1);
    expect(before.filter((s) => s.current)).toHaveLength(1);
    const res = await sessionsRoute.POST(post("/api/auth/sessions", { session: "others" }, { cookie: browser }));
    expect(res.headers.get("location")).toBe("/account?notice=sessions");
    const after = await session.listSessions(found!.accountId, found!.id);
    expect(after.map((s) => s.current)).toEqual([true]);
  });

  it("signs out: the session row goes, so a copy of the cookie stops working", async () => {
    const res = await logoutRoute.POST(post("/api/auth/logout", {}, { cookie: browser }));
    expect(res.headers.get("location")).toBe("/login?notice=signed-out");
    expect(res.headers.get("set-cookie")).toMatch(/Max-Age=0/i);
    expect(await session.findSession(browser)).toBeNull();
  });

  it.each([
    ["/connect/iphone", "/login?notice=signed-out&next=%2Fconnect%2Fiphone"],
    ["https://elsewhere.example/connect", "/login?notice=signed-out"],
    ["//elsewhere.example/connect", "/login?notice=signed-out"],
  ])("preserves a safe connection destination when switching accounts: %s", async (next, location) => {
    const res = await logoutRoute.POST(post("/api/auth/logout", { next }));
    expect(res.headers.get("location")).toBe(location);
    expect(res.headers.get("set-cookie")).toMatch(/Max-Age=0/i);
  });

  it("lets no more guesses through a parallel burst than the bucket holds", () => withFixedTime(async () => {
    // All in flight at once, from one address, at one account: only as many
    // as the bucket holds may reach the password check — the right password
    // among them included, whatever its place.
    const attempts = Array.from({ length: 30 }, (_, i) =>
      loginRoute.POST(post("/api/auth/login", { email: EMAIL, password: i === 29 ? "another long passphrase" : `parallel ${i} guess` }, { ip: "198.51.100.50" })),
    );
    const outcomes = (await Promise.all(attempts)).map((r) => r.headers.get("location"));
    const evaluated = outcomes.filter((l) => l !== "/login?error=throttled");
    expect(evaluated.length).toBeLessThanOrEqual(10);
    expect(outcomes.filter((l) => l === "/login?error=throttled").length).toBeGreaterThanOrEqual(20);
  }));

  it("charges nothing for a successful sign-in", async () => {
    // Twenty in a row from one address: none is a failure, so none is refused.
    // (A fresh email bucket: the parallel test above spent the shared one.)
    for (let i = 0; i < 12; i++) {
      const res = await loginRoute.POST(post("/api/auth/login", { email: OTHER_EMAIL, password: OTHER_PASSWORD }, { ip: "198.51.100.60" }));
      expect(res.headers.get("location"), `attempt ${i}`).toBe("/");
    }
  });

  it("keeps a disabled account sealed for both old and newly issued invitations", async () => {
    const stale = newInviteToken();
    await admin.query(
      `INSERT INTO auth.invites (token_hash, user_id, email, created_at, expires_at)
       VALUES ($1, $2, $3, now() - interval '1 minute', now() + interval '1 hour')`,
      [stale.hash, B, OTHER_EMAIL],
    );
    await admin.query("UPDATE auth.accounts SET disabled_at = now() WHERE user_id = $1", [B]);
    try {
      const res = await inviteRoute.POST(
        post("/api/auth/invite", { token: stale.token, password: "a brand new passphrase", confirm: "a brand new passphrase" }, { ip: "198.51.100.70" }),
      );
      expect(res.headers.get("location")).toBe(`/invite/${stale.token}?error=invalid`);
      const login = await loginRoute.POST(post("/api/auth/login", { email: OTHER_EMAIL, password: OTHER_PASSWORD }, { ip: "198.51.100.71" }));
      expect(login.headers.get("location")).toBe("/login?error=invalid");

      const fresh = newInviteToken();
      await admin.query(
        `INSERT INTO auth.invites (token_hash, user_id, email, created_at, expires_at)
         VALUES ($1, $2, $3, now() + interval '1 second', now() + interval '1 hour')`,
        [fresh.hash, B, OTHER_EMAIL],
      );
      const back = await inviteRoute.POST(
        post("/api/auth/invite", { token: fresh.token, password: OTHER_PASSWORD, confirm: OTHER_PASSWORD }, { ip: "198.51.100.72" }),
      );
      expect(back.headers.get("location")).toBe(`/invite/${fresh.token}?error=invalid`);
      const sealed = await admin.query<{ sealed: boolean }>("SELECT disabled_at IS NOT NULL AS sealed FROM auth.accounts WHERE user_id=$1", [B]);
      expect(sealed.rows.at(0)?.sealed).toBe(true);
    } finally {
      await admin.query("UPDATE auth.accounts SET disabled_at = NULL WHERE user_id = $1", [B]);
    }
  });

  it("retires the session a browser held when it accepts an invite", async () => {
    const store = await import("./accounts/store");
    const account = await store.findAccountForLogin(EMAIL);
    if (!account) throw new Error("Synthetic signed-in account is missing");
    const held = await session.createSession(account.id, { userAgent: null, ip: null });
    expect(await session.findSession(held)).not.toBeNull();
    const pending = randomUUID();
    await admin.query("INSERT INTO users(id) VALUES($1)", [pending]);
    const invite = newInviteToken();
    await admin.query(
      `INSERT INTO auth.invites (token_hash, user_id, email, expires_at) VALUES ($1, $2, $3, now() + interval '1 hour')`,
      [invite.hash, pending, `${pending}@synthetic.invalid`],
    );
    try {
      const res = await inviteRoute.POST(
        post("/api/auth/invite", { token: invite.token, password: PASSWORD, confirm: PASSWORD }, { cookie: held, ip: "198.51.100.81" }),
      );
      expect(res.headers.get("location")).toBe("/?notice=welcome");
      expect(await session.findSession(held)).toBeNull();
      expect((await session.findSession(sessionCookie(res)))?.userId).toBe(pending);
    } finally {
      await admin.query("DELETE FROM auth.invites WHERE user_id=$1", [pending]);
      await admin.query("DELETE FROM auth.sessions WHERE id=$1 OR account_id IN (SELECT id FROM auth.accounts WHERE user_id=$2)", [session.tokenHash(held), pending]);
      await admin.query("DELETE FROM auth.accounts WHERE user_id=$1", [pending]);
      await admin.query("DELETE FROM users WHERE id=$1", [pending]);
    }
  });

  it("throttles one address, and one account from many addresses, before checking the password", () => withFixedTime(async () => {
    for (let i = 0; i < 10; i++) {
      await loginRoute.POST(post("/api/auth/login", { email: `nobody${i}@example.com`, password: "x".repeat(12) }, { ip: "198.51.100.40" }));
    }
    const sprayed = await loginRoute.POST(post("/api/auth/login", { email: EMAIL, password: "another long passphrase" }, { ip: "198.51.100.40" }));
    expect(sprayed.headers.get("location")).toBe("/login?error=throttled");

    for (let i = 0; i < 10; i++) {
      await loginRoute.POST(post("/api/auth/login", { email: EMAIL, password: `guess number ${i}` }, { ip: `203.0.113.${i + 1}` }));
    }
    const right = await loginRoute.POST(post("/api/auth/login", { email: EMAIL, password: "another long passphrase" }, { ip: "203.0.113.99" }));
    expect(right.headers.get("location")).toBe("/login?error=throttled");
  }));

  it("still lets the owner in from an address one of their live sessions signed in from", () => withFixedTime(async () => {
    // Establish this test's exhausted email bucket independently. A live
    // session's address still answers only to its own address bucket.
    const { AUTH_FAILURE_BURST, authFailures } = await import("./accounts/ratelimit");
    for (let i = 0; i < AUTH_FAILURE_BURST; i++) authFailures.fail(`email:${EMAIL}`);
    const accountId = (await admin.query("SELECT id::text FROM auth.accounts WHERE email = $1", [EMAIL])).rows[0].id;
    await admin.query(
      "INSERT INTO auth.sessions (id, account_id, expires_at, ip) VALUES ($1, $2, now() + interval '1 day', '198.51.100.90')",
      [randomBytes(32), accountId],
    );
    // An expired session's address proves nothing.
    await admin.query(
      "INSERT INTO auth.sessions (id, account_id, expires_at, ip) VALUES ($1, $2, now() - interval '1 minute', '198.51.100.91')",
      [randomBytes(32), accountId],
    );
    const login = (ip: string) => loginRoute.POST(post("/api/auth/login", { email: EMAIL, password: "another long passphrase" }, { ip }));
    expect((await login("203.0.113.98")).headers.get("location")).toBe("/login?error=throttled");
    expect((await login("198.51.100.91")).headers.get("location")).toBe("/login?error=throttled");
    const owner = await login("198.51.100.90");
    expect(owner.headers.get("location")).toBe("/");
    expect(sessionCookie(owner)).toBeTruthy();
  }));

  it("leaves expired sessions to the database's hourly job, which removes them", async () => {
    const accountId = (await admin.query("SELECT id::text FROM auth.accounts WHERE email = $1", [OTHER_EMAIL])).rows[0].id;
    const live = randomBytes(32);
    const slid = randomBytes(32);
    const capped = randomBytes(32);
    await admin.query(
      `INSERT INTO auth.sessions (id, account_id, created_at, expires_at) VALUES
         ($1, $4, now(), now() + interval '1 day'),
         ($2, $4, now() - interval '40 days', now() - interval '1 minute'),
         ($3, $4, now() - interval '91 days', now() + interval '1 day')`,
      [live, slid, capped, accountId],
    );
    const present = async () =>
      (await admin.query<{ id: Buffer }>("SELECT id FROM auth.sessions WHERE id = ANY($1::bytea[])", [[live, slid, capped]])).rows.length;
    // Signing in no longer sweeps them...
    const res = await loginRoute.POST(post("/api/auth/login", { email: OTHER_EMAIL, password: OTHER_PASSWORD }, { ip: "198.51.100.95" }));
    expect(res.headers.get("location")).toBe("/");
    expect(await present()).toBe(3);
    // ...the hourly job does: past the sliding expiry or the 90-day cap.
    await admin.query("CALL auth.prune_signups(0, NULL)");
    expect((await admin.query("SELECT id FROM auth.sessions WHERE id = ANY($1::bytea[])", [[live, slid, capped]])).rows.map((r) => r.id.toString("hex")))
      .toEqual([live.toString("hex")]);
  });
});
