import { createHash, randomUUID, verify } from "node:crypto";
import { Client } from "pg";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { createAPICredential } from "./oauth/apiCredential";
import { ACCESS_TOKEN_SECONDS } from "./oauth/config";
import { oauthKeyFixture } from "./oauth/keyFixture";
import { createCode, exchangeCode, registerClient } from "./oauth/store";
import { issueAccessToken, type APIPrincipal } from "./oauth/jwt";
import { type ReadPermissions } from "./oauth/permissions";
import { createSession, findSession, newToken, tokenHash } from "./accounts/session";
import { requireIntegrationDatabases } from "./integrationEnv";

const databases = requireIntegrationDatabases();
const apiUrl = process.env.API_DATABASE_URL;
if (databases.available && !apiUrl) throw new Error("API_DATABASE_URL is required for personal API credential tests");
const keys = oauthKeyFixture();
const resource = "https://reader.synthetic.invalid/mcp";
const signer = { issuer: "https://viewer.synthetic.invalid", resource, signingKey: keys.signingKey, publicKeys: keys.publicKeys };
const redirectUri = "https://client.synthetic.invalid/callback";
const verifier = "v".repeat(43);
const members = [randomUUID(), randomUUID()];
const permissions = { operations: ["metrics:read"], record_types: ["HKQuantityTypeIdentifierStepCount", "HKQuantityTypeIdentifierHeartRate"],
  time_range: { from: "2020-01-01T00:00:00Z", until: "2030-01-01T00:00:00Z" } } satisfies ReadPermissions;
const narrow = { ...permissions, record_types: [permissions.record_types[0]], time_range: { from: "2026-01-01T00:00:00Z", until: "2026-02-01T00:00:00Z" } };

interface Resolved {
  assertion: string;
  principal: APIPrincipal;
  grant_id: string | null;
  client_id: string;
  expires_at: Date;
}

describe.skipIf(!databases.available || !apiUrl)("personal API credential authority (integration)", () => {
  let admin: Client;
  let reader: Client;
  let accountA: string;
  let accountB: string;
  let clientId: string;
  const grant = async (indicateResource = true) => {
    const code = await createCode(accountA, { clientId, clientName: "Synthetic reader", redirectUri,
      codeChallenge: createHash("sha256").update(verifier).digest("base64url"), scope: "health:read",
      resource: indicateResource ? resource : null, state: null }, permissions);
    const result = await exchangeCode({ code, clientId, redirectUri, codeVerifier: verifier, resource: null, configuredResource: resource });
    if (!result.ok) throw new Error("Synthetic grant exchange failed");
    return result.grant;
  };
  const subject = (parent: Awaited<ReturnType<typeof grant>>, member = members[0], client = clientId, version = parent.authorizationVersion, audience = resource) =>
    issueAccessToken({ ...signer, resource: audience }, member, client, undefined,
      { grantId: parent.grantId, permissions, authorizationVersion: version }).token;
  const requestFor = (parent: Awaited<ReturnType<typeof grant>>) => ({ kind: "grant", subjectToken: subject(parent), permissions: narrow,
  } satisfies Parameters<typeof createAPICredential>[1]);
  const resolve = (token: string) => reader.query<Resolved>("SELECT * FROM auth.resolve_api_credential($1)", [tokenHash(token)]).then((r) => r.rows);

  beforeAll(async () => {
    process.env.DATABASE_URL = databases.webUrl;
    process.env.WEB_ACCOUNTS = "true";
    admin = new Client({ connectionString: databases.adminUrl });
    reader = new Client({ connectionString: apiUrl });
    await admin.connect();
    await reader.connect();
    for (const member of members) await admin.query("INSERT INTO public.users(id) VALUES($1)", [member]);
    const accounts: string[] = [];
    for (const member of members) accounts.push((await admin.query<{ id: string }>(
      "INSERT INTO auth.accounts(user_id,email,password_hash) VALUES($1,$2,'synthetic-account-hash') RETURNING id::text",
      [member, `${member}@synthetic.invalid`])).rows[0].id);
    [accountA, accountB] = accounts;
    clientId = (await registerClient({ name: "Synthetic reader", redirectUris: [redirectUri], authMethod: "none", grantTypes: ["authorization_code", "refresh_token"] })).id;
    expect((await reader.query("SELECT current_user")).rows).toEqual([{ current_user: "api_reader" }]);
  });

  afterAll(async () => {
    await (await import("./db")).getPool()?.end();
    await (await import("./db")).getHealthPool()?.end();
    if (reader) await reader.end();
    if (admin) {
      if (clientId) await admin.query("DELETE FROM auth.oauth_clients WHERE id=$1", [clientId]);
      for (const member of members) {
        await admin.query("DELETE FROM auth.accounts WHERE user_id=$1", [member]);
        await admin.query("DELETE FROM public.users WHERE id=$1", [member]);
      }
      await admin.end();
    }
  });

  it("refuses a browser session without personal sign-in proof", async () => {
    const token = newToken();
    const id = tokenHash(token);
    if (!id) throw new Error("Synthetic session token is invalid");
    await admin.query("INSERT INTO auth.sessions(id,account_id,expires_at) VALUES($1,$2,now()+interval '1 hour')", [id, accountA]);
    try {
      expect(await findSession(token)).toBeNull();
    } finally {
      await admin.query("DELETE FROM auth.sessions WHERE id=$1", [id]);
    }
  });

  it("refuses an API credential derived from an untyped legacy session", async () => {
    const token = newToken();
    const id = tokenHash(token);
    if (!id) throw new Error("Synthetic session token is invalid");
    await admin.query("INSERT INTO auth.sessions(id,account_id,expires_at) VALUES($1,$2,now()+interval '1 hour')", [id, accountA]);
    try {
      expect(await createAPICredential(signer, { kind: "session", sessionId: id, permissions: narrow })).toBeNull();
    } finally {
      await admin.query("DELETE FROM auth.sessions WHERE id=$1", [id]);
    }
  });

  it("keeps a stored legacy shared account sealed without an environment flag", async () => {
    const cookie = await createSession(accountA, { userAgent: null, ip: null });
    const sessionId = tokenHash(cookie);
    if (!sessionId) throw new Error("Synthetic session token is invalid");
    const previous = (await admin.query("SELECT demo_user_id FROM auth.device_pairing_policy WHERE singleton")).rows[0].demo_user_id;
    try {
      const issued = await createAPICredential(signer, { kind: "session", sessionId, permissions: narrow });
      if (!issued) throw new Error("Synthetic personal credential was not issued");
      await admin.query("UPDATE auth.device_pairing_policy SET demo_user_id=$1 WHERE singleton", [members[0]]);
      expect(await findSession(cookie)).toBeNull();
      expect(await createAPICredential(signer, { kind: "session", sessionId, permissions: narrow })).toBeNull();
      expect(await resolve(issued.token)).toHaveLength(0);
      expect((await admin.query("SELECT 1 FROM public.users WHERE id=$1", [members[0]])).rowCount).toBe(1);
    } finally {
      await admin.query("UPDATE auth.device_pairing_policy SET demo_user_id=$1 WHERE singleton", [previous]);
      await admin.query("DELETE FROM auth.sessions WHERE id=$1", [sessionId]);
    }
  });

  it("stops an issued credential when its session no longer has personal sign-in proof", async () => {
    const cookie = await createSession(accountA, { userAgent: null, ip: null });
    const sessionId = tokenHash(cookie);
    if (!sessionId) throw new Error("Synthetic session token is invalid");
    try {
      const issued = await createAPICredential(signer, { kind: "session", sessionId, permissions: narrow });
      if (!issued) throw new Error("Synthetic personal credential was not issued");
      expect(await findSession(cookie)).toMatchObject({ userId: members[0] });
      expect(await resolve(issued.token)).toHaveLength(1);
      await admin.query("UPDATE auth.sessions SET purpose='legacy',expires_at=now()+interval '1 day' WHERE id=$1", [sessionId]);
      expect(await findSession(cookie, true)).toBeNull();
      expect(await resolve(issued.token)).toHaveLength(0);
      expect(await createAPICredential(signer, { kind: "session", sessionId, permissions: narrow })).toBeNull();
    } finally {
      await admin.query("DELETE FROM auth.sessions WHERE id=$1", [sessionId]);
    }
  });

  it("issues a separate opaque bearer with a public-key-verifiable API assertion", async () => {
    const parent = await grant(false);
    expect(parent.resource).toBe(resource);
    const issued = await createAPICredential(signer, requestFor(parent));
    if (!issued) throw new Error("Synthetic API credential was not issued");
    expect(issued.token).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(issued.expiresIn).toBe(60);
    const [row] = await resolve(issued.token);
    expect(row.principal).toMatchObject({ member_id: members[0], credential_kind: "exchange", allowed_operations: narrow.operations,
      allowed_types: narrow.record_types, allowed_time_range: narrow.time_range, authorization_version: 1 });
    expect(row.grant_id).toBe(parent.grantId);
    expect(row.client_id).toBe(clientId);
    expect(row.assertion).not.toBe(issued.token);
    const [header, payload, signature] = row.assertion.split(".");
    expect(verify(null, Buffer.from(`${header}.${payload}`), keys.publicKey, Buffer.from(signature, "base64url"))).toBe(true);
    const claims: unknown = JSON.parse(Buffer.from(payload, "base64url").toString());
    expect(claims).toMatchObject({ iss: signer.issuer, aud: "family-health-api", sub: members[0], client_id: clientId,
      jti: row.principal.credential_id, grant_id: parent.grantId, principal: row.principal });
    expect(await resolve(row.assertion)).toHaveLength(0);
    await expect(reader.query("SELECT * FROM auth.api_credentials")).rejects.toMatchObject({ code: "42501" });
  });

  it("refuses another member, client, audience, version or broader permission before issuing", async () => {
    const parent = await grant();
    const request = requestFor(parent);
    const before = await admin.query("SELECT count(*) AS n FROM auth.api_credentials");
    for (const changed of [
      { ...request, subjectToken: subject(parent, members[1]) }, { ...request, subjectToken: subject(parent, members[0], "pc_other") },
      { ...request, subjectToken: subject(parent, members[0], clientId, 1, "https://other.synthetic.invalid/mcp") },
      { ...request, subjectToken: subject(parent, members[0], clientId, 2) }, { ...request, subjectToken: "not-a-signed-personal-grant" },
      { ...request, permissions: { ...narrow, record_types: ["oura.sleep"] } },
      { ...request, permissions: { ...narrow, operations: ["samples:read"] } },
      { ...request, permissions: { ...narrow, time_range: { from: "2019-12-31T23:59:59.999999999Z", until: narrow.time_range.until } } },
    ] satisfies Parameters<typeof createAPICredential>[1][]) expect(await createAPICredential(signer, changed)).toBeNull();
    expect((await admin.query("SELECT count(*) AS n FROM auth.api_credentials")).rows).toEqual(before.rows);
  });

  it("never gives a child credential a longer lifetime than its parent", async () => {
    const parent = await grant();
    await admin.query("UPDATE auth.oauth_grants SET refresh_expires_at=statement_timestamp()+interval '55 seconds' WHERE id=$1", [parent.grantId]);
    const issued = await createAPICredential(signer, requestFor(parent));
    if (!issued) throw new Error("Short-lived synthetic API credential was not issued");
    expect(issued.expiresIn).toBeGreaterThan(0);
    expect(issued.expiresIn).toBeLessThanOrEqual(55);
    expect((await admin.query(
      "SELECT c.expires_at<=g.refresh_expires_at AS bounded FROM auth.api_credentials c JOIN auth.oauth_grants g ON g.id=c.grant_id WHERE c.token_hash=$1",
      [tokenHash(issued.token)])).rows).toEqual([{ bounded: true }]);
    const subjectToken = issueAccessToken(signer, members[0], clientId,
      Math.floor(Date.now() / 1000) - ACCESS_TOKEN_SECONDS + 25,
      { grantId: parent.grantId, permissions, authorizationVersion: parent.authorizationVersion }).token;
    const shorter = await createAPICredential(signer, { ...requestFor(parent), subjectToken });
    if (!shorter) throw new Error("Short-lived subject credential was not exchanged");
    expect(shorter.expiresIn).toBeGreaterThan(0);
    expect(shorter.expiresIn).toBeLessThanOrEqual(25);
  });

  it("rechecks parent version, audience and revocation after a credential has been issued", async () => {
    const parent = await grant();
    const issued = await createAPICredential(signer, requestFor(parent));
    if (!issued) throw new Error("Synthetic API credential was not issued");
    expect(await resolve(issued.token)).toHaveLength(1);
    await admin.query("UPDATE auth.oauth_grants SET authorization_version=2 WHERE id=$1", [parent.grantId]);
    expect(await resolve(issued.token)).toHaveLength(0);
    const current = await createAPICredential(signer, { ...requestFor(parent), subjectToken: subject(parent, members[0], clientId, 2) });
    if (!current) throw new Error("Current synthetic credential was not issued");
    expect(await resolve(current.token)).toHaveLength(1);
    await admin.query("UPDATE auth.oauth_grants SET resource='https://other.synthetic.invalid/mcp' WHERE id=$1", [parent.grantId]);
    expect(await resolve(current.token)).toHaveLength(0);
    await admin.query("UPDATE auth.oauth_grants SET resource=$2,revoked_at=now() WHERE id=$1", [parent.grantId, resource]);
    expect(await resolve(current.token)).toHaveLength(0);
  });

  it("checks expiry with each statement in an already-open read transaction", async () => {
    const issued = await createAPICredential(signer, requestFor(await grant()));
    if (!issued) throw new Error("Synthetic API credential was not issued");
    await reader.query("BEGIN ISOLATION LEVEL READ COMMITTED READ ONLY");
    try {
      expect(await resolve(issued.token)).toHaveLength(1);
      const started = (await reader.query<{ started: string }>("SELECT transaction_timestamp()::text AS started")).rows[0].started;
      const changed = await admin.query<{ after_start: boolean }>(
        `UPDATE auth.api_credentials SET created_at=statement_timestamp()-interval '60 seconds',expires_at=statement_timestamp()
          WHERE token_hash=$1 RETURNING expires_at>$2::timestamptz AS after_start`, [tokenHash(issued.token), started]);
      expect(changed.rows).toEqual([{ after_start: true }]);
      expect(await resolve(issued.token)).toHaveLength(0);
    } finally { await reader.query("ROLLBACK"); }
  });

  it("binds session credentials to the owning account and stops them on logout", async () => {
    const cookie = await createSession(accountA, { userAgent: null, ip: null });
    const sessionId = tokenHash(cookie);
    if (!sessionId) throw new Error("Synthetic session hash was not created");
    const issued = await createAPICredential(signer, { kind: "session", sessionId, permissions: { ...narrow, operations: ["data:export"] } });
    if (!issued) throw new Error("Synthetic session credential was not issued");
    expect(issued.expiresIn).toBe(1800);
    const [row] = await resolve(issued.token);
    expect(row.principal).toMatchObject({ member_id: members[0], credential_kind: "session", allowed_operations: ["data:export"], authorization_version: 1 });
    expect(row.grant_id).toBeNull();
    await expect(admin.query("UPDATE auth.api_credentials SET account_id=$2 WHERE token_hash=$1", [tokenHash(issued.token), accountB]))
      .rejects.toMatchObject({ code: "23503" });
    await admin.query("DELETE FROM auth.sessions WHERE id=$1", [sessionId]);
    expect(await resolve(issued.token)).toHaveLength(0);
    expect((await admin.query("SELECT 1 FROM auth.api_credentials WHERE token_hash=$1", [tokenHash(issued.token)])).rowCount).toBe(0);
  });

  it("refuses child scope tampering, credential revocation and disabled accounts", async () => {
    const issued = await createAPICredential(signer, requestFor(await grant()));
    if (!issued) throw new Error("Synthetic API credential was not issued");
    await admin.query("UPDATE auth.api_credentials SET permissions=$2 WHERE token_hash=$1", [tokenHash(issued.token), JSON.stringify({ ...narrow, operations: ["data:export"] })]);
    expect(await resolve(issued.token)).toHaveLength(0);
    await admin.query("UPDATE auth.api_credentials SET permissions=$2,revoked_at=now() WHERE token_hash=$1", [tokenHash(issued.token), JSON.stringify(narrow)]);
    expect(await resolve(issued.token)).toHaveLength(0);
    await admin.query("UPDATE auth.api_credentials SET revoked_at=NULL WHERE token_hash=$1", [tokenHash(issued.token)]);
    await admin.query("UPDATE auth.accounts SET disabled_at=now() WHERE id=$1", [accountA]);
    try { expect(await resolve(issued.token)).toHaveLength(0); }
    finally { await admin.query("UPDATE auth.accounts SET disabled_at=NULL WHERE id=$1", [accountA]); }
  });
});
