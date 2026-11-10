import type { NextRequest } from "next/server";

import { accountsOnly, field, readForm, requestIp, requestSession, seeOther, setSessionCookie } from "@/lib/accounts/http";
import { hashPassword, newPasswordProblem, verifyPassword } from "@/lib/accounts/password";
import { authFailures, failureKeys, refundAll, takeAll } from "@/lib/accounts/ratelimit";
import { changePasswordAndSession, findPasswordHash } from "@/lib/accounts/store";

// The account page's "Change password" form. It needs the current password
// as well as the session — a borrowed, unlocked browser should not be enough
// to take the account over — and a wrong one is charged like a failed
// sign-in. A change ends every session of the account, this one included,
// and gives this browser a new one: whoever held a copy of any cookie,
// the current one too, is out.
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  const off = accountsOnly();
  if (off) return off;
  const session = await requestSession(request);
  if (!session) return seeOther("/login?next=%2Faccount");

  const form = await readForm(request);
  const password = field(form, "password");
  const problem = newPasswordProblem(password, field(form, "confirm"));
  if (problem) return seeOther(`/account?error=${problem}`);

  const keys = [...failureKeys(requestIp(request)), `account:${session.accountId}`];
  if (!takeAll(authFailures, keys).allowed) return seeOther("/account?error=throttled");
  try {
    const current = await findPasswordHash(session.accountId);
    if (!current || !(await verifyPassword(field(form, "current"), current))) return seeOther("/account?error=current");
    const token = await changePasswordAndSession(session.accountId, current, session.id, await hashPassword(password), {
      userAgent: request.headers.get("user-agent"),
      ip: requestIp(request),
    });
    if (!token) return seeOther("/account?error=current");
    refundAll(authFailures, keys);
    return setSessionCookie(seeOther("/account?notice=password"), token);
  } catch (e) {
    refundAll(authFailures, keys);
    console.error("[puls-web] password change failed:", e instanceof Error ? e.message : e);
    return seeOther("/account?error=unavailable");
  }
}
