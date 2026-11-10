import type { NextRequest } from "next/server";

import { accountsOnly, clearSessionCookie, field, readForm, requestSession, seeOther } from "@/lib/accounts/http";
import { deleteAccountSession, deleteOtherSessions } from "@/lib/accounts/session";

// The account page's session list: sign out one browser (`session` is the
// hex id the page lists), or every browser but this one (`session=others`).
// Only the account's own sessions can be named; signing out the current one
// is signing out.
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  const off = accountsOnly();
  if (off) return off;
  const session = await requestSession(request);
  if (!session) return seeOther("/login?next=%2Faccount");

  const target = field(await readForm(request), "session");
  try {
    if (target === "others") {
      await deleteOtherSessions(session.accountId, session.id);
    } else {
      await deleteAccountSession(session.accountId, target);
      if (target === session.id.toString("hex")) return clearSessionCookie(seeOther("/login?notice=signed-out"));
    }
    return seeOther("/account?notice=sessions");
  } catch (e) {
    console.error("[puls-web] session sign-out failed:", e instanceof Error ? e.message : e);
    return seeOther("/account?error=unavailable");
  }
}
