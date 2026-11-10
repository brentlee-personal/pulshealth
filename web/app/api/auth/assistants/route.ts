import type { NextRequest } from "next/server";

import { accountsOnly, field, readForm, requestSession, seeOther } from "@/lib/accounts/http";
import { revokeConnectedApp } from "@/lib/oauth/store";

// "Revoke" next to an AI assistant on the account page: the grant stops
// refreshing at once, and the access token it already holds expires within
// 30 minutes (the MCP server does not ask the database). Only the account's
// own grants. Works whether or not OAuth is still on, so a grant left over
// from before it was switched off can still be revoked.
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  const off = accountsOnly();
  if (off) return off;
  const session = await requestSession(request);
  if (!session) return seeOther("/login?next=%2Faccount");
  const form = await readForm(request);
  try {
    await revokeConnectedApp(session.accountId, field(form, "id"));
    return seeOther("/account?notice=assistant_revoked");
  } catch (e) {
    console.error("[puls-web] assistant revocation failed:", e instanceof Error ? e.message : e);
    return seeOther("/account?error=failed");
  }
}
