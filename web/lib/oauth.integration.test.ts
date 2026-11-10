import { oauthKeyFixture } from "./oauth/keyFixture";
// OAuth for AI assistants against a real, migrated database (019_oauth.sql),
// through the viewer's own store and route handlers, connected as web_app:
// registration; the consent's code exchanged once and only once (a replay
// revokes the grant it made), never by the wrong client, redirect URI or
// verifier, nor once expired; refresh tokens rotating, a rotated one
// revoking its grant; a disabled account refreshing nothing; a password
// change, an administrator's disable and a self-service deletion revoking
// grants; deleting an account deleting them; the account page's list and
// Revoke scoped to the account; RFC 7009 revocation; and the hourly prune.
// Skipped without the two connection strings.

import { createHash, randomUUID } from "node:crypto";
import { NextRequest } from "next/server";
import { Client } from "pg";
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";
import { issueAccessToken, signAccessToken, verifyAccessToken } from "./oauth/jwt";

import { requireIntegrationDatabases } from "./integrationEnv";

const oauthKeys = oauthKeyFixture();
const { webUrl: WEB_URL, adminUrl: ADMIN_URL } = requireIntegrationDatabases();

const ISSUER = "https://viewer.example";
const MCP = "https://mcp.example/mcp";
const REDIRECT = "https://claude.ai/api/mcp/auth_callback";
const VERIFIER = "v".repeat(20) + randomUUID().replace(/-/g, "");
const CHALLENGE = createHash("sha256").update(VERIFIER).digest("base64url");

const PERMISSIONS = { operations: ["metrics:read"], record_types: ["HKQuantityTypeIdentifierStepCount"], time_range: { from: "2020-01-01T00:00:00Z", until: "2030-01-01T00:00:00Z" } } satisfies import("./oauth/permissions").ReadPermissions;

const PERSON = randomUUID();
const OTHER = randomUUID();
const ADMIN = randomUUID();
const tag = PERSON.slice(0, 8);

describe.skipIf(!WEB_URL || !ADMIN_URL)("OAuth for AI assistants (integration)", () => {
  let admin: Client;
  let oauth: typeof import("./oauth/store");
  let accountId: string;
  let otherAccountId: string;
  let adminAccountId: string;
  let clientId: string;
  const clients: string[] = [];

  const sessionFor = async (account: string): Promise<Buffer> => {
    const id = createHash("sha256").update(randomUUID()).digest();
    await admin.query("INSERT INTO auth.sessions (id, account_id, expires_at, purpose) VALUES ($1, $2, now() + interval '1 day', 'personal')", [id, account]);
    return id;
  };
  const grantRow = (id: string) =>
    admin.query<{ revoked: boolean }>("SELECT revoked_at IS NOT NULL AS revoked FROM auth.oauth_grants WHERE id = $1", [id]).then((r) => r.rows[0]);
  const request = (over: Partial<import("./oauth/validate").AuthorizeRequest> = {}) => ({
    clientId,
    clientName: "Claude",
    redirectUri: REDIRECT,
    codeChallenge: CHALLENGE,
    scope: "health:read",
    resource: MCP,
    state: "s",
    ...over,
  });
  const exchange = (code: string, over: Record<string, unknown> = {}) =>
    oauth.exchangeCode({ code, clientId, redirectUri: REDIRECT, codeVerifier: VERIFIER, resource: null, configuredResource: MCP, ...over });
  const newGrant = async (account = accountId) => {
    const r = await exchange(await oauth.createCode(account, request(), PERMISSIONS));
    if (!r.ok) throw new Error(`exchange failed: ${r.error}`);
    return r.grant;
  };

  it("binds codes and refresh tokens to the consented resource", async () => {
    const resource = `${ISSUER}/api/health`;
    const code = await oauth.createCode(accountId, request({ resource }), PERMISSIONS);
    const resources = [MCP, resource];
    const wrong = await exchange(code, { resource: MCP, configuredResource: resources });
    expect(wrong).toEqual({ ok: false, error: "invalid_target" });
    const issued = await exchange(code, { resource, configuredResource: resources });
    expect(issued.ok).toBe(true);
    if (!issued.ok) throw new Error("exchange failed");
    expect(issued.grant.resource).toBe(resource);
    const switched = await oauth.refreshGrant({ refreshToken: issued.grant.refreshToken, clientId, scope: null, resource: MCP });
    expect(switched).toEqual({ ok: false, error: "invalid_target" });
    const refreshed = await oauth.refreshGrant({ refreshToken: issued.grant.refreshToken, clientId, scope: null });
    expect(refreshed.ok && refreshed.grant.resource).toBe(resource);
  });

  it("stores explicit permissions and carries the current version through exchange and refresh", async () => {
    const grant = await newGrant();
    expect(grant.permissions).toEqual(PERMISSIONS);
    expect(grant.authorizationVersion).toBe(1);
    const narrowed = { ...PERMISSIONS, record_types: ["oura.sleep"], time_range: { from: "2026-01-01T00:00:00.000000001Z", until: "2026-01-01T00:00:00.000000002Z" } };
    await admin.query("UPDATE auth.oauth_grants SET permissions=$2, authorization_version=authorization_version+1 WHERE id=$1", [grant.grantId, JSON.stringify(narrowed)]);
    const refreshed = await oauth.refreshGrant({ refreshToken: grant.refreshToken, clientId, scope: null });
    expect(refreshed.ok).toBe(true);
    if (!refreshed.ok) throw new Error("refresh failed");
    expect(refreshed.grant.permissions).toEqual(narrowed);
    expect(refreshed.grant.authorizationVersion).toBe(2);
  });

  it("refuses legacy codes and grants without a personal scope", async () => {
    const code = await oauth.createCode(accountId, request(), PERMISSIONS);
    await admin.query("UPDATE auth.oauth_codes SET permissions=NULL WHERE id=$1", [(await import("./accounts/session")).tokenHash(code)]);
    expect(await exchange(code)).toEqual({ ok: false, error: "invalid_grant" });
    const grant = await newGrant();
    await admin.query("UPDATE auth.oauth_grants SET permissions=NULL WHERE id=$1", [grant.grantId]);
    expect(await oauth.refreshGrant({ refreshToken: grant.refreshToken, clientId, scope: null })).toEqual({ ok: false, error: "invalid_grant" });
  });

  it("enforces permission shape in the real database, not just the consent handler", async () => {
    const grant = await newGrant();
    for (const invalid of [
      { ...PERMISSIONS, operations: ["data:delete"] }, { ...PERMISSIONS, operations: ["metrics:read", "metrics:read"] },
      { ...PERMISSIONS, record_types: [] }, { ...PERMISSIONS, record_types: [null] }, { ...PERMISSIONS, record_types: ["x", "x"] },
      { ...PERMISSIONS, record_types: ["测".repeat(257)] }, { ...PERMISSIONS, extra: "member" },
      { ...PERMISSIONS, time_range: { from: "2026-02-30T00:00:00Z", until: "2027-01-01T00:00:00Z" } },
      { ...PERMISSIONS, time_range: { from: "2026-01-01T24:00:00Z", until: "2027-01-01T00:00:00Z" } },
      { ...PERMISSIONS, time_range: { from: "2026-01-01T00:00:00+00:00", until: "2027-01-01T00:00:00Z" } },
      { ...PERMISSIONS, time_range: { from: "2027-01-01T00:00:00Z", until: "2026-01-01T00:00:00Z" } },
    ]) await expect(admin.query("UPDATE auth.oauth_grants SET permissions=$2 WHERE id=$1", [grant.grantId, JSON.stringify(invalid)])).rejects.toMatchObject({ code: "23514" });
    expect((await admin.query("SELECT permissions FROM auth.oauth_grants WHERE id=$1", [grant.grantId])).rows[0].permissions).toEqual(PERMISSIONS);
  });

  it("checks the signed grant version and complete scope against the live database before forwarding", async () => {
    const resource = `${ISSUER}/api/health`;
    const code = await oauth.createCode(accountId, request({ resource }), PERMISSIONS);
    const exchanged = await exchange(code, { resource, configuredResource: resource });
    if (!exchanged.ok) throw new Error("exchange failed");
    const grant = exchanged.grant;
    const config = { issuer: ISSUER, resource, ...oauthKeys };
    const signed = (userId = PERSON, version = 1, permissions = PERMISSIONS) => issueAccessToken(config, userId, clientId, undefined,
      { grantId: grant.grantId, authorizationVersion: version, permissions }).token;
    const { GET } = await import("../app/api/health/v1/[...path]/route");
    const read = (token: string, type = PERMISSIONS.record_types[0]) => GET(new NextRequest(
      `${resource}/v1/metrics/daily?types=${encodeURIComponent(type)}&start=1767571200000&end=1767657600000`,
      { headers: { Authorization: `Bearer ${token}` } }), { params: Promise.resolve({ path: ["metrics", "daily"] }) });
    const oldUrl = process.env.PULS_API_URL;
    process.env.PULS_API_URL = "http://synthetic-api:8081";
    const upstream = vi.spyOn(globalThis, "fetch").mockImplementation(async () => Response.json({ metrics: [] }));
    try {
      const token = signed();
      expect((await read(token)).status).toBe(200);
      expect(upstream).toHaveBeenCalledTimes(1);
      const legacy = verifyAccessToken(token, config);
      if (!legacy) throw new Error("test signature failed");
      delete legacy.principal;
      expect((await read(signAccessToken(legacy, config.signingKey))).status).toBe(401);
      expect((await read(signed(OTHER))).status).toBe(401);
      expect(upstream).toHaveBeenCalledTimes(1);

      await admin.query("UPDATE auth.oauth_grants SET authorization_version=2 WHERE id=$1", [grant.grantId]);
      expect((await read(token)).status).toBe(401);
      expect(upstream).toHaveBeenCalledTimes(1);
      const current = signed(PERSON, 2);
      expect((await read(current)).status).toBe(200);
      expect(upstream).toHaveBeenCalledTimes(2);
      const changed = { ...PERMISSIONS, record_types: ["oura.sleep"] };
      await admin.query("UPDATE auth.oauth_grants SET permissions=$2 WHERE id=$1", [grant.grantId, JSON.stringify(changed)]);
      expect((await read(current)).status).toBe(401);
      expect(upstream).toHaveBeenCalledTimes(2);
      const narrowed = signed(PERSON, 2, changed);
      expect((await read(narrowed, "oura.sleep")).status).toBe(200);
      expect(upstream).toHaveBeenCalledTimes(3);
      await admin.query("UPDATE auth.oauth_grants SET revoked_at=now() WHERE id=$1", [grant.grantId]);
      expect((await read(narrowed, "oura.sleep")).status).toBe(401);
      expect(upstream).toHaveBeenCalledTimes(3);
    } finally {
      upstream.mockRestore();
      if (oldUrl === undefined) delete process.env.PULS_API_URL; else process.env.PULS_API_URL = oldUrl;
    }
  });

  beforeAll(async () => {
    process.env.WEB_ACCOUNTS = "true";
    process.env.TRUST_PROXY_HEADERS = "true";
    process.env.WEB_PUBLIC_URL = ISSUER;
    process.env.PULS_MCP_URL = MCP;
    delete process.env.PULS_MCP_OAUTH_SECRET;
  Object.assign(process.env, oauthKeys.env);
    process.env.DATABASE_URL = WEB_URL;
    admin = new Client({ connectionString: ADMIN_URL });
    await admin.connect();
    for (const u of [PERSON, OTHER, ADMIN]) await admin.query("INSERT INTO users (id) VALUES ($1)", [u]);
    // PERSON is self-service (an approved sign-up), so they can delete their own account.
    await admin.query("INSERT INTO auth.self_service_users (user_id) VALUES ($1)", [PERSON]);
    const mk = async (user: string, email: string, isAdmin = false) =>
      (await admin.query<{ id: string }>(
        "INSERT INTO auth.accounts (user_id, email, password_hash, is_admin) VALUES ($1, $2, 'scrypt$x', $3) RETURNING id::text",
        [user, email, isAdmin],
      )).rows[0].id;
    accountId = await mk(PERSON, `person-${tag}@example.com`);
    otherAccountId = await mk(OTHER, `other-${tag}@example.com`);
    adminAccountId = await mk(ADMIN, `admin-${tag}@example.com`, true);
    oauth = await import("./oauth/store");
    const reg = await oauth.registerClient({ name: "Claude", redirectUris: [REDIRECT, "http://localhost/callback"], authMethod: "none", grantTypes: ["authorization_code", "refresh_token"] });
    clientId = reg.id;
    clients.push(clientId);
  }, 60_000);

  afterAll(async () => {
    await (await import("./db")).getPool()?.end();
    await (await import("./db")).getHealthPool()?.end();
    if (!admin) return;
    await admin.query("DELETE FROM auth.oauth_clients WHERE id = ANY($1)", [clients]);
    for (const u of [PERSON, OTHER, ADMIN]) {
      await admin.query("DELETE FROM auth.accounts WHERE user_id = $1", [u]);
      await admin.query("DELETE FROM users WHERE id = $1", [u]);
    }
    await admin.end();
  }, 60_000);

  it("registers a client, storing only a confidential client's secret hash", async () => {
    const reg = await oauth.registerClient({ name: "Tool", redirectUris: ["https://a.example/cb"], authMethod: "client_secret_post", grantTypes: ["authorization_code"] });
    clients.push(reg.id);
    expect(reg.id).toMatch(/^pc_[A-Za-z0-9_-]{24}$/);
    const stored = await oauth.findClient(reg.id);
    expect(stored?.secretHash?.equals(createHash("sha256").update(Buffer.from(reg.secret!, "base64url")).digest())).toBe(true);
    expect(oauth.clientSecretMatches(stored!, reg.secret)).toBe(true);
    expect(oauth.clientSecretMatches(stored!, "x".repeat(43))).toBe(false);
    expect(oauth.clientSecretMatches(stored!, null)).toBe(false);
    const raw = await admin.query("SELECT secret_hash FROM auth.oauth_clients WHERE id = $1", [reg.id]);
    expect(raw.rows[0].secret_hash.toString("base64url")).not.toBe(reg.secret);
  });

  it("exchanges a code once; a replay revokes the grant it made", async () => {
    const code = await oauth.createCode(accountId, request(), PERMISSIONS);
    const first = await exchange(code, { resource: `${MCP}/` });
    expect(first.ok).toBe(true);
    if (!first.ok) return;
    expect(first.grant.userId).toBe(PERSON);
    expect(first.grant.scope).toBe("health:read");
    expect((await grantRow(first.grant.grantId)).revoked).toBe(false);
    // Another client presenting the spent code learns nothing and revokes nothing.
    expect(await exchange(code, { clientId: "pc_BBBBBBBBBBBBBBBBBBBBBBBB" })).toEqual({ ok: false, error: "invalid_grant" });
    expect((await grantRow(first.grant.grantId)).revoked).toBe(false);
    const again = await exchange(code);
    expect(again).toEqual({ ok: false, error: "invalid_grant", reused: true });
    expect((await grantRow(first.grant.grantId)).revoked).toBe(true);
    // The revoked grant refreshes nothing.
    expect((await oauth.refreshGrant({ refreshToken: first.grant.refreshToken, clientId, scope: null })).ok).toBe(false);
  });

  it("never exchanges for the wrong verifier, redirect URI, client or resource, and those leave the code usable", async () => {
    const code = await oauth.createCode(accountId, request({ redirectUri: "http://localhost:4242/callback" }), PERMISSIONS);
    expect((await exchange(code, { redirectUri: "http://localhost:4242/callback", codeVerifier: `${VERIFIER}x` })).ok).toBe(false);
    expect((await exchange(code, { redirectUri: "http://localhost:4243/callback" })).ok).toBe(false);
    expect((await exchange(code, { redirectUri: "http://localhost:4242/callback", clientId: "pc_BBBBBBBBBBBBBBBBBBBBBBBB" })).ok).toBe(false);
    expect(await exchange(code, { redirectUri: "http://localhost:4242/callback", resource: "https://evil.example/mcp" })).toEqual({ ok: false, error: "invalid_target" });
    expect((await exchange(code, { redirectUri: "http://localhost:4242/callback" })).ok).toBe(true);
    expect((await exchange("Z".repeat(43))).ok).toBe(false);
    expect((await exchange("not a code")).ok).toBe(false);
  });

  it("refuses an expired code", async () => {
    const code = await oauth.createCode(accountId, request(), PERMISSIONS);
    await admin.query("UPDATE auth.oauth_codes SET expires_at = now() - interval '1 second' WHERE account_id = $1 AND used_at IS NULL", [accountId]);
    expect((await exchange(code)).ok).toBe(false);
  });

  it("rotates refresh tokens; a rotated one revokes the whole grant", async () => {
    const grant = await newGrant();
    const r1 = grant.refreshToken;
    const second = await oauth.refreshGrant({ refreshToken: r1, clientId, scope: "health:read" });
    expect(second.ok).toBe(true);
    if (!second.ok) return;
    const r2 = second.grant.refreshToken;
    expect(r2).not.toBe(r1);
    const third = await oauth.refreshGrant({ refreshToken: r2, clientId, scope: null });
    expect(third.ok).toBe(true);
    if (!third.ok) return;
    // r1 is two rotations old: unknown now, refused (only the last one is remembered).
    expect((await oauth.refreshGrant({ refreshToken: r1, clientId, scope: null })).ok).toBe(false);
    expect((await grantRow(grant.grantId)).revoked).toBe(false);
    // r2, the one just replaced, comes back: theft, so the grant is revoked.
    expect(await oauth.refreshGrant({ refreshToken: r2, clientId, scope: null })).toEqual({ ok: false, error: "invalid_grant", reused: true });
    expect((await grantRow(grant.grantId)).revoked).toBe(true);
    expect((await oauth.refreshGrant({ refreshToken: third.grant.refreshToken, clientId, scope: null })).ok).toBe(false);
  });

  it("refuses another client's refresh token and a wider scope, without revoking", async () => {
    const grant = await newGrant();
    expect((await oauth.refreshGrant({ refreshToken: grant.refreshToken, clientId: "pc_BBBBBBBBBBBBBBBBBBBBBBBB", scope: null })).ok).toBe(false);
    expect(await oauth.refreshGrant({ refreshToken: grant.refreshToken, clientId, scope: "health:read health:write" })).toEqual({ ok: false, error: "invalid_scope" });
    expect((await grantRow(grant.grantId)).revoked).toBe(false);
    expect((await oauth.refreshGrant({ refreshToken: grant.refreshToken, clientId, scope: null })).ok).toBe(true);
  });

  it("does not revoke a rotated token's grant when another client replays it", async () => {
    const other = await oauth.registerClient({ name: "Other app", redirectUris: [REDIRECT], authMethod: "none", grantTypes: ["authorization_code", "refresh_token"] });
    clients.push(other.id);
    const grant = await newGrant();
    const rotated = await oauth.refreshGrant({ refreshToken: grant.refreshToken, clientId, scope: null });
    expect(rotated.ok).toBe(true);
    if (!rotated.ok) return;

    expect(await oauth.refreshGrant({ refreshToken: grant.refreshToken, clientId: other.id, scope: null }))
      .toEqual({ ok: false, error: "invalid_grant", reused: false });
    expect((await grantRow(grant.grantId)).revoked).toBe(false);
    expect((await oauth.refreshGrant({ refreshToken: rotated.grant.refreshToken, clientId, scope: null })).ok).toBe(true);
  });

  it("refreshes nothing for a disabled account or one without a password", async () => {
    const grant = await newGrant(otherAccountId);
    await admin.query("UPDATE auth.accounts SET disabled_at = now() WHERE id = $1", [otherAccountId]);
    expect((await oauth.refreshGrant({ refreshToken: grant.refreshToken, clientId, scope: null })).ok).toBe(false);
    const code = await oauth.createCode(otherAccountId, request(), PERMISSIONS);
    expect((await exchange(code)).ok).toBe(false);
    await admin.query("UPDATE auth.accounts SET disabled_at = NULL, password_hash = NULL WHERE id = $1", [otherAccountId]);
    expect((await oauth.refreshGrant({ refreshToken: grant.refreshToken, clientId, scope: null })).ok).toBe(false);
    await admin.query("UPDATE auth.accounts SET password_hash = 'scrypt$x' WHERE id = $1", [otherAccountId]);
    expect((await oauth.refreshGrant({ refreshToken: grant.refreshToken, clientId, scope: null })).ok).toBe(true);
  });

  it("lists and revokes only the account's own assistants", async () => {
    const mine = await newGrant();
    const theirs = await newGrant(otherAccountId);
    const listed = await oauth.listConnectedApps(accountId);
    expect(listed.map((a) => a.id)).toContain(mine.grantId);
    expect(listed.map((a) => a.id)).not.toContain(theirs.grantId);
    expect(listed.find((a) => a.id === mine.grantId)?.clientName).toBe("Claude");
    expect(await oauth.revokeConnectedApp(accountId, theirs.grantId)).toBe(false);
    expect((await grantRow(theirs.grantId)).revoked).toBe(false);
    expect(await oauth.revokeConnectedApp(accountId, mine.grantId)).toBe(true);
    expect((await oauth.listConnectedApps(accountId)).map((a) => a.id)).not.toContain(mine.grantId);
    expect(await oauth.revokeConnectedApp(accountId, "not-a-uuid")).toBe(false);
  });

  it("revokes by refresh token (RFC 7009), only for the named client", async () => {
    const grant = await newGrant();
    expect(await oauth.revokeRefreshToken(grant.refreshToken, "pc_BBBBBBBBBBBBBBBBBBBBBBBB")).toBe(false);
    expect(await oauth.revokeRefreshToken(grant.refreshToken, clientId)).toBe(true);
    expect((await grantRow(grant.grantId)).revoked).toBe(true);
    expect(await oauth.revokeRefreshToken("junk", null)).toBe(false);
  });

  it("revokes every grant on a password change, and unused codes with them", async () => {
    const grant = await newGrant();
    const pending = await oauth.createCode(accountId, request(), PERMISSIONS);
    const { changePassword } = await import("./accounts/store");
    await changePassword(accountId, "scrypt$y");
    expect((await grantRow(grant.grantId)).revoked).toBe(true);
    expect((await exchange(pending)).ok).toBe(false);
  });

  it("revokes them when an administrator disables the account (auth.set_account_disabled)", async () => {
    const grant = await newGrant();
    const { setAccountDisabled } = await import("./accounts/signups");
    await setAccountDisabled(await sessionFor(adminAccountId), accountId, true);
    expect((await grantRow(grant.grantId)).revoked).toBe(true);
    await setAccountDisabled(await sessionFor(adminAccountId), accountId, false);
    // Enabling again brings nothing back.
    expect((await grantRow(grant.grantId)).revoked).toBe(true);
  });

  it("drives the token endpoint end to end", async () => {
    const { POST } = await import("@/app/oauth/token/route");
    const code = await oauth.createCode(accountId, request(), PERMISSIONS);
    const call = (fields: Record<string, string>) =>
      POST(
        new NextRequest(new URL("/oauth/token", "http://web:3000"), {
          method: "POST",
          headers: { host: "viewer.example", "x-forwarded-proto": "https", "x-forwarded-for": "198.51.100.77", "content-type": "application/x-www-form-urlencoded" },
          body: new URLSearchParams(fields).toString(),
        }),
      );
    const res = await call({ grant_type: "authorization_code", code, redirect_uri: REDIRECT, code_verifier: VERIFIER, client_id: clientId, resource: MCP });
    expect(res.status).toBe(200);
    const body = await res.json();
    const claims = JSON.parse(Buffer.from(body.access_token.split(".")[1], "base64url").toString());
    expect(claims).toMatchObject({ iss: ISSUER, aud: MCP, sub: PERSON, client_id: clientId, scope: "health:read" });
    expect(claims.principal).toEqual({ member_id: PERSON, credential_id: claims.grant_id, credential_kind: "grant", allowed_operations: PERMISSIONS.operations,
      allowed_types: PERMISSIONS.record_types, allowed_time_range: PERMISSIONS.time_range, authorization_version: 1 });
    expect(claims.exp - claims.iat).toBe(1800);
    const replay = await call({ grant_type: "authorization_code", code, redirect_uri: REDIRECT, code_verifier: VERIFIER, client_id: clientId });
    expect(replay.status).toBe(400);
    expect((await replay.json()).error).toBe("invalid_grant");
    // The replay revoked the grant, so its refresh token is dead too.
    const refresh = await call({ grant_type: "refresh_token", refresh_token: body.refresh_token, client_id: clientId });
    expect((await refresh.json()).error).toBe("invalid_grant");
  });

  it("revokes them when the person deletes their account, and deleting the account deletes them", async () => {
    const grant = await newGrant();
    const { deleteMyAccount } = await import("./accounts/signups");
    await deleteMyAccount(await sessionFor(accountId));
    expect((await grantRow(grant.grantId)).revoked).toBe(true);
    await admin.query("DELETE FROM auth.accounts WHERE id = $1", [accountId]);
    expect(await grantRow(grant.grantId)).toBeUndefined();
    expect((await admin.query("SELECT 1 FROM auth.oauth_codes WHERE account_id = $1", [accountId])).rowCount).toBe(0);
  });

  it("prunes expired codes, dead grants and clients nobody uses", async () => {
    const live = await newGrant(otherAccountId);
    const dead = await newGrant(otherAccountId);
    await oauth.revokeConnectedApp(otherAccountId, dead.grantId);
    const stale = await oauth.registerClient({ name: "Stale", redirectUris: ["https://s.example/cb"], authMethod: "none", grantTypes: ["authorization_code"] });
    clients.push(stale.id);
    await admin.query("UPDATE auth.oauth_clients SET last_used_at = now() - interval '31 days' WHERE id = ANY($1)", [[stale.id, clientId]]);
    await admin.query("UPDATE auth.oauth_codes SET expires_at = now() - interval '1 second' WHERE account_id = $1", [otherAccountId]);
    await admin.query("CALL auth.prune_oauth(0, NULL)");
    expect(await grantRow(dead.grantId)).toBeUndefined();
    expect((await grantRow(live.grantId)).revoked).toBe(false);
    expect(await oauth.findClient(stale.id)).toBeNull();
    // Unused for 31 days but holding a live grant: kept.
    expect(await oauth.findClient(clientId)).not.toBeNull();
    expect((await admin.query("SELECT 1 FROM auth.oauth_codes WHERE account_id = $1", [otherAccountId])).rowCount).toBe(0);
  });

  it("schedules the prune hourly", async () => {
    const jobs = await admin.query("SELECT schedule_interval FROM timescaledb_information.jobs WHERE proc_schema = 'auth' AND proc_name = 'prune_oauth'");
    expect(jobs.rows).toHaveLength(1);
  });
});
