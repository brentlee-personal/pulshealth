import { oauthKeyFixture } from "./lib/oauth/keyFixture";
import { NextRequest } from "next/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// The session store is the database; here it is whatever each test says.
const oauthKeys = oauthKeyFixture();
const findSession = vi.hoisted(() => vi.fn());
vi.mock("@/lib/accounts/session", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/accounts/session")>();
  return { ...actual, findSession };
});

const TOKEN = "A".repeat(43);
const SESSION = {
  id: Buffer.alloc(32),
  accountId: "acc",
  userId: "11111111-1111-4111-8111-111111111111",
  email: "a@example.com",
  isAdmin: false,
  refreshed: false,
};

function request(path: string, init: { method?: string; headers?: Record<string, string>; cookie?: string; base?: string } = {}) {
  const headers = new Headers(init.headers);
  if (init.cookie) headers.set("cookie", init.cookie);
  return new NextRequest(new URL(path, init.base ?? "http://web:3000"), { method: init.method ?? "GET", headers });
}

// What a request through a TLS proxy (TRUST_PROXY_HEADERS=true) looks like.
const viaProxy = { host: "viewer.example", "x-forwarded-proto": "https", "x-forwarded-for": "203.0.113.5" };
const passed = (res: Response) => res.headers.get("x-middleware-next") === "1";

const saved = { ...process.env };
beforeEach(() => {
  findSession.mockReset();
  vi.stubEnv("NODE_ENV", "production");
  delete process.env.WEB_AUTH_PASSWORD;
  delete process.env.WEB_ACCOUNTS;
  delete process.env.TRUST_PROXY_HEADERS;
  delete process.env.WEB_PUBLIC_URL;
});
afterEach(() => {
  vi.unstubAllEnvs();
  process.env = { ...saved };
});

async function proxy(req: NextRequest) {
  const { default: run } = await import("./proxy");
  return run(req);
}

describe("personal route headers", () => {
  beforeEach(() => {
    process.env.TRUST_PROXY_HEADERS = "true";
    findSession.mockResolvedValue(SESSION);
  });
  it("sends a nonce'd Content-Security-Policy and hands the nonce to the page", async () => {
    const res = await proxy(request("/", { headers: viaProxy, cookie: `__Host-puls-session=${TOKEN}` }));
    const csp = res.headers.get("content-security-policy")!;
    const nonce = /'nonce-([^']+)'/.exec(csp)?.[1];
    expect(nonce).toBeTruthy();
    expect(res.headers.get("x-middleware-request-x-nonce")).toBe(nonce);
  });

  it("hands the page its path, overwriting any the client sent", async () => {
    const req = request("/workouts", { headers: viaProxy, cookie: `__Host-puls-session=${TOKEN}` });
    req.headers.set("x-puls-pathname", "/oauth/authorize");
    const res = await proxy(req);
    expect(res.headers.get("x-middleware-request-x-puls-pathname")).toBe("/workouts");
  });
});

describe("personal accounts by default", () => {
  it("requires sign-in without an opt-in flag and ignores a target user", async () => {
    process.env.TRUST_PROXY_HEADERS = "true";
    findSession.mockResolvedValue(null);
    const response = await proxy(request("/workouts?user=22222222-2222-4222-8222-222222222222", { headers: viaProxy }));
    expect(response.status).toBe(303);
    expect(new URL(response.headers.get("location")!).pathname).toBe("/login");
    expect(response.cookies.get("puls-user")).toBeUndefined();
  });

  it("rejects the retired Basic setting without echoing its value", async () => {
    process.env.WEB_AUTH_PASSWORD = "synthetic-secret";
    await expect(proxy(request("/"))).rejects.toThrow(/WEB_AUTH_PASSWORD/);
    await expect(proxy(request("/"))).rejects.not.toThrow(/synthetic-secret/);
    expect(findSession).not.toHaveBeenCalled();
  });
});

describe("accounts mode", () => {
  beforeEach(() => {
    process.env.WEB_ACCOUNTS = "true";
    process.env.TRUST_PROXY_HEADERS = "true";
  });

  it("answers the health check over plain HTTP, and nothing else", async () => {
    delete process.env.TRUST_PROXY_HEADERS;
    expect(passed(await proxy(request("/api/healthz")))).toBe(true);
    expect(passed(await proxy(request("/_next/static/chunks/main.js")))).toBe(true);
    expect((await proxy(request("/login"))).status).toBe(403);
    expect((await proxy(request("/"))).status).toBe(403);
    expect(findSession).not.toHaveBeenCalled();
  });

  it("redirects plain HTTP from the trusted proxy to https on the public origin", async () => {
    const res = await proxy(request("/workouts?range=W", { headers: { host: "viewer.example", "x-forwarded-proto": "http" } }));
    expect(res.status).toBe(308);
    expect(res.headers.get("location")).toBe("https://viewer.example/workouts?range=W");
    process.env.WEB_PUBLIC_URL = "https://app.example";
    const pinned = await proxy(request("/login", { headers: { host: "evil.example", "x-forwarded-proto": "http" } }));
    expect(pinned.headers.get("location")).toBe("https://app.example/login");
  });

  it("does not believe X-Forwarded-Proto unless told to", async () => {
    delete process.env.TRUST_PROXY_HEADERS;
    expect((await proxy(request("/login", { headers: viaProxy }))).status).toBe(403);
  });

  it("serves the sign-in and invite pages without a session", async () => {
    expect(passed(await proxy(request("/login", { headers: viaProxy })))).toBe(true);
    expect(passed(await proxy(request(`/invite/${TOKEN}`, { headers: viaProxy })))).toBe(true);
    expect(findSession).not.toHaveBeenCalled();
  });

  it("sends a page load without a session to sign in, remembering where it was going", async () => {
    findSession.mockResolvedValue(null);
    const res = await proxy(request("/workouts?range=W&_rsc=x1", { headers: viaProxy }));
    expect(res.status).toBe(303);
    // Absolute (Next.js refuses a relative Location from the proxy), on the
    // origin the browser used rather than the container's own host.
    expect(res.headers.get("location")).toBe("https://viewer.example/login?next=%2Fworkouts%3Frange%3DW");
    expect((await proxy(request("/", { headers: viaProxy }))).headers.get("location")).toBe("https://viewer.example/login");
    process.env.WEB_PUBLIC_URL = "https://app.example/";
    expect((await proxy(request("/", { headers: viaProxy }))).headers.get("location")).toBe("https://app.example/login");
  });

  it("answers 401, not a redirect, to API calls and posts without a session", async () => {
    findSession.mockResolvedValue(null);
    expect((await proxy(request("/api/user", { headers: viaProxy }))).status).toBe(401);
    const post = request("/api/auth/password", { method: "POST", headers: { ...viaProxy, origin: "https://viewer.example" } });
    expect((await proxy(post)).status).toBe(401);
  });

  it("drops a cookie that names no live session", async () => {
    findSession.mockResolvedValue(null);
    const res = await proxy(request("/", { headers: viaProxy, cookie: `__Host-puls-session=${TOKEN}` }));
    expect(res.status).toBe(303);
    const cleared = res.cookies.get("__Host-puls-session");
    expect(cleared?.value).toBe("");
    expect(cleared?.maxAge).toBe(0);
  });

  it("serves a live session, checking it against the store and sliding it", async () => {
    findSession.mockResolvedValue({ ...SESSION, refreshed: true });
    const res = await proxy(request("/workouts", { headers: viaProxy, cookie: `__Host-puls-session=${TOKEN}` }));
    expect(passed(res)).toBe(true);
    expect(findSession).toHaveBeenCalledWith(TOKEN, true);
    const cookie = res.cookies.get("__Host-puls-session");
    expect(cookie).toMatchObject({ value: TOKEN, secure: true, httpOnly: true, sameSite: "lax", path: "/" });
  });

  it("ignores ?user= entirely", async () => {
    findSession.mockResolvedValue(SESSION);
    const res = await proxy(
      request("/workouts?user=22222222-2222-4222-8222-222222222222", { headers: viaProxy, cookie: `__Host-puls-session=${TOKEN}` }),
    );
    expect(passed(res)).toBe(true);
    expect(res.cookies.get("puls-user")).toBeUndefined();
  });

  it("refuses a state-changing request from another origin, or with none", async () => {
    findSession.mockResolvedValue(SESSION);
    for (const headers of [
      { ...viaProxy, origin: "https://evil.example" },
      { ...viaProxy },
      { ...viaProxy, origin: "https://viewer.example", "sec-fetch-site": "cross-site" },
    ]) {
      const res = await proxy(request("/api/auth/login", { method: "POST", headers }));
      expect(res.status, JSON.stringify(headers)).toBe(403);
    }
    const ok = await proxy(request("/api/auth/login", { method: "POST", headers: { ...viaProxy, origin: "https://viewer.example" } }));
    expect(passed(ok)).toBe(true);
  });

  it("is a 503, never a sign-in redirect, when the session store is down", async () => {
    findSession.mockRejectedValue(new Error("connection refused"));
    vi.spyOn(console, "error").mockImplementation(() => {});
    const res = await proxy(request("/", { headers: viaProxy, cookie: `__Host-puls-session=${TOKEN}` }));
    expect(res.status).toBe(503);
  });

});

describe("OAuth for AI assistants", () => {
  const OAUTH_ENV = {
    WEB_ACCOUNTS: "true",
    TRUST_PROXY_HEADERS: "true",
    WEB_PUBLIC_URL: "https://viewer.example",
    PULS_MCP_URL: "https://mcp.example/mcp",
    ...oauthKeys.env,
  };
  const OAUTH_PATHS = [
    "/.well-known/oauth-authorization-server",
    "/oauth/register",
    "/oauth/authorize",
    "/oauth/authorize/decision",
    "/oauth/token",
    "/oauth/revoke",
  ];
  const AUTHORIZE =
    "/oauth/authorize?client_id=pc_AAAAAAAAAAAAAAAAAAAAAAAA&redirect_uri=" +
    encodeURIComponent("https://claude.ai/api/mcp/auth_callback") +
    "&response_type=code&code_challenge=" +
    "p".repeat(43) +
    "&code_challenge_method=S256&state=s";

  it("answers 404 on every OAuth path while OAuth is off", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    for (const env of [{}, { WEB_ACCOUNTS: "true", TRUST_PROXY_HEADERS: "true" }]) {
      Object.assign(process.env, env);
      for (const path of OAUTH_PATHS) {
        for (const method of ["GET", "POST"]) {
          const res = await proxy(request(path, { method, headers: { ...viaProxy, origin: "https://viewer.example" } }));
          expect(res.status, `${JSON.stringify(env)} ${method} ${path}`).toBe(404);
        }
      }
      for (const k of Object.keys(env)) delete process.env[k];
    }
    expect(findSession).not.toHaveBeenCalled();
  });

  it("returns 503 for unavailable signing configuration instead of another authentication mode", async () => {
    Object.assign(process.env, OAUTH_ENV, { PULS_AUTH_PUBLIC_KEYS_FILE: "synthetic-missing-key-file" });
    const response = await proxy(request("/oauth/token", { method: "POST", headers: viaProxy }));
    expect(response.status).toBe(503);
    expect(await response.text()).not.toContain("synthetic-missing-key-file");
    expect(findSession).not.toHaveBeenCalled();
  });

  it("lets machine endpoints through over HTTPS with no session and any Origin", async () => {
    Object.assign(process.env, OAUTH_ENV);
    for (const path of ["/.well-known/oauth-authorization-server", "/oauth/register", "/oauth/token", "/oauth/revoke"]) {
      for (const headers of [{ ...viaProxy, origin: "https://claude.ai" }, { ...viaProxy }]) {
        expect(passed(await proxy(request(path, { method: "POST", headers }))), path).toBe(true);
      }
      expect(passed(await proxy(request(path, { method: "OPTIONS", headers: { ...viaProxy, origin: "https://claude.ai" } })))).toBe(true);
    }
    expect(findSession).not.toHaveBeenCalled();
    // Still HTTPS only.
    expect((await proxy(request("/oauth/token", { method: "POST" }))).status).toBe(403);
  });

  it("sends the consent page to sign in with the whole query, and keeps the form same-origin", async () => {
    Object.assign(process.env, OAUTH_ENV);
    findSession.mockResolvedValue(null);
    const res = await proxy(request(AUTHORIZE, { headers: viaProxy }));
    expect(res.status).toBe(303);
    const loc = new URL(res.headers.get("location")!);
    expect(loc.pathname).toBe("/login");
    expect(loc.searchParams.get("next")).toBe(AUTHORIZE);

    findSession.mockResolvedValue(SESSION);
    const cross = await proxy(
      request("/oauth/authorize", { method: "POST", headers: { ...viaProxy, origin: "https://evil.example" }, cookie: `__Host-puls-session=${TOKEN}` }),
    );
    expect(cross.status).toBe(403);
    findSession.mockResolvedValue(null);
    const noSession = await proxy(request("/oauth/authorize", { method: "POST", headers: { ...viaProxy, origin: "https://viewer.example" } }));
    expect(noSession.status).toBe(401);
  });

  it("rewrites the consent form's POST to its handler", async () => {
    Object.assign(process.env, OAUTH_ENV);
    findSession.mockResolvedValue(SESSION);
    const res = await proxy(
      request("/oauth/authorize", { method: "POST", headers: { ...viaProxy, origin: "https://viewer.example" }, cookie: `__Host-puls-session=${TOKEN}` }),
    );
    expect(new URL(res.headers.get("x-middleware-rewrite")!).pathname).toBe("/oauth/authorize/decision");
  });

  it("lets the consent page's form lead to the redirect URI's origin, and only there", async () => {
    Object.assign(process.env, OAUTH_ENV);
    findSession.mockResolvedValue(SESSION);
    const res = await proxy(request(AUTHORIZE, { headers: viaProxy, cookie: `__Host-puls-session=${TOKEN}` }));
    expect(passed(res)).toBe(true);
    const csp = res.headers.get("content-security-policy")!;
    expect(csp).toContain("form-action 'self' https://claude.ai;");
    expect(csp).toContain("frame-ancestors 'none'");
    const other = await proxy(request("/account?redirect_uri=https%3A%2F%2Fclaude.ai%2Fcb", { headers: viaProxy, cookie: `__Host-puls-session=${TOKEN}` }));
    expect(other.headers.get("content-security-policy")).toContain("form-action 'self' puls:;");
    expect(other.headers.get("content-security-policy")).not.toContain("https://evil.example");
    const bogus = await proxy(
      request(`/oauth/authorize?redirect_uri=${encodeURIComponent("javascript:alert(1)")}`, { headers: viaProxy, cookie: `__Host-puls-session=${TOKEN}` }),
    );
    expect(bogus.headers.get("content-security-policy")).toContain("form-action 'self';");
  });
});


describe("phone connection handoff", () => {
  it("remembers the connection page across sign-in and blocks foreign POSTs", async () => {
    process.env.WEB_ACCOUNTS = "true";
    process.env.TRUST_PROXY_HEADERS = "true";
    findSession.mockResolvedValue(null);
    const login = await proxy(request("/connect/iphone", { headers: viaProxy }));
    expect(new URL(login.headers.get("location")!).searchParams.get("next")).toBe("/connect/iphone");
    findSession.mockResolvedValue(SESSION);
    const rejected = await proxy(request("/api/auth/connect-iphone", { method: "POST", headers: { ...viaProxy, origin: "https://evil.example" }, cookie: `__Host-puls-session=${TOKEN}` }));
    expect(rejected.status).toBe(403);
  });
  it("allows the fixed app callback only on the connection pages", async () => {
    process.env.WEB_ACCOUNTS = "true";
    process.env.TRUST_PROXY_HEADERS = "true";
    findSession.mockResolvedValue(SESSION);
    for (const path of ["/connect/iphone", "/api/auth/connect-iphone", "/account", "/login"]) {
      const res = await proxy(request(path, { headers: viaProxy, cookie: `__Host-puls-session=${TOKEN}` }));
      expect(res.headers.get("content-security-policy")!.includes("puls:")).toBe(path !== "/login");
    }
  });
});
