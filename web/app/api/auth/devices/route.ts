import type { NextRequest } from "next/server";

import { accountsOnly, field, readForm, requestSession, seeOther } from "@/lib/accounts/http";
import { revokeMyDevice } from "@/lib/accounts/signups";

// "Disconnect" next to one of the account's own iPhones: its sync token is
// revoked, and the next upload from it is refused. The database checks that
// the token belongs to the session's own user.
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  const off = accountsOnly();
  if (off) return off;
  const session = await requestSession(request);
  if (!session) return seeOther("/login?next=%2Faccount");
  const form = await readForm(request);
  try {
    await revokeMyDevice(session.id, field(form, "id"));
    return seeOther("/account?notice=device_revoked");
  } catch (e) {
    console.error("[puls-web] disconnect failed:", e instanceof Error ? e.message : e);
    return seeOther("/account?error=failed");
  }
}
