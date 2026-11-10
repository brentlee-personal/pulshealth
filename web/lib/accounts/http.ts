// Small pieces the accounts route handlers share. Server-only.

import { NextResponse, type NextRequest } from "next/server";
import { trustProxyHeaders, viewerMode } from "../mode";
import { clientIp } from "./request";
import { findSession, SESSION_COOKIE, sessionCookieAttributes, type Session } from "./session";

/**
 * 303 after a form POST. Callers use a fixed same-origin path or the fixed
 * puls://pair callback; a proxy's internal host never appears in Location.
 */
export function seeOther(location: string): NextResponse {
  return new NextResponse(null, { status: 303, headers: { Location: location, "Cache-Control": "no-store" } });
}

/** These routes exist only in accounts mode; elsewhere they are a 404. */
export function accountsOnly(): NextResponse | null {
  if (viewerMode() === "accounts") return null;
  return new NextResponse("Not found\n", { status: 404, headers: { "Content-Type": "text/plain; charset=utf-8" } });
}

/** The client address failures are charged to (lib/accounts/request.ts). */
export function requestIp(request: NextRequest): string {
  return clientIp(request.headers, trustProxyHeaders());
}

/** The live session this request's cookie names, or null. */
export async function requestSession(request: NextRequest): Promise<Session | null> {
  return findSession(request.cookies.get(SESSION_COOKIE)?.value);
}

/** Puts a new session's token in the cookie (`maxAge` seconds; 30 days by default). */
export function setSessionCookie(response: NextResponse, token: string, maxAge?: number): NextResponse {
  response.cookies.set(SESSION_COOKIE, token, sessionCookieAttributes(maxAge));
  return response;
}

export function clearSessionCookie(response: NextResponse): NextResponse {
  response.cookies.set(SESSION_COOKIE, "", sessionCookieAttributes(0));
  return response;
}

/** A form field as a string ("" when absent or a file). */
export function field(form: FormData | null, name: string): string {
  const value = form?.get(name);
  return typeof value === "string" ? value : "";
}

export async function readForm(request: NextRequest): Promise<FormData | null> {
  try {
    return await request.formData();
  } catch {
    return null;
  }
}
