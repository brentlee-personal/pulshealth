import type { NextRequest } from "next/server";
import { accountsOnly, field, readForm, requestSession, seeOther } from "@/lib/accounts/http";
import { query } from "@/lib/db";

export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  const off = accountsOnly();
  if (off) return off;
  const session = await requestSession(request);
  if (!session) return seeOther("/login?next=%2Faccount");
  const form = await readForm(request);
  const zone = field(form, "time_zone").trim();
  try {
    if (!zone || zone.length > 100) throw new Error("Invalid time zone");
    new Intl.DateTimeFormat("en", { timeZone: zone }).format();
  } catch {
    return seeOther("/account?error=time_zone");
  }
  try {
    await query("SELECT auth.set_my_time_zone($1, $2)", [session.id, zone]);
    return seeOther("/account?notice=time_zone");
  } catch (e) {
    console.error("[puls-web] time zone change failed:", e instanceof Error ? e.message : e);
    return seeOther("/account?error=failed");
  }
}
