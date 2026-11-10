import { NextResponse, type NextRequest } from "next/server";

import { decideAccounts, unauthenticatedAnswer } from "@/lib/accounts/policy";
import { isSameOriginRequest, isSecureRequest, publicOrigin } from "@/lib/accounts/request";
import { findSession, SESSION_COOKIE, sessionCookieAttributes } from "@/lib/accounts/session";
import { trustProxyHeaders, viewerMode } from "@/lib/mode";
import { isOAuthPath, OAUTH_AUTHORIZE_PATH, OAUTH_DECISION_PATH, readOAuthConfig } from "@/lib/oauth/config";
import { redirectOrigin } from "@/lib/oauth/validate";
import { contentSecurityPolicy, newNonce } from "@/lib/securityHeaders";
import { PATH_HEADER } from "@/lib/shell";
import { safeReturnPath } from "@/lib/viewer";

// Every private route requires a live personal session.
export default async function proxy(request: NextRequest) {
  viewerMode();
  const nonce = newNonce();
  const development = process.env.NODE_ENV !== "production";
  const { pathname, searchParams } = request.nextUrl;
  const oauthPath = isOAuthPath(pathname);
  // The consent page's form leads (by 303) to the client's redirect URI.
  const consentTarget =
    oauthPath && pathname === OAUTH_AUTHORIZE_PATH && request.method === "GET" && searchParams.getAll("redirect_uri").length === 1
      ? redirectOrigin(searchParams.get("redirect_uri"))
      : null;
  const formActions = consentTarget ? [consentTarget] : [];
  // The phone connection form returns to the fixed puls://pair callback.
  // Browsers apply form-action to the POST's redirect too.
  if (pathname === "/connect/account" || pathname === "/api/auth/connect-account" || pathname === "/connect/iphone" || pathname === "/api/auth/connect-iphone" || pathname === "/account") {
    formActions.push("puls:");
  }
  const csp = contentSecurityPolicy(nonce, development, formActions);

  if (oauthPath) {
    const setting = readOAuthConfig();
    if (setting.state !== "on") {
      const blocked = setting.state === "off" ? text(404, "Not found") : text(503, "Authorization unavailable");
      blocked.headers.set("Content-Security-Policy", csp);
      return blocked;
    }
  }

  const response = await accountsGate(request, nonce, csp, development);
  response.headers.set("Content-Security-Policy", csp);
  return response;
}

/** Hands the request on, with the nonce where the layout and Next.js read it. */
function serve(request: NextRequest, nonce: string, csp: string): NextResponse {
  const headers = new Headers(request.headers);
  headers.set("x-nonce", nonce);
  headers.set("Content-Security-Policy", csp);
  // The layout serves pages under /oauth/ without the sidebar (app/layout.tsx).
  // Always overwritten, so a client cannot choose it; it only picks a frame.
  headers.set(PATH_HEADER, request.nextUrl.pathname);
  // The consent form posts to the page's own address; its handler lives beside it.
  if (request.method === "POST" && request.nextUrl.pathname === OAUTH_AUTHORIZE_PATH) {
    return NextResponse.rewrite(new URL(OAUTH_DECISION_PATH, request.url), { request: { headers } });
  }
  return NextResponse.next({ request: { headers } });
}

function text(status: number, body: string, extra: Record<string, string> = {}): NextResponse {
  return new NextResponse(`${body}\n`, {
    status,
    headers: { "Content-Type": "text/plain; charset=utf-8", "Cache-Control": "no-store", ...extra },
  });
}

async function accountsGate(request: NextRequest, nonce: string, csp: string, development: boolean): Promise<NextResponse> {
  const url = request.nextUrl;
  const trust = trustProxyHeaders();
  const forwardedHttp = trust && request.headers.get("x-forwarded-proto")?.split(",")[0]?.trim().toLowerCase() === "http";
  const decision = decideAccounts({
    pathname: url.pathname,
    secure: isSecureRequest(url, request.headers, trust, development),
    sameOrigin: isSameOriginRequest(request.method, url, request.headers, trust, development, process.env.WEB_PUBLIC_URL),
    development,
    forwardedHttp,
  });

  if (decision === "pass") return serve(request, nonce, csp);
  if (decision === "upgrade") {
    // The proxy reached us for a browser on plain http: send it to https on
    // the public origin (never echoing a Host it might not own), before any
    // cookie or form travels in the clear. 308 keeps a POST a POST.
    const origin = publicOrigin(url, request.headers, trust, process.env.WEB_PUBLIC_URL).replace(/^http:/, "https:");
    return new NextResponse(null, {
      status: 308,
      headers: { Location: new URL(`${url.pathname}${url.search}`, origin).toString(), "Cache-Control": "no-store" },
    });
  }
  if (decision === "insecure") {
    return text(
      403,
      "This viewer signs people in, so it answers only over HTTPS. Reach it through its HTTPS address; " +
        "an operator running it behind a TLS proxy sets TRUST_PROXY_HEADERS=true (see web/README.md, \"Access control\").",
    );
  }
  if (decision === "cross-origin") return text(403, "Cross-site request refused.");

  const token = request.cookies.get(SESSION_COOKIE)?.value;
  let session;
  try {
    session = await findSession(token, true);
  } catch (e) {
    // Never a sign-in redirect: the session may be fine and the database not.
    console.error("[puls-web] session lookup failed:", e instanceof Error ? e.message : e);
    return text(503, "Sign-in is unavailable right now: the database cannot be reached.", { "Retry-After": "30" });
  }

  if (!session) {
    const response =
      unauthenticatedAnswer(request.method, url.pathname) === "redirect"
        ? signInRedirect(request, trust)
        : text(401, "Sign in required.");
    // A cookie that names no live session is dead weight; drop it.
    if (token !== undefined) response.cookies.set(SESSION_COOKIE, "", sessionCookieAttributes(0));
    return response;
  }

  const response = serve(request, nonce, csp);
  // An active session's expiry just slid forward; let the cookie follow.
  if (session.refreshed && token) response.cookies.set(SESSION_COOKIE, token, sessionCookieAttributes());
  return response;
}

// To /login, remembering where the person was going. Next.js refuses a
// relative Location from the proxy, so it is absolute — on the origin the
// browser used (lib/accounts/request.ts publicOrigin), never a proxy's
// internal host name.
function signInRedirect(request: NextRequest, trust: boolean): NextResponse {
  const target = new URL(request.nextUrl.toString());
  target.searchParams.delete("_rsc"); // client-navigation plumbing, not the page's own query
  const next = safeReturnPath(`${target.pathname}${target.search}`);
  const location = new URL(next === "/" ? "/login" : `/login?next=${encodeURIComponent(next)}`, publicOrigin(request.nextUrl, request.headers, trust, process.env.WEB_PUBLIC_URL));
  return new NextResponse(null, { status: 303, headers: { Location: location.toString(), "Cache-Control": "no-store" } });
}
