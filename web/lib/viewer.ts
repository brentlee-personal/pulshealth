import { cookies } from "next/headers";
import { redirect } from "next/navigation";
import { cache } from "react";
import { findSession, SESSION_COOKIE, type Session } from "./accounts/session";

/**
 * Where to send the browser after a choice or a sign-in: `value` when it is a
 * same-origin path, otherwise `/`. Anything with a scheme or host
 * (`https://…`, `//…`, `\\…`) is refused so a `next` field cannot be pointed
 * off-site — and so is any control character or backslash, because browsers
 * strip tabs and newlines from a URL before parsing it: `/<TAB>/evil.example`
 * would otherwise arrive as `//evil.example`.
 */
export function safeReturnPath(value: string | null | undefined): string {
  if (!value || !value.startsWith("/") || value.startsWith("//")) return "/";
  if (/[\u0000-\u001f\u007f\\]/.test(value)) return "/";
  const base = "http://viewer.invalid";
  try {
    const url = new URL(value, base);
    const path = `${url.pathname}${url.search}${url.hash}`;
    // Parsing resolves dot segments, and `/.//evil.example` comes out as
    // `//evil.example` — another site. Check what comes out, not just what
    // went in.
    if (url.origin !== base || path.startsWith("//") || path.includes("\\")) return "/";
    return path;
  } catch {
    return "/";
  }
}

/**
 * The personal session, checked against storage once per request.
 */
export const currentSession = cache(async (): Promise<Session | null> => {
  const jar = await cookies();
  return findSession(jar.get(SESSION_COOKIE)?.value);
});

/** The authenticated member; a URL or chooser cookie cannot replace them. */
export const viewerUser = cache(async (): Promise<string> => {
  const session = await currentSession();
  if (!session) redirect("/login");
  return session.userId;
});
