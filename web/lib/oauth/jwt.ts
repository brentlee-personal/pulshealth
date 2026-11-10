import { isUuid } from "../uuid";
// Fixed Ed25519 access tokens. Verification accepts only a configured public key.

import { randomBytes, sign, verify } from "node:crypto";

import { ACCESS_TOKEN_SECONDS, OAUTH_SCOPE, type OAuthConfig } from "./config";
import { type SigningKey } from "./keys";

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
  };
  const payload = Buffer.from(JSON.stringify(ordered)).toString("base64url");
  const input = `${header(key.id)}.${payload}`;
  const signature = sign(null, Buffer.from(input), key.privateKey).toString("base64url");
  return `${input}.${signature}`;
}

/** A fresh access token for `userId`, through `clientId`. */
export function issueAccessToken(
  config: OAuthConfig,
  userId: string,
  clientId: string,
  nowSeconds = Math.floor(Date.now() / 1000),
  grantId?: string,
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
      ...(grantId ? { grant_id: grantId } : {}),
    },
    config.signingKey,
  );
  return { token, expiresIn: ACCESS_TOKEN_SECONDS };
}

/** Verify a bounded access token for exactly this resource, without trusting a cookie. */
export function verifyAccessToken(token: string, config: Pick<OAuthConfig, "issuer" | "resource" | "publicKeys">, now = Math.floor(Date.now() / 1000)): AccessTokenClaims | null {
  if (token.length > 4096) return null;
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
    return c;
  } catch { return null; }
}
