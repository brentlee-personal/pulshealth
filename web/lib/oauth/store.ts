// The OAuth rows in the account store (019_oauth.sql): clients, grants and
// authorization codes, written as web_app like sessions. Every secret here
// — client secret, code, refresh token — is 32 random bytes handed out once
// and stored as its SHA-256 (`tokenHash`), so the tables sign nobody in.
// Nothing here is ever logged. Server-only.

import { randomBytes, timingSafeEqual } from "node:crypto";

import { query, transaction, type QueryFn } from "../db";
import { newToken, tokenHash } from "../accounts/session";
import { CODE_SECONDS, REFRESH_TOKEN_DAYS } from "./config";
import { type AuthorizeRequest, type ClientRegistration, type KnownClient, resourceMatches, verifyPkce } from "./validate";
import { readPermissions, type ReadPermissions } from "./permissions";

export interface StoredClient extends KnownClient {
  authMethod: "none" | "client_secret_post" | "client_secret_basic";
  secretHash: Buffer | null;
  createdAt: number;
}

const CLIENT_ID_SHAPE = /^pc_[A-Za-z0-9_-]{16,64}$/;

export function isClientId(value: unknown): value is string {
  return typeof value === "string" && CLIENT_ID_SHAPE.test(value);
}

/** Registers a client; returns its id and, for a confidential one, its secret (shown once). */
export async function registerClient(reg: ClientRegistration): Promise<{ id: string; secret: string | null; createdAt: number }> {
  const id = `pc_${randomBytes(18).toString("base64url")}`;
  const secret = reg.authMethod === "none" ? null : newToken();
  const rows = await query<{ created_at: Date }>(
    `INSERT INTO auth.oauth_clients (id, name, redirect_uris, auth_method, secret_hash, grant_types)
     VALUES ($1, $2, $3, $4, $5, $6)
     RETURNING created_at`,
    [id, reg.name, reg.redirectUris, reg.authMethod, secret ? tokenHash(secret) : null, reg.grantTypes],
  );
  return { id, secret, createdAt: rows[0].created_at.getTime() };
}

export async function findClient(id: string | null | undefined): Promise<StoredClient | null> {
  if (!isClientId(id)) return null;
  const rows = await query<{
    id: string;
    name: string;
    redirect_uris: string[];
    grant_types: string[];
    auth_method: StoredClient["authMethod"];
    secret_hash: Buffer | null;
    created_at: Date;
  }>(
    `SELECT id, name, redirect_uris, grant_types, auth_method, secret_hash, created_at
       FROM auth.oauth_clients WHERE id = $1`,
    [id],
  );
  const r = rows[0];
  if (!r) return null;
  return {
    id: r.id,
    name: r.name,
    redirectUris: r.redirect_uris,
    grantTypes: r.grant_types,
    authMethod: r.auth_method,
    secretHash: r.secret_hash,
    createdAt: r.created_at.getTime(),
  };
}

/**
 * Whether `secret` is the client's: true for a public client (no secret is
 * asked of it), else a constant-time comparison of hashes.
 */
export function clientSecretMatches(client: StoredClient, secret: string | null): boolean {
  if (client.authMethod === "none") return true;
  if (!client.secretHash || !secret) return false;
  const presented = tokenHash(secret);
  return presented !== null && presented.length === client.secretHash.length && timingSafeEqual(presented, client.secretHash);
}

/**
 * Issues an authorization code for a request the signed-in account just
 * allowed (5 minutes, single use). Returns the code, shown once in the
 * redirect.
 */
export async function createCode(accountId: string, req: AuthorizeRequest, permissions: ReadPermissions): Promise<string> {
  const code = newToken();
  await transaction(async (q) => {
    await q(
      `INSERT INTO auth.oauth_codes (id, client_id, account_id, redirect_uri, code_challenge, scope, resource, expires_at, permissions)
       VALUES ($1, $2, $3, $4, $5, $6, $7, now() + make_interval(secs => $8), $9)`,
      [tokenHash(code), req.clientId, accountId, req.redirectUri, req.codeChallenge, req.scope, req.resource, CODE_SECONDS, JSON.stringify(permissions)],
    );
    await q("UPDATE auth.oauth_clients SET last_used_at = now() WHERE id = $1", [req.clientId]);
  });
  return code;
}

/** What a successful exchange or refresh hands the token endpoint. */
export interface IssuedGrant {
  grantId: string;
  userId: string;
  scope: string;
  refreshToken: string;
  resource?: string | null;
  permissions: ReadPermissions;
  authorizationVersion: number;
}

export type GrantFailure =
  /** Unknown, expired, used, revoked, bound to something else, or the account cannot sign in. */
  | "invalid_grant"
  /** Asked for more than the grant holds. */
  | "invalid_scope"
  /** A resource other than the one the code was issued for. */
  | "invalid_target";

export type GrantResult = { ok: true; grant: IssuedGrant } | { ok: false; error: GrantFailure; reused?: boolean };

async function startGrant(
  q: QueryFn,
  accountId: string,
  clientId: string,
  scope: string,
  resource: string | null,
  permissions: ReadPermissions,
): Promise<{ id: string; refreshToken: string }> {
  const refreshToken = newToken();
  const rows = await q<{ id: string }>(
    `INSERT INTO auth.oauth_grants (account_id, client_id, scope, resource, refresh_hash, refresh_expires_at, permissions)
     VALUES ($1, $2, $3, $4, $5, now() + make_interval(days => $6), $7)
     RETURNING id::text`,
    [accountId, clientId, scope, resource, tokenHash(refreshToken), REFRESH_TOKEN_DAYS, JSON.stringify(permissions)],
  );
  await q("UPDATE auth.oauth_clients SET last_used_at = now() WHERE id = $1", [clientId]);
  return { id: rows[0].id, refreshToken };
}

/**
 * The authorization_code grant: spends the code and starts a grant. The code
 * row is locked, so a code exchanged twice at once is spent once. A code
 * already spent, presented again by the client it was issued to, revokes the
 * grant its first exchange made (RFC 6749 §4.1.2: someone else has it). The
 * caller has already authenticated that client (a confidential client's
 * secret checked), so another client presenting a spent code — a code is
 * no secret once it has crossed a browser — gets a plain invalid_grant and
 * cannot revoke someone's grant with it. A wrong verifier, redirect URI or
 * client leaves an unspent code unspent: a thief who has the code but not
 * the verifier must not be able to burn it.
 */
export async function exchangeCode(input: {
  code: string;
  clientId: string;
  redirectUri: string;
  codeVerifier: string;
  resource: string | null;
  configuredResource: string | string[];
}): Promise<GrantResult> {
  const id = tokenHash(input.code);
  if (!id) return { ok: false, error: "invalid_grant" };
  return transaction(async (q) => {
    const rows = await q<{
      client_id: string;
      account_id: string;
      user_id: string;
      redirect_uri: string;
      code_challenge: string;
      scope: string;
      resource: string | null;
      live: boolean;
      used: boolean;
      grant_id: string | null;
      account_ok: boolean;
      permissions: unknown;
    }>(
      `SELECT c.client_id, c.account_id::text, a.user_id::text, c.redirect_uri, c.code_challenge, c.scope, c.resource, c.permissions,
              c.expires_at > now() AS live, c.used_at IS NOT NULL AS used, c.grant_id::text,
              a.disabled_at IS NULL AND a.password_hash IS NOT NULL AS account_ok
         FROM auth.oauth_codes c
         JOIN auth.accounts a ON a.id = c.account_id
        WHERE c.id = $1
        FOR UPDATE OF c`,
      [id],
    );
    const code = rows[0];
    if (!code) return { ok: false, error: "invalid_grant" } as const;
    const permissions = readPermissions(code.permissions);
    if (!permissions) return { ok: false, error: "invalid_grant" } as const;
    if (code.used) {
      if (code.client_id !== input.clientId) return { ok: false, error: "invalid_grant" } as const;
      if (code.grant_id) {
        await q("UPDATE auth.oauth_grants SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL", [code.grant_id]);
      }
      return { ok: false, error: "invalid_grant", reused: true } as const;
    }
    if (
      !code.live ||
      code.client_id !== input.clientId ||
      code.redirect_uri !== input.redirectUri ||
      !verifyPkce(input.codeVerifier, code.code_challenge)
    ) {
      return { ok: false, error: "invalid_grant" } as const;
    }
    const allowed = typeof input.configuredResource === "string" ? [input.configuredResource] : input.configuredResource;
    const audience = code.resource ?? allowed[0];
    if (!allowed.includes(audience) || !resourceMatches(input.resource, audience)) {
      return { ok: false, error: "invalid_target" } as const;
    }
    if (!code.account_ok) return { ok: false, error: "invalid_grant" } as const;

    await q("UPDATE auth.oauth_codes SET used_at = now() WHERE id = $1", [id]);
    const grant = await startGrant(q, code.account_id, code.client_id, code.scope, code.resource, permissions);
    await q("UPDATE auth.oauth_codes SET grant_id = $2 WHERE id = $1", [id, grant.id]);
    return {
      ok: true,
      grant: { grantId: grant.id, userId: code.user_id, scope: code.scope, resource: code.resource, refreshToken: grant.refreshToken, permissions, authorizationVersion: 1 },
    } as const;
  });
}

/**
 * The refresh_token grant: rotates the refresh token (the old one stops
 * working) and slides its expiry. A refresh token that was already rotated
 * away revokes its whole grant: either the client lost track (and must sign
 * in again) or someone else holds a copy.
 */
export async function refreshGrant(input: { refreshToken: string; clientId: string; scope: string | null; resource?: string | null; configuredResource?: string }): Promise<GrantResult> {
  const hash = tokenHash(input.refreshToken);
  if (!hash) return { ok: false, error: "invalid_grant" };
  return transaction(async (q) => {
    const rows = await q<{
      id: string;
      client_id: string;
      user_id: string;
      scope: string;
      resource: string | null;
      live: boolean;
      account_ok: boolean;
      permissions: unknown;
      authorization_version: string;
    }>(
      `SELECT g.id::text, g.client_id, a.user_id::text, g.scope, g.resource, g.permissions, g.authorization_version::text,
              g.revoked_at IS NULL AND g.refresh_expires_at > now() AS live,
              a.disabled_at IS NULL AND a.password_hash IS NOT NULL AS account_ok
         FROM auth.oauth_grants g
         JOIN auth.accounts a ON a.id = g.account_id
        WHERE g.refresh_hash = $1
        FOR UPDATE OF g`,
      [hash],
    );
    const grant = rows[0];
    if (!grant) {
      // A token belonging to another client must not let this client revoke
      // its grant, whether the token is current or has just been rotated.
      const reused = await q(
        "UPDATE auth.oauth_grants SET revoked_at = now() WHERE previous_refresh_hash = $1 AND client_id = $2 AND revoked_at IS NULL RETURNING 1",
        [hash, input.clientId],
      );
      return { ok: false, error: "invalid_grant", reused: reused.length > 0 } as const;
    }
    if (grant.client_id !== input.clientId || !grant.live || !grant.account_ok) {
      return { ok: false, error: "invalid_grant" } as const;
    }
    const permissions = readPermissions(grant.permissions);
    const authorizationVersion = Number(grant.authorization_version);
    if (!permissions || !Number.isSafeInteger(authorizationVersion) || authorizationVersion < 1) return { ok: false, error: "invalid_grant" } as const;
    if (!resourceMatches(input.resource, grant.resource ?? input.configuredResource ?? "")) {
      return { ok: false, error: "invalid_target" } as const;
    }
    if (input.scope !== null) {
      const held = new Set(grant.scope.split(" "));
      const asked = input.scope.split(" ").filter(Boolean);
      if (!asked.every((s) => held.has(s))) return { ok: false, error: "invalid_scope" } as const;
    }
    const refreshToken = newToken();
    await q(
      `UPDATE auth.oauth_grants
          SET previous_refresh_hash = refresh_hash, refresh_hash = $2,
              refresh_expires_at = now() + make_interval(days => $3), last_used_at = now()
        WHERE id = $1`,
      [grant.id, tokenHash(refreshToken), REFRESH_TOKEN_DAYS],
    );
    await q("UPDATE auth.oauth_clients SET last_used_at = now() WHERE id = $1", [grant.client_id]);
    return { ok: true, grant: { grantId: grant.id, userId: grant.user_id, scope: grant.scope, resource: grant.resource, refreshToken, permissions, authorizationVersion } } as const;
  });
}

/**
 * RFC 7009: revokes the grant whose current refresh token this is. A token
 * of another client (when one is named) or one that is not a refresh token
 * of ours — an access token included, which cannot be recalled — changes
 * nothing; the endpoint answers 200 either way.
 */
export async function revokeRefreshToken(token: string, clientId: string | null): Promise<boolean> {
  const hash = tokenHash(token);
  if (!hash) return false;
  const rows = await query(
    `UPDATE auth.oauth_grants SET revoked_at = now()
      WHERE refresh_hash = $1 AND revoked_at IS NULL AND ($2::text IS NULL OR client_id = $2)
      RETURNING 1`,
    [hash, clientId],
  );
  return rows.length > 0;
}

/** An assistant connected to the account, as the account page lists it. */
export interface ConnectedApp {
  id: string;
  clientName: string;
  createdAt: number;
  lastUsedAt: number;
}

export async function listConnectedApps(accountId: string): Promise<ConnectedApp[]> {
  const rows = await query<{ id: string; name: string; created_at: Date; last_used_at: Date }>(
    `SELECT g.id::text, c.name, g.created_at, g.last_used_at
       FROM auth.oauth_grants g
       JOIN auth.oauth_clients c ON c.id = g.client_id
      WHERE g.account_id = $1 AND g.revoked_at IS NULL AND g.refresh_expires_at > now()
      ORDER BY g.last_used_at DESC`,
    [accountId],
  );
  return rows.map((r) => ({ id: r.id, clientName: r.name, createdAt: r.created_at.getTime(), lastUsedAt: r.last_used_at.getTime() }));
}

/** Revokes one of the account's own grants; true when one was revoked. */
export async function revokeConnectedApp(accountId: string, grantId: string): Promise<boolean> {
  if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(grantId)) return false;
  const rows = await query(
    "UPDATE auth.oauth_grants SET revoked_at = now() WHERE id = $1 AND account_id = $2 AND revoked_at IS NULL RETURNING 1",
    [grantId, accountId],
  );
  return rows.length > 0;
}

/**
 * Revokes every grant of the account and drops its unused codes, inside the
 * caller's transaction: a password change or reset (lib/accounts/store.ts).
 * Disabling and deleting do the same in the database (019_oauth.sql).
 */
export async function revokeAllGrants(q: QueryFn, accountId: string): Promise<void> {
  await q("UPDATE auth.oauth_grants SET revoked_at = now() WHERE account_id = $1 AND revoked_at IS NULL", [accountId]);
  await q("DELETE FROM auth.oauth_codes WHERE account_id = $1", [accountId]);
}
