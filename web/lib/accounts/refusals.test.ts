import { oauthKeyFixture } from "../oauth/keyFixture";
// Every app/api route handler and both server actions, refusing what accounts
// mode says they must refuse (docs/deep-review-2026-10.md §6 #7):
//
//   - proxy.ts, in front of all of them: a protected route without a session
//     is a 401, and any state-changing request from another origin (or with
//     no Origin at all) is a 403 — public routes included;
//   - the handlers again, should the proxy ever let one through: no session
//     is a 303 to sign in, a session without the right standing (not an
//     administrator; not a self-service account) is a 303 with
//     error=forbidden, and in either case the database is never reached;
//   - the server actions (/admin's Approve, the account page's Connect this
//     iPhone), which run outside the route table but behind the same proxy;
//   - a demo session (WEB_DEMO_USER, view-only): every session route, both
//     server actions and the OAuth consent decision refuse it — a 303 to
//     /account?error=demo, or the consent's 403 — before the database.
//
// ROUTES below is checked against the files on disk and against each
// module's exported methods, so a new route.ts (or a new method on an old
// one) fails here until it is given a row. The database is mocked to throw:
// any refusal path that touches it fails the test.

import { readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { NextRequest, type NextResponse } from "next/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const oauthKeys = oauthKeyFixture();
const db = vi.hoisted(() => {
  const refuse = vi.fn(async () => {
    throw new Error("the database was reached on a refusal path");
  });
  return { query: refuse, scoped: refuse, transaction: refuse, longStatement: refuse, getPool: vi.fn(() => null) };
});
const findSession = vi.hoisted(() => vi.fn());
const cookieValue = vi.hoisted(() => ({ current: undefined as string | undefined }));
const mail = vi.hoisted(() => ({ notifyNewRequest: vi.fn(), sendApproval: vi.fn(), notifyDeletion: vi.fn(), publicBase: vi.fn(() => "") }));

vi.mock("@/lib/db", async (importOriginal) => ({ ...(await importOriginal<typeof import("@/lib/db")>()), ...db }));
vi.mock("@/lib/accounts/session", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/accounts/session")>()),
  findSession,
}));
vi.mock("@/lib/accounts/mail", async (importOriginal) => ({ ...(await importOriginal<typeof import("@/lib/accounts/mail")>()), ...mail }));
// The server actions read the session cookie through next/headers, which
// needs a request scope; here it is whatever each test sets.
vi.mock("next/headers", () => ({
  cookies: async () => ({ get: () => (cookieValue.current === undefined ? undefined : { value: cookieValue.current }) }),
  headers: async () => new Headers({ host: "viewer.example" }),
}));

type Method = "GET" | "HEAD" | "POST" | "PUT" | "PATCH" | "DELETE" | "OPTIONS";
const HTTP_METHODS: Method[] = ["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"];

/** How a route is guarded in accounts mode. */
type Guard =
  /** proxy.ts: 401 without a session. The handler: `noSession` without one, `forbidden` for these sessions. */
  | { kind: "session"; noSession: string; forbidden?: { session: Partial<Session>; location: string }[] }
  /** Reachable without a session by design (lib/accounts/policy.ts PUBLIC_API); `why` says why that is safe. */
  | { kind: "public"; why: string }
  /** The container health check: no session, any scheme. Returns no personal data. */
  | { kind: "health" }
  /** HTTPS machine endpoint; bearer authorization is enforced by its handler. */
  | { kind: "oauth" }
  /** Does not exist in accounts mode: the handler answers 404 (and the proxy wants a session first). */
  | { kind: "absent" };

interface Session {
  id: Buffer;
  accountId: string;
  userId: string;
  email: string;
  isAdmin: boolean;
  selfService: boolean;
  demo: boolean;
  refreshed: boolean;
}

const ROUTES: Record<string, { methods: Method[]; guard: Guard }> = {
  "/api/app-store/notifications/[environment]": { methods: ["POST"], guard: { kind: "oauth" } },
  "/api/native/profile": { methods: ["GET", "POST", "PUT", "DELETE"], guard: { kind: "oauth" } },
  "/api/auth/connect-account": { methods: ["POST"], guard: { kind: "session", noSession: "/login?next=%2Faccount" } },
  "/api/health/v1/[...path]": { methods: ["GET"], guard: { kind: "oauth" } },
  "/api/auth/forgot-password": { methods: ["POST"], guard: { kind: "public", why: "Generic response; bounded email recovery requests." } },
  "/api/auth/reset-password": { methods: ["POST"], guard: { kind: "public", why: "Requires expiring one-use recovery token." } },
  "/api/admin": {
    methods: ["POST"],
    guard: {
      kind: "session",
      noSession: "/admin?error=forbidden",
      forbidden: [
        { session: { isAdmin: false, selfService: true }, location: "/admin?error=forbidden" },
        { session: { isAdmin: false, selfService: false }, location: "/admin?error=forbidden" },
      ],
    },
  },
  "/api/auth/delete-account": {
    methods: ["POST"],
    guard: {
      kind: "session",
      noSession: "/login?next=%2Faccount",
      forbidden: [
        // Administrators and household accounts are the operator's to remove.
        { session: { isAdmin: true, selfService: true }, location: "/account?error=forbidden" },
        { session: { isAdmin: false, selfService: false }, location: "/account?error=forbidden" },
      ],
    },
  },
  "/api/auth/time-zone": {
    methods: ["POST"],
    guard: { kind: "session", noSession: "/login?next=%2Faccount" },
  },
  "/api/auth/devices": {
    methods: ["POST"],
    guard: {
      kind: "session",
      noSession: "/login?next=%2Faccount",
    },
  },
  "/api/auth/connect-iphone": {
    methods: ["POST"], guard: { kind: "session", noSession: "/login?next=%2Fconnect%2Fiphone" },
  },
  "/api/auth/assistants": { methods: ["POST"], guard: { kind: "session", noSession: "/login?next=%2Faccount" } },
  "/api/auth/password": { methods: ["POST"], guard: { kind: "session", noSession: "/login?next=%2Faccount" } },
  "/api/auth/sessions": { methods: ["POST"], guard: { kind: "session", noSession: "/login?next=%2Faccount" } },
  "/api/auth/login": { methods: ["POST"], guard: { kind: "public", why: "signing in" } },
  "/api/auth/invite": { methods: ["POST"], guard: { kind: "public", why: "the invite token is the credential (rate-limited)" } },
  "/api/auth/logout": { methods: ["POST"], guard: { kind: "public", why: "acts only on the cookie it is sent" } },
  "/api/auth/signup": { methods: ["POST"], guard: { kind: "public", why: "records a request; creates nothing until approved" } },
  "/api/healthz": { methods: ["GET"], guard: { kind: "health" } },
  "/api/user": { methods: ["POST"], guard: { kind: "absent" } },
};

const API_DIR = fileURLToPath(new URL("../../app/api/", import.meta.url));

/** Every app/api/**\/route.ts on disk, as the URL path it serves. */
function routesOnDisk(): string[] {
  return (readdirSync(API_DIR, { recursive: true }) as string[])
    .map((f) => f.split("\\").join("/"))
    .filter((f) => /(^|\/)route\.(ts|tsx|js)$/.test(f))
    .map((f) => `/api/${f.replace(/\/?route\.(ts|tsx|js)$/, "")}`.replace(/\/$/, ""))
    .sort();
}

async function routeModule(path: string): Promise<Record<string, unknown>> {
  // A computed path, which Vite cannot analyse; the file is the one routesOnDisk found.
  return import(/* @vite-ignore */ `${API_DIR}${path.slice("/api/".length)}/route.ts`);
}

const TOKEN = "A".repeat(43);
const COOKIE = `__Host-puls-session=${TOKEN}`;
const SESSION: Session = {
  id: Buffer.alloc(32, 7),
  accountId: "10000000-0000-4000-8000-000000000001",
  userId: "11111111-1111-4111-8111-111111111111",
  email: "person@example.com",
  isAdmin: false,
  selfService: true,
  demo: false,
  refreshed: false,
};
// The shared demo account's session, as findSession returns it.
const DEMO: Session = { ...SESSION, email: "demo@demo.invalid", isAdmin: false, selfService: false, demo: true };
const OTHER = "20000000-0000-4000-8000-000000000002";

// A browser behind the TLS proxy (TRUST_PROXY_HEADERS=true), same origin.
const VIA_PROXY = { host: "viewer.example", "x-forwarded-proto": "https", "x-forwarded-for": "203.0.113.5" };
const SAME_ORIGIN = { ...VIA_PROXY, origin: "https://viewer.example", "sec-fetch-site": "same-origin" };

// A form that would do something real if it were let through: every field
// any of the handlers reads, filled in.
function form(): FormData {
  const body = new FormData();
  for (const [k, v] of [
    ["action", "purge"],
    ["id", OTHER],
    ["confirm", "yes"],
    ["session", "others"],
    ["current", "the old passphrase"],
    ["password", "a new long passphrase"],
    ["mode", "approve"],
    ["name", "iPhone"],
  ]) {
    body.append(k, v);
  }
  return body;
}

function request(path: string, method: Method, headers: Record<string, string>, cookie = true): NextRequest {
  const h = new Headers(headers);
  if (cookie) h.set("cookie", COOKIE);
  const init: { method: string; headers: Headers; body?: FormData } = { method, headers: h };
  if (method !== "GET" && method !== "HEAD") init.body = form();
  return new NextRequest(new URL(path.replace("[environment]", "production"), "http://web:3000"), init);
}

async function proxy(req: NextRequest): Promise<Response> {
  if (req.nextUrl.pathname.startsWith("/api/health/")) {
    process.env.WEB_PUBLIC_URL = "https://viewer.example";
    process.env.PULS_MCP_URL = "https://mcp.example/mcp";
    delete process.env.PULS_MCP_OAUTH_SECRET;
    Object.assign(process.env, oauthKeys.env);
  }
  const { default: run } = await import("@/proxy");
  return run(req);
}
const passed = (res: Response) => res.headers.get("x-middleware-next") === "1";

async function handle(path: string, method: Method, req: NextRequest): Promise<NextResponse> {
  const handler = (await routeModule(path))[method] as (r: NextRequest) => Promise<NextResponse>;
  return handler(req);
}

const saved = { ...process.env };
beforeEach(() => {
  vi.stubEnv("NODE_ENV", "production");
  process.env.WEB_ACCOUNTS = "true";
  process.env.WEB_SIGNUPS = "true";
  process.env.TRUST_PROXY_HEADERS = "true";
  process.env.WEB_INGEST_URL = "https://ingest.example";
  delete process.env.WEB_AUTH_PASSWORD;
  delete process.env.WEB_PUBLIC_URL;
  delete process.env.WEB_CLIENT_IP_HEADER;
  findSession.mockReset();
  for (const fn of [...Object.values(db), ...Object.values(mail)]) fn.mockClear();
  cookieValue.current = TOKEN;
  vi.spyOn(console, "error").mockImplementation(() => {});
});
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllEnvs();
  process.env = { ...saved };
});

function expectNothingReached() {
  for (const [name, fn] of Object.entries(db)) {
    if (name !== "getPool") expect(fn, `db.${name}`).not.toHaveBeenCalled();
  }
  for (const [name, fn] of Object.entries(mail)) {
    if (name !== "publicBase") expect(fn, `mail.${name}`).not.toHaveBeenCalled();
  }
}

const rows = Object.entries(ROUTES).flatMap(([path, { methods, guard }]) => methods.map((method) => ({ path, method, guard })));

describe("the route table", () => {
  it("lists every app/api route on disk, and nothing else", () => {
    expect(Object.keys(ROUTES).sort()).toEqual(routesOnDisk());
  });

  it.each(Object.keys(ROUTES))("%s exports exactly the methods its row lists", async (path) => {
    const mod = await routeModule(path);
    const exported = HTTP_METHODS.filter((m) => typeof mod[m] === "function");
    expect(exported).toEqual(ROUTES[path].methods);
  });

  it("agrees with the proxy's own list of routes reachable without a session", async () => {
    const { PUBLIC_API } = await import("./policy");
    const publicRows = Object.entries(ROUTES)
      .filter(([, r]) => r.guard.kind === "public")
      .map(([p]) => p)
      .sort();
    expect(publicRows).toEqual([...PUBLIC_API].sort());
  });
});

describe("proxy.ts in front of every route (accounts mode)", () => {
  it.each(rows)("$method $path without a session", async ({ path, method, guard }) => {
    findSession.mockResolvedValue(null);
    const res = await proxy(request(path, method, SAME_ORIGIN));
    if (guard.kind === "session" || guard.kind === "absent") {
      expect(res.status).toBe(401);
      expect(passed(res)).toBe(false);
    } else {
      expect(passed(res)).toBe(true);
    }
  });

  it.each(rows.filter((r) => r.method !== "GET" && r.method !== "HEAD" && r.guard.kind !== "oauth"))(
    "$method $path from another origin, or naming none, even with a live session",
    async ({ path, method }) => {
      findSession.mockResolvedValue({ ...SESSION, isAdmin: true });
      for (const headers of [
        { ...VIA_PROXY, origin: "https://evil.example" },
        { ...VIA_PROXY },
        { ...VIA_PROXY, origin: "null" },
        { ...SAME_ORIGIN, "sec-fetch-site": "cross-site" },
        { ...SAME_ORIGIN, "sec-fetch-site": "same-site" },
      ]) {
        const res = await proxy(request(path, method, headers));
        expect(res.status, JSON.stringify(headers)).toBe(403);
      }
      expect(findSession).not.toHaveBeenCalled();
    },
  );

  it.each(rows.filter((r) => r.guard.kind !== "health"))("$method $path over plain HTTP", async ({ path, method }) => {
    delete process.env.TRUST_PROXY_HEADERS;
    findSession.mockResolvedValue({ ...SESSION, isAdmin: true });
    const res = await proxy(request(path, method, { host: "viewer.example", origin: "http://viewer.example" }));
    expect(res.status).toBe(403);
    expect(findSession).not.toHaveBeenCalled();
  });
});

describe("the handlers themselves, should a request get past the proxy", () => {
  const guarded = rows.filter((r) => r.guard.kind === "session") as { path: string; method: Method; guard: Extract<Guard, { kind: "session" }> }[];

  it.each(guarded)("$method $path without a session: 303 to $guard.noSession", async ({ path, method, guard }) => {
    findSession.mockResolvedValue(null);
    const res = await handle(path, method, request(path, method, SAME_ORIGIN));
    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe(guard.noSession);
    expect(res.cookies.get("__Host-puls-session")).toBeUndefined();
    expectNothingReached();
  });

  it.each(guarded.filter((r) => r.guard.forbidden))("$method $path refuses sessions without the standing", async ({ path, method, guard }) => {
    for (const { session, location } of guard.forbidden!) {
      findSession.mockResolvedValue({ ...SESSION, ...session });
      const res = await handle(path, method, request(path, method, SAME_ORIGIN));
      expect(res.status, JSON.stringify(session)).toBe(303);
      expect(res.headers.get("location"), JSON.stringify(session)).toBe(location);
    }
    expectNothingReached();
  });

  // Every route that needs a session changes something about an account,
  // so every one refuses the view-only demo; a new one fails here until it
  // does (lib/accounts/http.ts refuseDemo).
  it.each(guarded)("$method $path refuses a demo session: 303 to /account?error=demo", async ({ path, method }) => {
    // Even a demo account whose row says administrator or self-service.
    for (const session of [DEMO, { ...DEMO, isAdmin: true, selfService: true }]) {
      findSession.mockResolvedValue(session);
      const res = await handle(path, method, request(path, method, SAME_ORIGIN));
      expect(res.status, JSON.stringify(session)).toBe(303);
      expect(res.headers.get("location")).toBe(path === "/api/auth/connect-iphone" ? "/connect/iphone?error=demo" : "/account?error=demo");
      expect(res.cookies.get("__Host-puls-session")).toBeUndefined();
    }
    expectNothingReached();
  });

  it("POST /api/auth/logout still signs a demo session out (it ends only this browser's session)", async () => {
    findSession.mockResolvedValue(DEMO);
    db.query.mockImplementationOnce(async () => [] as never);
    const res = await handle("/api/auth/logout", "POST", request("/api/auth/logout", "POST", SAME_ORIGIN));
    expect(res.headers.get("location")).toBe("/login?notice=signed-out");
    expect(res.cookies.get("__Host-puls-session")?.maxAge).toBe(0);
    expect(db.query).toHaveBeenCalledTimes(1);
    expect(String((db.query.mock.calls[0] as unknown[])[0])).toBe("DELETE FROM auth.sessions WHERE id = $1");
  });

  it.each(rows.filter((r) => r.guard.kind === "absent"))("$method $path does not exist in accounts mode", async ({ path, method }) => {
    findSession.mockResolvedValue({ ...SESSION, isAdmin: true });
    const res = await handle(path, method, request(path, method, SAME_ORIGIN));
    expect(res.status).toBe(404);
    expect(res.cookies.get("puls-user")).toBeUndefined();
    expectNothingReached();
  });

  it.each(rows.filter((r) => r.guard.kind === "session" || r.guard.kind === "public"))(
    "$method $path is a 404 outside accounts mode",
    async ({ path, method }) => {
      delete process.env.WEB_ACCOUNTS;
      const res = await handle(path, method, request(path, method, SAME_ORIGIN));
      expect(res.status).toBe(404);
      expect(findSession).not.toHaveBeenCalled();
      expectNothingReached();
    },
  );
});

describe("the server actions", () => {
  it("Approve refuses without a session and for anyone but an administrator", async () => {
    const { approveRequest } = await import("@/app/admin/actions");
    cookieValue.current = undefined;
    findSession.mockResolvedValue(null);
    expect(await approveRequest(null, form())).toEqual({ ok: false, error: "Only an administrator can approve requests." });
    cookieValue.current = TOKEN;
    for (const session of [{ isAdmin: false, selfService: true }, { isAdmin: false, selfService: false }]) {
      findSession.mockResolvedValue({ ...SESSION, ...session });
      expect(await approveRequest(null, form())).toEqual({ ok: false, error: "Only an administrator can approve requests." });
    }
    // A database that cannot be asked is no session either.
    findSession.mockRejectedValue(new Error("connection refused"));
    expect(await approveRequest(null, form())).toMatchObject({ ok: false });
    // And outside accounts mode there is no administrator at all.
    delete process.env.WEB_ACCOUNTS;
    findSession.mockResolvedValue({ ...SESSION, isAdmin: true });
    expect(await approveRequest(null, form())).toMatchObject({ ok: false });
    expectNothingReached();
  });

  it("Connect this iPhone refuses without a live accounts-mode session", async () => {
    const { connectIphone } = await import("@/app/account/actions");
    cookieValue.current = undefined;
    findSession.mockResolvedValue(null);
    expect(await connectIphone(null, form())).toEqual({ ok: false, error: "Sign in again, then try once more." });
    cookieValue.current = TOKEN;
    findSession.mockRejectedValue(new Error("connection refused"));
    expect(await connectIphone(null, form())).toMatchObject({ ok: false });
    delete process.env.WEB_ACCOUNTS;
    findSession.mockResolvedValue(SESSION);
    expect(await connectIphone(null, form())).toMatchObject({ ok: false });
    expectNothingReached();
  });

  it("both refuse a demo session, whatever its rows say", async () => {
    const { approveRequest } = await import("@/app/admin/actions");
    const { connectIphone } = await import("@/app/account/actions");
    for (const session of [DEMO, { ...DEMO, isAdmin: true, selfService: true }]) {
      findSession.mockResolvedValue(session);
      expect(await approveRequest(null, form())).toEqual({ ok: false, error: "Only an administrator can approve requests." });
      expect(await connectIphone(null, form())).toEqual({ ok: false, error: expect.stringMatching(/^Not available on the demo account/) });
    }
    expectNothingReached();
  });
});

describe("the OAuth consent decision", () => {
  it("never connects an assistant to the demo account", async () => {
    process.env.WEB_PUBLIC_URL = "https://viewer.example";
    process.env.PULS_MCP_URL = "https://mcp.example/mcp";
    delete process.env.PULS_MCP_OAUTH_SECRET;
  Object.assign(process.env, oauthKeys.env);
    const { POST } = await import("@/app/oauth/authorize/decision/route");
    const body = new FormData();
    for (const [k, v] of [
      ["decision", "allow"],
      ["client_id", "pc_AAAAAAAAAAAAAAAAAAAAAAAA"],
      ["redirect_uri", "https://claude.ai/api/mcp/auth_callback"],
      ["response_type", "code"],
      ["code_challenge", "p_j-Nj_Xa-C0MOoCl6lv9UzGgSvHUvKaouTlEnyXOiE"],
      ["code_challenge_method", "S256"],
      ["scope", "health:read"],
    ]) {
      body.append(k, v);
    }
    findSession.mockResolvedValue(DEMO);
    const res = await POST(
      new NextRequest(new URL("/oauth/authorize/decision", "http://web:3000"), { method: "POST", headers: new Headers(SAME_ORIGIN), body }),
    );
    expect(res.status).toBe(403);
    expect(res.headers.get("location")).toBeNull();
    expect(await res.text()).toMatch(/AI assistants cannot connect to the demo account/);
    expectNothingReached();
  });
});
