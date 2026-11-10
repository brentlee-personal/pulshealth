import type { NextRequest } from "next/server";

import { accountsOnly, field, readForm, requestIp, seeOther, setSessionCookie } from "@/lib/accounts/http";
import { hashPassword, newPasswordProblem } from "@/lib/accounts/password";
import { authFailures, failureKeys, refundAll, takeAll } from "@/lib/accounts/ratelimit";
import { SESSION_COOKIE, tokenHash } from "@/lib/accounts/session";
import { acceptInvite, findInvite } from "@/lib/accounts/store";

// An invite link's form posts here: the token, and the new password twice.
// A good token creates a new account and
// signs the browser in; a bad one is charged to the client's address like a
// wrong password (lib/accounts/ratelimit.ts: taken up front, refunded for
// anything that is not a bad token). Tokens are 256 random bits, so the
// limit is a second line, not the first. Only this POST charges, never the
// GET of the invite page: a page any site can embed must not be able to
// spend a visitor's sign-in attempts.
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  const off = accountsOnly();
  if (off) return off;

  const form = await readForm(request);
  const token = field(form, "token");
  if (!tokenHash(token)) return seeOther("/login?error=invite");
  const page = `/invite/${token}`;
  const keys = failureKeys(requestIp(request));
  if (!takeAll(authFailures, keys).allowed) return seeOther(`${page}?error=throttled`);

  try {
    // A token that names no usable invite keeps its charge.
    if (!(await findInvite(token))) return seeOther(`${page}?error=invalid`);
    const problem = newPasswordProblem(field(form, "password"), field(form, "confirm"));
    if (problem) {
      refundAll(authFailures, keys);
      return seeOther(`${page}?error=${problem}`);
    }

    const previous = tokenHash(request.cookies.get(SESSION_COOKIE)?.value);
    const result = await acceptInvite(token, await hashPassword(field(form, "password")), {
      userAgent: request.headers.get("user-agent"),
      ip: requestIp(request),
    }, previous);
    if (!result.ok) {
      if (result.reason !== "invalid") refundAll(authFailures, keys);
      return seeOther(`${page}?error=${result.reason}`);
    }
    refundAll(authFailures, keys);
    return setSessionCookie(seeOther("/?notice=welcome"), result.sessionToken);
  } catch (e) {
    refundAll(authFailures, keys);
    console.error("[puls-web] invite failed:", e instanceof Error ? e.message : e);
    return seeOther(`${page}?error=unavailable`);
  }
}
