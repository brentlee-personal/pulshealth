import type { NextRequest } from "next/server";

import { connectPhone } from "@/lib/accounts/connect";
import { accountsOnly, field, readForm, requestSession, seeOther } from "@/lib/accounts/http";

export const dynamic = "force-dynamic";

// A form POST, never a GET: mint exactly for the live session's own user,
// then hand the ordinary pairing link directly to iOS's authentication sheet.
export async function POST(request: NextRequest) {
  const off = accountsOnly();
  if (off) return off;
  const session = await requestSession(request);
  if (!session) return seeOther("/login?next=%2Fconnect%2Fiphone");
  const state = await connectPhone(session, field(await readForm(request), "name"));
  if (!state.ok) return seeOther(`/connect/iphone?error=${state.code}`);
  return seeOther(state.link);
}
