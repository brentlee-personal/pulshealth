import { oauthResources } from "@/lib/oauth/config";
import type { NextRequest } from "next/server";

import { requestIp } from "@/lib/accounts/http";
import { refundAll, takeAll } from "@/lib/accounts/ratelimit";
import { issueAccessToken } from "@/lib/oauth/jwt";
import { clientCredentials, formParam, json, oauthError, oauthOrNotFound, parseForm, preflight, readBodyCapped, tokenFailures } from "@/lib/oauth/http";
import { clientSecretMatches, exchangeCode, findClient, type GrantResult, refreshGrant } from "@/lib/oauth/store";

// The token endpoint (RFC 6749 §3.2), form-encoded:
//
//   authorization_code  code, redirect_uri, code_verifier (PKCE S256),
//                       client_id (+ secret for a confidential client).
//                       Single use; a second use by the same client
//                       revokes the grant.
//   refresh_token       Rotates on every use; an already-rotated token
//                       revokes the grant.
//
// Answers an access token (30 minutes, verified by the MCP server alone), a
// new refresh token (60 days from last use) and the scope. Every failure is
// charged to the client address (lib/oauth/http.ts, tokenFailures): the
// token is taken before anything is checked and refunded on success or on
// the server's own error. Nothing about a request is logged.
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  const { config, off } = oauthOrNotFound();
  if (off) return off;

  const keys = [`ip:${requestIp(request)}`];
  const gate = takeAll(tokenFailures, keys);
  if (!gate.allowed) {
    return oauthError(429, "temporarily_unavailable", "too many failed requests; try again later", {
      "Retry-After": String(gate.retryAfterSeconds),
    });
  }
  // From here, returning without a refund records a failure.

  const type = request.headers.get("content-type") ?? "";
  if (!/^application\/x-www-form-urlencoded\b/i.test(type)) {
    return oauthError(400, "invalid_request", "the body must be application/x-www-form-urlencoded");
  }
  let form: FormData;
  try {
    const text = await readBodyCapped(request);
    if (text === null) return oauthError(400, "invalid_request", "the request is too large");
    form = parseForm(text);
  } catch {
    return oauthError(400, "invalid_request");
  }

  const creds = clientCredentials(request.headers.get("authorization"), form);
  const challenge = (basic: boolean) => (basic ? { "WWW-Authenticate": 'Basic realm="PulsHealth"' } : undefined);
  if (!creds.ok) {
    return creds.error === "invalid_client"
      ? oauthError(401, "invalid_client", undefined, challenge(creds.basic))
      : oauthError(400, "invalid_request", "client credentials are malformed or repeated");
  }

  const grantType = formParam(form, "grant_type");
  try {
    const client = await findClient(creds.clientId);
    if (!client || !clientSecretMatches(client, creds.secret)) {
      return oauthError(401, "invalid_client", undefined, challenge(creds.basic));
    }
    if (grantType !== "authorization_code" && grantType !== "refresh_token") {
      return oauthError(400, "unsupported_grant_type");
    }
    if (!client.grantTypes.includes(grantType)) return oauthError(400, "unauthorized_client");

    let result: GrantResult;
    if (grantType === "authorization_code") {
      const code = formParam(form, "code");
      const redirectUri = formParam(form, "redirect_uri");
      const verifier = formParam(form, "code_verifier");
      const resource = formParam(form, "resource");
      if (!code || !redirectUri || !verifier || resource === undefined) {
        return oauthError(400, "invalid_request", "code, redirect_uri and code_verifier are required, once each");
      }
      result = await exchangeCode({
        code,
        clientId: client.id,
        redirectUri,
        codeVerifier: verifier,
        resource,
        configuredResource: oauthResources(config),
      });
    } else {
      const refreshToken = formParam(form, "refresh_token");
      const scope = formParam(form, "scope");
      const resource = formParam(form, "resource");
      if (resource === undefined) return oauthError(400, "invalid_request");
      if (!refreshToken || scope === undefined) return oauthError(400, "invalid_request", "refresh_token is required, once");
      result = await refreshGrant({ refreshToken, clientId: client.id, scope, resource, configuredResource: config.resource });
    }
    if (!result.ok) return oauthError(400, result.error);

    refundAll(tokenFailures, keys);
    const audience = result.grant.resource ?? config.resource;
    if (!oauthResources(config).includes(audience)) return oauthError(400, "invalid_target");
    const access = issueAccessToken({ ...config, resource: audience }, result.grant.userId, client.id, undefined, {
      grantId: result.grant.grantId, permissions: result.grant.permissions, authorizationVersion: result.grant.authorizationVersion,
    });
    return json(200, {
      access_token: access.token,
      token_type: "Bearer",
      expires_in: access.expiresIn,
      refresh_token: result.grant.refreshToken,
      scope: result.grant.scope,
    });
  } catch (e) {
    refundAll(tokenFailures, keys);
    console.error("[puls-web] oauth token request failed:", e instanceof Error ? e.message : e);
    return oauthError(500, "server_error");
  }
}

export function OPTIONS() {
  const { off } = oauthOrNotFound();
  return off ?? preflight();
}
