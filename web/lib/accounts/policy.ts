// Which requests accounts mode lets through, and on what condition. Pure:
// proxy.ts feeds it the facts and carries out the answer, and the tests walk
// every route class through it.

import { APP_STORE_NOTIFICATION_PATHS } from "../appStore/paths";
import { OAUTH_MACHINE_PATHS } from "../oauth/config";

export type RouteClass =
  /** The container health check: always answered, even over plain HTTP. */
  | "health"
  /** Build assets and icons the sign-in page itself needs. Nothing personal. */
  | "asset"
  /** Signing in, asking for an account and accepting an invite need no session. */
  | "public"
  /**
   * The OAuth endpoints other servers and apps call (metadata, registration,
   * token, revocation): HTTPS, but no session and no Origin check — they
   * carry no cookie, and their callers are other origins by nature. They
   * OAuth paths answer 404 unless OAuth is on (lib/oauth/config.ts, proxy.ts).
   * /api/native/profile is also a cookie-free machine endpoint: a PKCE code
   * exchange or a live bearer session, independently of OAuth configuration.
   */
  | "oauth"
  /** Everything else: a valid session or nothing. */
  | "protected";

/** Paths reachable without a session, beyond assets and the health check. */
export const PUBLIC_PAGES = ["/login", "/signup", "/forgot-password"] as const;
// Sign-out too: leaving a session that already expired should not be an
// error. Every POST, these included, still has to pass the origin check.
export const PUBLIC_API = ["/api/auth/login", "/api/auth/invite", "/api/auth/logout", "/api/auth/signup", "/api/auth/forgot-password", "/api/auth/reset-password"] as const;

export function classifyPath(pathname: string, development = false): RouteClass {
  if (pathname === "/api/healthz") return "health";
  if (
    pathname.startsWith("/_next/static/") ||
    pathname === "/icon.svg" ||
    pathname === "/apple-icon.svg" ||
    pathname === "/favicon.ico" ||
    // The dev server's own endpoints (error overlay, HMR); absent in production.
    (development && pathname.startsWith("/__nextjs"))
  ) {
    return "asset";
  }
  if ((APP_STORE_NOTIFICATION_PATHS as readonly string[]).includes(pathname)) return "oauth";
  if (pathname === "/api/native/profile" || pathname.startsWith("/api/health/") || (OAUTH_MACHINE_PATHS as readonly string[]).includes(pathname)) return "oauth";
  if ((PUBLIC_PAGES as readonly string[]).includes(pathname)) return "public";
  if ((PUBLIC_API as readonly string[]).includes(pathname)) return "public";
  // /invite/<token>: exactly one segment, the token.
  if (/^\/(invite|reset-password|deletion)\/[^/]+$/.test(pathname)) return "public";
  return "protected";
}

export type AccountsDecision =
  /** Serve it; no session needed. */
  | "pass"
  /** Plain HTTP that came through the trusted TLS proxy: send it to https. */
  | "upgrade"
  /** Plain HTTP outside development: refuse rather than take a password over it. */
  | "insecure"
  /** A state-changing request from another origin. */
  | "cross-origin"
  /** Serve it only to a valid session. */
  | "session";

export function decideAccounts(facts: {
  pathname: string;
  secure: boolean;
  sameOrigin: boolean;
  development: boolean;
  /** The trusted proxy says the browser came over plain http (and can be redirected). */
  forwardedHttp?: boolean;
}): AccountsDecision {
  const route = classifyPath(facts.pathname, facts.development);
  if (route === "health" || route === "asset") return "pass";
  if (!facts.secure) return facts.forwardedHttp ? "upgrade" : "insecure";
  if (route === "oauth") return "pass";
  if (!facts.sameOrigin) return "cross-origin";
  return route === "public" ? "pass" : "session";
}

/** What a request without a valid session gets: pages go to sign-in, the rest 401. */
export function unauthenticatedAnswer(method: string, pathname: string): "redirect" | "unauthorized" {
  const navigable = method === "GET" || method === "HEAD";
  return navigable && !pathname.startsWith("/api/") ? "redirect" : "unauthorized";
}
