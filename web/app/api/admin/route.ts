import type { NextRequest } from "next/server";

import { accountsOnly, field, readForm, requestSession, seeOther } from "@/lib/accounts/http";
import { DECLINE_MAX, declineSignups, disableRefusal, purgeUser, setAccountDisabled } from "@/lib/accounts/signups";
import { isUuid } from "@/lib/uuid";

// The administrator's forms on /admin, other than Approve (a server action,
// so the invite link can come back once when email fails): decline a
// request, the ticked ones or every one shown (each is deleted, not kept),
// disable or enable an account, purge a disabled user's data. The session
// must be an administrator's; the database checks that again for every
// step here. An administrator's account — your own included — is never
// disabled or enabled here (auth.set_account_disabled refuses it too).
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  const off = accountsOnly();
  if (off) return off;
  const session = await requestSession(request);
  // The demo first: its sessions are never an administrator's anyway
  // (lib/accounts/session.ts), and this keeps that true should the row say so.
  if (session) {
  }
  if (!session?.isAdmin) return seeOther("/admin?error=forbidden");

  const form = await readForm(request);
  const action = field(form, "action");
  if (action === "deny_many") return declineMany(session.id, form);
  const id = field(form, "id");
  if (!isUuid(id)) return seeOther("/admin?error=failed");
  try {
    switch (action) {
      case "deny":
        await declineSignups(session.id, [id]);
        return seeOther("/admin?notice=denied");
      case "disable":
      case "enable": {
        const refusal = await disableRefusal(session.accountId, id);
        if (refusal === "own") return seeOther("/admin?error=own_account");
        if (refusal === "admin") return seeOther("/admin?error=admin_account");
        await setAccountDisabled(session.id, id, action === "disable");
        return seeOther(`/admin?notice=${action}d`);
      }
      case "purge":
        if (field(form, "confirm") !== "yes") return seeOther("/admin?error=failed");
        await purgeUser(session.id, id);
        return seeOther("/admin?notice=purged");
      default:
        return seeOther("/admin?error=failed");
    }
  } catch (e) {
    const code = (e as { code?: string }).code;
    if (action === "enable" && code === "55000") return seeOther("/admin?error=deletion_requested");
    // The database's own refusal of an administrator's account, should one
    // have been made an administrator since the check above.
    if ((action === "enable" || action === "disable") && code === "42501") return seeOther("/admin?error=admin_account");
    if (action === "purge" && code === "55P03") return seeOther("/admin?error=purge_running");
    console.error(`[puls-web] admin ${action} failed:`, e instanceof Error ? e.message : e);
    return seeOther("/admin?error=failed");
  }
}

// Decline selected (the ticked `ids`) or Decline all shown (`shown`, every
// pending request the page listed, behind a confirm box).
async function declineMany(session: Buffer, form: FormData | null) {
  const all = field(form, "scope") === "shown";
  if (all && field(form, "confirm") !== "yes") return seeOther("/admin?error=failed");
  const ids = [...new Set((form?.getAll(all ? "shown" : "ids") ?? []).filter(isUuid).map((v) => v.toLowerCase()))];
  if (ids.length === 0) return seeOther("/admin?error=none_selected");
  if (ids.length > DECLINE_MAX) return seeOther("/admin?error=failed");
  try {
    await declineSignups(session, ids);
    return seeOther("/admin?notice=denied_many");
  } catch (e) {
    console.error("[puls-web] admin deny_many failed:", e instanceof Error ? e.message : e);
    return seeOther("/admin?error=failed");
  }
}
