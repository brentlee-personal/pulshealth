import { randomUUID } from "node:crypto";
import { newToken, tokenHash } from "../accounts/session";
import { transaction } from "../db";
import { oauthResources, type OAuthConfig } from "./config";
import { signAccessToken, verifyAccessToken, type AccessTokenClaims, type APIPrincipal } from "./jwt";
import { isUuid } from "../uuid";
import { type APIPermissions, type ReadPermissions } from "./permissions";

type APIRequest =
  | { kind: "grant"; subjectToken: string; permissions: ReadPermissions }
  | { kind: "session"; sessionId: Buffer; permissions: APIPermissions };

interface AuthorityRow {
  account_id: string;
  member_id: string;
  client_id: string;
  authorization_version: string;
  parent_expires_at: string;
  parent_resource: string | null;
}

/** Only the authorization module has this signer. The bearer is a fresh secret, never the assertion. */
export async function createAPICredential(
  signer: OAuthConfig,
  request: APIRequest,
): Promise<{ token: string; expiresIn: number } | null> {
  return transaction(async (q) => {
    let row: AuthorityRow | undefined;
    let grantId: string | null = null;
    let subjectExpiry = Infinity;
    if (request.kind === "grant") {
      let claims: AccessTokenClaims | null = null;
      for (const resource of oauthResources(signer)) {
        claims = verifyAccessToken(request.subjectToken, { ...signer, resource });
        if (claims) break;
      }
      if (!claims?.principal || !isUuid(claims.grant_id)) return null;
      grantId = claims.grant_id;
      subjectExpiry = claims.exp;
      [row] = await q<AuthorityRow>(
        `SELECT g.account_id::text, a.user_id::text AS member_id, g.client_id, g.authorization_version::text, g.resource AS parent_resource,
                floor(extract(epoch FROM g.refresh_expires_at))::bigint::text AS parent_expires_at
           FROM auth.oauth_grants g JOIN auth.accounts a ON a.id=g.account_id
          WHERE g.id=$1 AND a.user_id=$2 AND g.client_id=$3 AND g.authorization_version=$4
            AND g.permissions=$5::jsonb AND public.puls_api_permissions_within($6::jsonb,g.permissions)
            AND g.resource=$7
            AND g.revoked_at IS NULL AND g.refresh_expires_at>statement_timestamp()
            AND a.disabled_at IS NULL AND a.password_hash IS NOT NULL
          FOR SHARE OF g,a`,
        [grantId, claims.sub, claims.client_id, claims.principal.authorization_version,
          JSON.stringify({ operations: claims.principal.allowed_operations, record_types: claims.principal.allowed_types, time_range: claims.principal.allowed_time_range }),
          JSON.stringify(request.permissions), claims.aud],
      );
    } else {
      [row] = await q<AuthorityRow>(
        `SELECT a.id::text AS account_id, a.user_id::text AS member_id, 'family-health-web'::text AS client_id, NULL::text AS parent_resource,
                '1'::text AS authorization_version,
                floor(extract(epoch FROM least(s.expires_at,s.created_at+interval '90 days')))::bigint::text AS parent_expires_at
           FROM auth.sessions s JOIN auth.accounts a ON a.id=s.account_id
          WHERE s.id=$1 AND s.expires_at>statement_timestamp() AND s.created_at>statement_timestamp()-interval '90 days'
            AND a.disabled_at IS NULL AND a.password_hash IS NOT NULL
          FOR SHARE OF s,a`, [request.sessionId],
      );
    }
    if (!row) return null;
    const [clock] = await q<{ issued_at: string }>("SELECT floor(extract(epoch FROM clock_timestamp()))::bigint::text AS issued_at");
    const issuedAt = Number(clock.issued_at), version = Number(row.authorization_version), parentExpiry = Number(row.parent_expires_at);
    if (!Number.isSafeInteger(issuedAt) || !Number.isSafeInteger(parentExpiry) || !Number.isSafeInteger(version) || version < 1) throw new Error("API authorization state is invalid");
    const expiresIn = Math.min(request.kind === "grant" ? 60 : 1800, parentExpiry - issuedAt, subjectExpiry - issuedAt);
    if (expiresIn <= 0) return null;
    const id = randomUUID(), token = newToken();
    const common = { member_id: row.member_id, credential_id: id, allowed_types: request.permissions.record_types,
      allowed_time_range: request.permissions.time_range, authorization_version: version };
    const principal: APIPrincipal = request.kind === "grant"
      ? { ...common, credential_kind: "exchange", allowed_operations: request.permissions.operations }
      : { ...common, credential_kind: "session", allowed_operations: request.permissions.operations };
    const assertion = signAccessToken({
      iss: signer.issuer, aud: "family-health-api", sub: row.member_id, client_id: row.client_id,
      iat: issuedAt, exp: issuedAt + expiresIn, jti: id,
      ...(grantId ? { grant_id: grantId } : {}), principal,
    }, signer.signingKey);
    await q(
      `INSERT INTO auth.api_credentials(id,token_hash,account_id,session_id,grant_id,permissions,authorization_version,assertion,created_at,expires_at,parent_resource)
       VALUES($1,$2,$3,$4,$5,$6,$7,$8,to_timestamp($9),to_timestamp($10),$11)`,
      [id, tokenHash(token), row.account_id, request.kind === "session" ? request.sessionId : null,
        grantId, JSON.stringify(request.permissions), version, assertion, issuedAt, issuedAt + expiresIn, row.parent_resource],
    );
    return { token, expiresIn };
  });
}
