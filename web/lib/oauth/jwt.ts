import { isUuid } from "../uuid";
// Fixed Ed25519 access tokens. Verification accepts only a configured public key.

import { randomBytes, sign, verify } from "node:crypto";

import { ACCESS_TOKEN_SECONDS, OAUTH_SCOPE, type OAuthConfig } from "./config";
import { type SigningKey } from "./keys";
import { readPermissions, type ReadPermissions, type ReadOperation } from "./permissions";

export interface GrantAuthorization {
  grantId: string;
  permissions: ReadPermissions;
  authorizationVersion: number;
}

export interface GrantPrincipal {
  member_id: string;
  credential_id: string;
  credential_kind: "grant";
  allowed_operations: ReadOperation[];
  allowed_types: string[];
  allowed_time_range: { from: string; until: string };
  authorization_version: number;
}

const MAX_ACCESS_TOKEN_BYTES = 16_384;

export interface AccessTokenClaims {
  iss: string;
  aud: string;
  sub: string;
  client_id: string;
  scope: string;
  iat: number;
  exp: number;
  jti: string;
  grant_id?: string;
  principal?: GrantPrincipal;
}

function header(keyId: string): string {
  return Buffer.from(JSON.stringify({ alg: "EdDSA", typ: "at+jwt", kid: keyId })).toString("base64url");
}

/** Signs `claims`, serialised in the contract's key order whatever order they came in. */
export function signAccessToken(claims: AccessTokenClaims, key: SigningKey): string {
  const ordered = {
    iss: claims.iss,
    aud: claims.aud,
    sub: claims.sub,
    client_id: claims.client_id,
    scope: claims.scope,
    iat: claims.iat,
    exp: claims.exp,
    jti: claims.jti,
    ...(claims.grant_id ? { grant_id: claims.grant_id } : {}),
    ...(claims.principal ? { principal: claims.principal } : {}),
  };
  const payload = Buffer.from(JSON.stringify(ordered)).toString("base64url");
  const input = `${header(key.id)}.${payload}`;
  const signature = sign(null, Buffer.from(input), key.privateKey).toString("base64url");
  const token = `${input}.${signature}`;
  if (token.length > MAX_ACCESS_TOKEN_BYTES) throw new Error("Authorization scope is too large");
  return token;
}

/** A fresh access token for `userId`, through `clientId`. */
export function issueAccessToken(
  config: OAuthConfig,
  userId: string,
  clientId: string,
  nowSeconds = Math.floor(Date.now() / 1000),
  authorization?: GrantAuthorization,
): { token: string; expiresIn: number } {
  const token = signAccessToken(
    {
      iss: config.issuer,
      aud: config.resource,
      sub: userId.toLowerCase(),
      client_id: clientId,
      scope: OAUTH_SCOPE,
      iat: nowSeconds,
      exp: nowSeconds + ACCESS_TOKEN_SECONDS,
      jti: randomBytes(16).toString("base64url"),
      ...(authorization ? { grant_id: authorization.grantId, principal: {
        member_id: userId.toLowerCase(), credential_id: authorization.grantId, credential_kind: "grant",
        allowed_operations: authorization.permissions.operations, allowed_types: authorization.permissions.record_types,
        allowed_time_range: authorization.permissions.time_range, authorization_version: authorization.authorizationVersion,
      } } : {}),
    },
    config.signingKey,
  );
  return { token, expiresIn: ACCESS_TOKEN_SECONDS };
}

/** Verify a bounded access token for exactly this resource, without trusting a cookie. */
export function verifyAccessToken(token: string, config: Pick<OAuthConfig, "issuer" | "resource" | "publicKeys">, now = Math.floor(Date.now() / 1000)): AccessTokenClaims | null {
  if (token.length > MAX_ACCESS_TOKEN_BYTES) return null;
  const parts = token.split(".");
  if (parts.length !== 3 || !parts.every((p) => /^[A-Za-z0-9_-]+$/.test(p) && Buffer.from(p, "base64url").toString("base64url") === p)) return null;
  const key = [...config.publicKeys].find(([id]) => parts[0] === header(id))?.[1];
  if (!key) return null;
  const signature = Buffer.from(parts[2], "base64url");
  if (signature.length !== 64 || !verify(null, Buffer.from(`${parts[0]}.${parts[1]}`), key, signature)) return null;
  try {
    const c = JSON.parse(Buffer.from(parts[1], "base64url").toString("utf8"));
    if (c.iss !== config.issuer || c.aud !== config.resource || !isUuid(c.sub) ||
        c.scope !== OAUTH_SCOPE || typeof c.client_id !== "string" || !c.client_id ||
        typeof c.jti !== "string" || !c.jti || !Number.isInteger(c.iat) || !Number.isInteger(c.exp) ||
        c.iat > now + 60 || c.exp <= now || c.exp <= c.iat || c.exp - c.iat > ACCESS_TOKEN_SECONDS) return null;
    if (c.principal !== undefined) {
      const p = c.principal;
      if (typeof p !== "object" || p === null || Array.isArray(p) || Object.keys(p).length !== 7 ||
          p.member_id !== c.sub || !isUuid(p.credential_id) || p.credential_id !== c.grant_id || p.credential_kind !== "grant" ||
          !Number.isSafeInteger(p.authorization_version) || p.authorization_version < 1 ||
          !readPermissions({ operations: p.allowed_operations, record_types: p.allowed_types, time_range: p.allowed_time_range })) return null;
    }
    return c;
  } catch { return null; }
}
