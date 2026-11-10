import { oauthKeyFixture } from "./keyFixture";
// The OAuth route handlers' own decisions, with the database mocked out:
// metadata, registration, the token endpoint's client authentication,
// parameter checks, throttling and answers, revocation, and the consent
// form's re-validation. The database's side — single-use codes, refresh
// rotation and reuse detection — is in oauth.integration.test.ts.

import { verify } from "node:crypto";
import { NextRequest } from "next/server";
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

const oauthKeys = oauthKeyFixture();
const store = vi.hoisted(() => ({
  registerClient: vi.fn(),
  findClient: vi.fn(),
  exchangeCode: vi.fn(),
  refreshGrant: vi.fn(),
  revokeRefreshToken: vi.fn(),
  createCode: vi.fn(),
}));
const sessions = vi.hoisted(() => ({ findSession: vi.fn() }));
vi.mock("./store", async (importOriginal) => ({ ...(await importOriginal<typeof import("./store")>()), ...store }));
vi.mock("../accounts/session", async (importOriginal) => ({ ...(await importOriginal<typeof import("../accounts/session")>()), ...sessions }));

const ISSUER = "https://viewer.example.com";
const MCP = "https://mcp.example.com/mcp";
const CLIENT_ID = "pc_AAAAAAAAAAAAAAAAAAAAAAAA";
const USER = "5ea4d000-0000-4000-8000-000000000001";
const CHALLENGE = "p_j-Nj_Xa-C0MOoCl6lv9UzGgSvHUvKaouTlEnyXOiE";
const TOKEN = "R".repeat(43);
const PERMISSIONS = { operations: ["metrics:read"], record_types: ["HKQuantityTypeIdentifierStepCount"], time_range: { from: "2020-01-01T00:00:00Z", until: "2030-01-01T00:00:00Z" } } satisfies import("./permissions").ReadPermissions;
const PUBLIC_CLIENT = {
  id: CLIENT_ID,
  name: "Claude",
  redirectUris: ["https://claude.ai/api/mcp/auth_callback", "http://localhost/callback"],
  grantTypes: ["authorization_code", "refresh_token"],
  authMethod: "none",
  secretHash: null,
  createdAt: 0,
};

let ipCounter = 0;
const freshIp = () => `198.51.100.${++ipCounter}`;

function req(path: string, init: { method?: string; body?: string; type?: string; headers?: Record<string, string>; ip?: string } = {}) {
  const headers = new Headers({ host: "viewer.example.com", "x-forwarded-proto": "https", "x-forwarded-for": init.ip ?? freshIp(), ...init.headers });
  if (init.type) headers.set("content-type", init.type);
  return new NextRequest(new URL(path, "http://web:3000"), { method: init.method ?? "POST", headers, body: init.body });
}
const form = (fields: Record<string, string>) => new URLSearchParams(fields).toString();
const tokenReq = (fields: Record<string, string>, opts: { ip?: string; headers?: Record<string, string> } = {}) =>
  req("/oauth/token", { body: form(fields), type: "application/x-www-form-urlencoded", ...opts });

const saved = { ...process.env };
beforeAll(() => {
  process.env.WEB_ACCOUNTS = "true";
  process.env.TRUST_PROXY_HEADERS = "true";
  process.env.WEB_PUBLIC_URL = `${ISSUER}/`;
  process.env.PULS_MCP_URL = MCP;
  delete process.env.PULS_MCP_OAUTH_SECRET;
  Object.assign(process.env, oauthKeys.env);
  delete process.env.WEB_CLIENT_IP_HEADER;
});
afterAll(() => {
  process.env = saved;
});
beforeEach(() => {
  for (const fn of [...Object.values(store), ...Object.values(sessions)]) fn.mockReset();
  store.findClient.mockResolvedValue(PUBLIC_CLIENT);
});

describe("while OAuth is off", () => {
  it("answers 404 on every endpoint", async () => {
    delete process.env.PULS_AUTH_PRIVATE_KEY_FILE;
    delete process.env.PULS_AUTH_PUBLIC_KEYS_FILE;
    delete process.env.PULS_AUTH_KEY_ID;
    delete process.env.PULS_MCP_URL;
    vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const meta = await import("@/app/.well-known/oauth-authorization-server/route");
      const reg = await import("@/app/oauth/register/route");
      const token = await import("@/app/oauth/token/route");
      const revoke = await import("@/app/oauth/revoke/route");
      const decision = await import("@/app/oauth/authorize/decision/route");
      expect(meta.GET().status).toBe(404);
      expect(meta.OPTIONS().status).toBe(404);
      expect((await reg.POST(req("/oauth/register", { body: "{}" }))).status).toBe(404);
      expect((await token.POST(tokenReq({ grant_type: "refresh_token" }))).status).toBe(404);
      expect((await revoke.POST(req("/oauth/revoke", { body: "token=x" }))).status).toBe(404);
      expect((await decision.POST(req("/oauth/authorize/decision", { body: "" }))).status).toBe(404);
      expect(store.findClient).not.toHaveBeenCalled();
    } finally {
      delete process.env.PULS_MCP_OAUTH_SECRET;
      Object.assign(process.env, oauthKeys.env);
      process.env.PULS_MCP_URL = MCP;
      vi.restoreAllMocks();
    }
  });
});

describe("metadata", () => {
  it("describes this server for RFC 8414, with CORS and no caching", async () => {
    const { GET, OPTIONS } = await import("@/app/.well-known/oauth-authorization-server/route");
    const res = GET();
    expect(res.headers.get("access-control-allow-origin")).toBe("*");
    expect(res.headers.get("access-control-allow-credentials")).toBeNull();
    expect(res.headers.get("cache-control")).toBe("no-store");
    expect(await res.json()).toMatchObject({
      issuer: ISSUER,
      authorization_endpoint: `${ISSUER}/oauth/authorize`,
      token_endpoint: `${ISSUER}/oauth/token`,
      registration_endpoint: `${ISSUER}/oauth/register`,
      revocation_endpoint: `${ISSUER}/oauth/revoke`,
      response_types_supported: ["code"],
      grant_types_supported: ["authorization_code", "refresh_token"],
      code_challenge_methods_supported: ["S256"],
      token_endpoint_auth_methods_supported: ["none", "client_secret_post", "client_secret_basic"],
      scopes_supported: ["health:read"],
    });
    const pre = OPTIONS();
    expect(pre.status).toBe(204);
    expect(pre.headers.get("access-control-allow-headers")).toContain("Authorization");
  });
});

describe("registration", () => {
  it("registers a public client and answers 201 without a secret", async () => {
    store.registerClient.mockResolvedValue({ id: CLIENT_ID, secret: null, createdAt: 1_790_000_000_000 });
    const { POST } = await import("@/app/oauth/register/route");
    const res = await POST(
      req("/oauth/register", {
        type: "application/json",
        body: JSON.stringify({ client_name: "Claude", redirect_uris: ["https://claude.ai/api/mcp/auth_callback"] }),
      }),
    );
    expect(res.status).toBe(201);
    const body = await res.json();
    expect(body).toMatchObject({ client_id: CLIENT_ID, client_id_issued_at: 1_790_000_000, token_endpoint_auth_method: "none" });
    expect(body.client_secret).toBeUndefined();
    expect(res.headers.get("access-control-allow-origin")).toBe("*");
  });

  it("returns a confidential client's secret once", async () => {
    store.registerClient.mockResolvedValue({ id: CLIENT_ID, secret: TOKEN, createdAt: 0 });
    const { POST } = await import("@/app/oauth/register/route");
    const res = await POST(
      req("/oauth/register", { body: JSON.stringify({ redirect_uris: ["https://a.example/cb"], token_endpoint_auth_method: "client_secret_post" }) }),
    );
    expect(await res.json()).toMatchObject({ client_secret: TOKEN, client_secret_expires_at: 0 });
  });

  it("refuses bad metadata and malformed JSON without storing anything", async () => {
    const { POST } = await import("@/app/oauth/register/route");
    const bad = await POST(req("/oauth/register", { body: JSON.stringify({ redirect_uris: ["http://evil.example/cb"] }) }));
    expect(bad.status).toBe(400);
    expect((await bad.json()).error).toBe("invalid_redirect_uri");
    expect((await POST(req("/oauth/register", { body: "{" }))).status).toBe(400);
    expect((await POST(req("/oauth/register", { body: JSON.stringify({ redirect_uris: ["https://a.example/cb"], x: "y".repeat(20_000) }) }))).status).toBe(400);
    expect(store.registerClient).not.toHaveBeenCalled();
  });

  it("allows thirty registrations an hour per address, an IPv6 address by its /64, and has no global cap", async () => {
    store.registerClient.mockResolvedValue({ id: CLIENT_ID, secret: null, createdAt: 0 });
    const { POST } = await import("@/app/oauth/register/route");
    const ip = freshIp();
    const body = JSON.stringify({ redirect_uris: ["https://a.example/cb"] });
    for (let i = 0; i < 30; i++) expect((await POST(req("/oauth/register", { body, ip }))).status).toBe(201);
    const refused = await POST(req("/oauth/register", { body, ip }));
    expect(refused.status).toBe(429);
    expect(Number(refused.headers.get("retry-after"))).toBeGreaterThan(0);
    expect((await POST(req("/oauth/register", { body }))).status).toBe(201);

    // One /64 is one bucket, whatever the low 64 bits.
    for (let i = 0; i < 30; i++) {
      expect((await POST(req("/oauth/register", { body, ip: `2001:db8:1:2::${(i + 1).toString(16)}` }))).status).toBe(201);
    }
    expect((await POST(req("/oauth/register", { body, ip: "2001:db8:1:2:ffff:ffff:ffff:ffff" }))).status).toBe(429);
    expect((await POST(req("/oauth/register", { body, ip: "2001:db8:1:3::1" }))).status).toBe(201);

    // Many addresses together are not throttled as a whole: draining the old
    // 200-an-hour global cap from a handful of addresses blocked everyone.
    for (let n = 0; n < 10; n++) {
      const each = `203.0.113.${n + 1}`;
      for (let i = 0; i < 25; i++) expect((await POST(req("/oauth/register", { body, ip: each }))).status).toBe(201);
    }
    expect((await POST(req("/oauth/register", { body }))).status).toBe(201);
  });

  it("refuses a redirect URI on the viewer's own origin", async () => {
    const { POST } = await import("@/app/oauth/register/route");
    for (const uri of [`${ISSUER}/account`, `${ISSUER}/oauth/authorize`, "https://VIEWER.example.com:443/x"]) {
      const res = await POST(req("/oauth/register", { body: JSON.stringify({ redirect_uris: ["https://a.example/cb", uri] }) }));
      expect(res.status, uri).toBe(400);
      expect(await res.json()).toMatchObject({ error: "invalid_redirect_uri" });
    }
    expect(store.registerClient).not.toHaveBeenCalled();
  });

  it("refuses an oversized body before reading it, and stops reading a chunked one at the limit", async () => {
    const { POST } = await import("@/app/oauth/register/route");
    const declared = await POST(req("/oauth/register", { body: "{}", headers: { "content-length": String(16 * 1024 + 1) } }));
    expect(declared.status).toBe(400);
    expect((await declared.json()).error_description).toBe("the registration is too large");

    let pulled = 0;
    const endless = new ReadableStream<Uint8Array>({
      pull(controller) {
        pulled++;
        controller.enqueue(new Uint8Array(4096).fill(0x20));
      },
    });
    const chunked = new NextRequest(new URL("/oauth/register", "http://web:3000"), {
      method: "POST",
      headers: { host: "viewer.example.com", "x-forwarded-proto": "https", "x-forwarded-for": freshIp() },
      body: endless,
      duplex: "half",
    } as ConstructorParameters<typeof NextRequest>[1]);
    const res = await POST(chunked);
    expect(res.status).toBe(400);
    expect(pulled).toBeLessThan(10);
    expect(store.registerClient).not.toHaveBeenCalled();
  });
});

describe("token endpoint", () => {
  const exchange = { grant_type: "authorization_code", code: TOKEN, redirect_uri: "https://claude.ai/api/mcp/auth_callback", code_verifier: "v".repeat(43), client_id: CLIENT_ID };

  it("exchanges a code for a signed access token and a refresh token", async () => {
    store.exchangeCode.mockResolvedValue({ ok: true, grant: { grantId: "5ea4d000-0000-4000-8000-000000000003", permissions: PERMISSIONS, authorizationVersion: 1, userId: USER, scope: "health:read", refreshToken: TOKEN } });
    const { POST } = await import("@/app/oauth/token/route");
    const res = await POST(tokenReq({ ...exchange, resource: MCP }));
    expect(res.status).toBe(200);
    expect(res.headers.get("cache-control")).toBe("no-store");
    const body = await res.json();
    expect(body).toMatchObject({ token_type: "Bearer", expires_in: 1800, refresh_token: TOKEN, scope: "health:read" });
    const [h, p, s] = body.access_token.split(".");
    expect(verify(null, Buffer.from(`${h}.${p}`), oauthKeys.publicKey, Buffer.from(s, "base64url"))).toBe(true);
    expect(JSON.parse(Buffer.from(p, "base64url").toString())).toMatchObject({ iss: ISSUER, aud: MCP, sub: USER, client_id: CLIENT_ID });
    expect(store.exchangeCode).toHaveBeenCalledWith({
      code: TOKEN,
      clientId: CLIENT_ID,
      redirectUri: exchange.redirect_uri,
      codeVerifier: exchange.code_verifier,
      resource: MCP,
      configuredResource: [MCP, `${ISSUER}/api/health`],
    });
  });

  it("answers invalid_grant for a spent, expired or mismatched code", async () => {
    store.exchangeCode.mockResolvedValue({ ok: false, error: "invalid_grant", reused: true });
    const { POST } = await import("@/app/oauth/token/route");
    const res = await POST(tokenReq(exchange));
    expect(res.status).toBe(400);
    expect(await res.json()).toEqual({ error: "invalid_grant" });
  });

  it("requires code, redirect_uri and code_verifier, once each", async () => {
    const { POST } = await import("@/app/oauth/token/route");
    for (const missing of ["code", "redirect_uri", "code_verifier"]) {
      const fields: Record<string, string> = { ...exchange };
      delete fields[missing];
      expect((await (await POST(tokenReq(fields))).json()).error, missing).toBe("invalid_request");
    }
    const twice = `${form(exchange)}&code=${TOKEN}`;
    const res = await POST(req("/oauth/token", { body: twice, type: "application/x-www-form-urlencoded" }));
    expect((await res.json()).error).toBe("invalid_request");
    expect(store.exchangeCode).not.toHaveBeenCalled();
  });

  it("refuses JSON bodies, unknown grant types and unknown clients", async () => {
    const { POST } = await import("@/app/oauth/token/route");
    const json = await POST(req("/oauth/token", { body: JSON.stringify(exchange), type: "application/json" }));
    expect((await json.json()).error).toBe("invalid_request");
    expect((await (await POST(tokenReq({ ...exchange, grant_type: "password" }))).json()).error).toBe("unsupported_grant_type");
    store.findClient.mockResolvedValue(null);
    const unknown = await POST(tokenReq(exchange));
    expect(unknown.status).toBe(401);
    expect((await unknown.json()).error).toBe("invalid_client");
    expect((await POST(tokenReq({ grant_type: "refresh_token", refresh_token: TOKEN }))).status).toBe(401);
  });

  it("requires a confidential client's secret, by post or by Basic", async () => {
    const { tokenHash } = await import("../accounts/session");
    const secret = "S".repeat(43);
    store.findClient.mockResolvedValue({ ...PUBLIC_CLIENT, authMethod: "client_secret_basic", secretHash: tokenHash(secret) });
    store.refreshGrant.mockResolvedValue({ ok: true, grant: { grantId: "5ea4d000-0000-4000-8000-000000000003", permissions: PERMISSIONS, authorizationVersion: 1, userId: USER, scope: "health:read", refreshToken: TOKEN } });
    const { POST } = await import("@/app/oauth/token/route");
    const refresh = { grant_type: "refresh_token", refresh_token: TOKEN };

    const none = await POST(tokenReq({ ...refresh, client_id: CLIENT_ID }));
    expect(none.status).toBe(401);
    const wrong = await POST(tokenReq(refresh, { headers: { authorization: `Basic ${Buffer.from(`${CLIENT_ID}:${"X".repeat(43)}`).toString("base64")}` } }));
    expect(wrong.status).toBe(401);
    expect(wrong.headers.get("www-authenticate")).toContain("Basic");
    const basic = await POST(tokenReq(refresh, { headers: { authorization: `Basic ${Buffer.from(`${CLIENT_ID}:${secret}`).toString("base64")}` } }));
    expect(basic.status).toBe(200);
    const post = await POST(tokenReq({ ...refresh, client_id: CLIENT_ID, client_secret: secret }));
    expect(post.status).toBe(200);
    // Basic and a different form client_id disagree.
    const both = await POST(
      tokenReq({ ...refresh, client_id: "pc_BBBBBBBBBBBBBBBBBBBBBBBB" }, { headers: { authorization: `Basic ${Buffer.from(`${CLIENT_ID}:${secret}`).toString("base64")}` } }),
    );
    expect((await both.json()).error).toBe("invalid_request");
  });

  it("rotates on refresh and refuses a scope the grant does not hold", async () => {
    store.refreshGrant.mockResolvedValueOnce({ ok: true, grant: { grantId: "5ea4d000-0000-4000-8000-000000000003", permissions: PERMISSIONS, authorizationVersion: 1, userId: USER, scope: "health:read", refreshToken: "N".repeat(43) } });
    store.refreshGrant.mockResolvedValueOnce({ ok: false, error: "invalid_scope" });
    const { POST } = await import("@/app/oauth/token/route");
    const ok = await POST(tokenReq({ grant_type: "refresh_token", refresh_token: TOKEN, client_id: CLIENT_ID }));
    expect((await ok.json()).refresh_token).toBe("N".repeat(43));
    expect(store.refreshGrant).toHaveBeenLastCalledWith({ refreshToken: TOKEN, clientId: CLIENT_ID, scope: null, resource: null, configuredResource: MCP });
    const scoped = await POST(tokenReq({ grant_type: "refresh_token", refresh_token: TOKEN, client_id: CLIENT_ID, scope: "health:write" }));
    expect((await scoped.json()).error).toBe("invalid_scope");
  });

  it("refuses a body over 16 KiB, declared or not", async () => {
    const { POST } = await import("@/app/oauth/token/route");
    const declared = await POST(req("/oauth/token", { body: "grant_type=x", type: "application/x-www-form-urlencoded", headers: { "content-length": "999999" } }));
    expect(await declared.json()).toMatchObject({ error: "invalid_request", error_description: "the request is too large" });
    const big = await POST(tokenReq({ ...exchange, pad: "x".repeat(17 * 1024) }));
    expect(await big.json()).toMatchObject({ error: "invalid_request", error_description: "the request is too large" });
    expect(store.findClient).not.toHaveBeenCalled();
  });

  it("charges failures to the address, refunds successes and server errors", async () => {
    const { POST } = await import("@/app/oauth/token/route");
    const ip = freshIp();
    store.refreshGrant.mockResolvedValue({ ok: true, grant: { grantId: "5ea4d000-0000-4000-8000-000000000003", permissions: PERMISSIONS, authorizationVersion: 1, userId: USER, scope: "health:read", refreshToken: TOKEN } });
    for (let i = 0; i < 100; i++) {
      expect((await POST(tokenReq({ grant_type: "refresh_token", refresh_token: TOKEN, client_id: CLIENT_ID }, { ip }))).status).toBe(200);
    }
    store.refreshGrant.mockRejectedValue(new Error("db down"));
    vi.spyOn(console, "error").mockImplementation(() => {});
    for (let i = 0; i < 100; i++) {
      expect((await POST(tokenReq({ grant_type: "refresh_token", refresh_token: TOKEN, client_id: CLIENT_ID }, { ip }))).status).toBe(500);
    }
    vi.restoreAllMocks();
    store.refreshGrant.mockResolvedValue({ ok: false, error: "invalid_grant" });
    for (let i = 0; i < 60; i++) {
      expect((await POST(tokenReq({ grant_type: "refresh_token", refresh_token: TOKEN, client_id: CLIENT_ID }, { ip }))).status).toBe(400);
    }
    const throttled = await POST(tokenReq({ grant_type: "refresh_token", refresh_token: TOKEN, client_id: CLIENT_ID }, { ip }));
    expect(throttled.status).toBe(429);
    const calls = store.refreshGrant.mock.calls.length;
    await POST(tokenReq({ grant_type: "refresh_token", refresh_token: TOKEN, client_id: CLIENT_ID }, { ip }));
    expect(store.refreshGrant.mock.calls.length).toBe(calls); // refused before anything was checked
  });
});

describe("revocation", () => {
  it("answers 200 whatever the token, and revokes through the store", async () => {
    store.revokeRefreshToken.mockResolvedValue(false);
    const { POST } = await import("@/app/oauth/revoke/route");
    const res = await POST(req("/oauth/revoke", { body: form({ token: TOKEN, client_id: CLIENT_ID }), type: "application/x-www-form-urlencoded" }));
    expect(res.status).toBe(200);
    expect(store.revokeRefreshToken).toHaveBeenCalledWith(TOKEN, CLIENT_ID);
    expect((await POST(req("/oauth/revoke", { body: "" }))).status).toBe(200);
  });

  it("throttles revocations that revoke nothing, per address, and refunds the ones that do", async () => {
    const { POST } = await import("@/app/oauth/revoke/route");
    const ip = freshIp();
    const revoke = () => POST(req("/oauth/revoke", { body: form({ token: TOKEN }), type: "application/x-www-form-urlencoded", ip }));
    store.revokeRefreshToken.mockResolvedValue(true);
    for (let i = 0; i < 100; i++) expect((await revoke()).status).toBe(200);
    store.revokeRefreshToken.mockResolvedValue(false);
    for (let i = 0; i < 60; i++) expect((await revoke()).status).toBe(200);
    const throttled = await revoke();
    expect(throttled.status).toBe(429);
    expect(Number(throttled.headers.get("retry-after"))).toBeGreaterThan(0);
    const calls = store.revokeRefreshToken.mock.calls.length;
    await revoke();
    expect(store.revokeRefreshToken.mock.calls.length).toBe(calls);
    expect((await POST(req("/oauth/revoke", { body: form({ token: TOKEN }), type: "application/x-www-form-urlencoded" }))).status).toBe(200);
  });

  it("refuses a body over 16 KiB without looking the token up", async () => {
    const { POST } = await import("@/app/oauth/revoke/route");
    const res = await POST(req("/oauth/revoke", { body: form({ token: TOKEN, pad: "x".repeat(17 * 1024) }), type: "application/x-www-form-urlencoded" }));
    expect(res.status).toBe(400);
    expect(store.revokeRefreshToken).not.toHaveBeenCalled();
  });
});

describe("the consent decision", () => {
  const SESSION = { id: Buffer.alloc(32, 7), accountId: "acc-1", userId: USER, email: "a@example.com", isAdmin: false, selfService: true, refreshed: false };
  const consent = {
    operation: "metrics:read", record_type: "HKQuantityTypeIdentifierStepCount", from: "2020-01-01T00:00:00", until: "2030-01-01T00:00:00",
    client_id: CLIENT_ID,
    redirect_uri: "https://claude.ai/api/mcp/auth_callback",
    response_type: "code",
    code_challenge: CHALLENGE,
    code_challenge_method: "S256",
    scope: "health:read",
    resource: MCP,
    state: "st",
  };
  const decide = (fields: Record<string, string>) => {
    const body = new FormData();
    for (const [k, v] of Object.entries(fields)) body.set(k, v);
    return new NextRequest(new URL("/oauth/authorize/decision", "http://web:3000"), {
      method: "POST",
      headers: { host: "viewer.example.com", "x-forwarded-proto": "https", cookie: `__Host-puls-session=${TOKEN}` },
      body,
    });
  };

  it("issues a code on Allow and sends it to the redirect URI with state and iss", async () => {
    sessions.findSession.mockResolvedValue(SESSION);
    store.createCode.mockResolvedValue("C".repeat(43));
    const { POST } = await import("@/app/oauth/authorize/decision/route");
    const res = await POST(decide({ ...consent, decision: "allow" }));
    expect(res.status).toBe(303);
    const loc = new URL(res.headers.get("location")!);
    expect(`${loc.origin}${loc.pathname}`).toBe(consent.redirect_uri);
    expect(Object.fromEntries(loc.searchParams)).toEqual({ code: "C".repeat(43), state: "st", iss: ISSUER });
    expect(store.createCode).toHaveBeenCalledWith("acc-1", expect.objectContaining({ clientId: CLIENT_ID, codeChallenge: CHALLENGE, resource: MCP }), PERMISSIONS);
  });

  it("answers access_denied on Deny, and issues nothing", async () => {
    sessions.findSession.mockResolvedValue(SESSION);
    const { POST } = await import("@/app/oauth/authorize/decision/route");
    const res = await POST(decide({ ...consent, decision: "deny" }));
    expect(Object.fromEntries(new URL(res.headers.get("location")!).searchParams)).toEqual({ error: "access_denied", state: "st", iss: ISSUER });
    expect(store.createCode).not.toHaveBeenCalled();
  });

  it("requires personal type and time choices before issuing a code", async () => {
    sessions.findSession.mockResolvedValue(SESSION);
    const { POST } = await import("@/app/oauth/authorize/decision/route");
    for (const bad of [
      { ...consent, record_type: "", decision: "allow" },
      { ...consent, operation: "data:delete", decision: "allow" },
      { ...consent, from: "2026-02-30T00:00:00", decision: "allow" },
      { ...consent, until: "2019-01-01T00:00:00", decision: "allow" },
    ]) expect((await POST(decide(bad))).status).toBe(400);
    expect(store.createCode).not.toHaveBeenCalled();
  });

  it("checks the hidden fields again: an edited redirect URI is refused outright", async () => {
    sessions.findSession.mockResolvedValue(SESSION);
    const { POST } = await import("@/app/oauth/authorize/decision/route");
    const res = await POST(decide({ ...consent, redirect_uri: "https://evil.example/cb", decision: "allow" }));
    expect(res.status).toBe(400);
    expect(res.headers.get("location")).toBeNull();
    expect(store.createCode).not.toHaveBeenCalled();
  });

  it("never redirects to the client on an invalid request, Deny included: back to the consent page instead", async () => {
    sessions.findSession.mockResolvedValue(SESSION);
    const { POST } = await import("@/app/oauth/authorize/decision/route");
    for (const [bad, decision] of [
      [{ code_challenge_method: "plain" }, "allow"],
      [{ response_type: "bogus" }, "allow"],
      [{ response_type: "bogus" }, "deny"],
      [{ scope: "health:write" }, "deny"],
    ] as const) {
      const res = await POST(decide({ ...consent, ...bad, decision }));
      expect(res.status).toBe(303);
      const location = res.headers.get("location")!;
      expect(location.startsWith("/oauth/authorize?")).toBe(true);
      const back = new URLSearchParams(location.slice("/oauth/authorize?".length));
      const expected = new URLSearchParams({ ...consent, ...bad });
      for (const key of ["operation", "record_type", "from", "until"]) {
        expected.delete(key);
        expect(back.has(key)).toBe(false);
      }
      expect(Object.fromEntries(back)).toEqual(Object.fromEntries(expected));
    }
    expect(store.createCode).not.toHaveBeenCalled();
  });

  it("needs a session", async () => {
    sessions.findSession.mockResolvedValue(null);
    const { POST } = await import("@/app/oauth/authorize/decision/route");
    expect((await POST(decide({ ...consent, decision: "allow" }))).status).toBe(401);
    expect(store.createCode).not.toHaveBeenCalled();
  });
});
