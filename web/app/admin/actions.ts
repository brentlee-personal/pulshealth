"use server";

import { headers } from "next/headers";

import { currentAdmin } from "@/lib/accounts/admin";
import { publicBase, sendApproval } from "@/lib/accounts/mail";
import { publicOrigin } from "@/lib/accounts/request";
import { APPROVAL_INVITE_DAYS, approveSignup, reinvite, type Approval } from "@/lib/accounts/signups";
import { trustProxyHeaders } from "@/lib/mode";
import { isUuid } from "@/lib/uuid";

export type ApproveState =
  | null
  | { ok: true; email: string; emailed: boolean; inviteUrl?: string }
  | { ok: false; error: string };

// Approve a request, or send a new invite to someone approved who never used
// theirs. An administrator's session is required here and checked again by
// the database. The person is emailed; when that cannot happen, the invite
// link comes back to the administrator, once, to pass on themselves — never
// in a URL.
export async function approveRequest(_prev: ApproveState, form: FormData): Promise<ApproveState> {
  const admin = await currentAdmin();
  if (!admin) return { ok: false, error: "Only an administrator can approve requests." };
  const id = String(form.get("id") ?? "");
  if (!isUuid(id)) return { ok: false, error: "No such request." };
  try {
    const approval =
      form.get("mode") === "reinvite"
        ? await reinvite(admin.id, id)
        : await approveSignup(admin.id, admin.accountId, id);
    if (!approval) return { ok: false, error: "That person already has an account." };
    return await deliver(approval);
  } catch (e) {
    const code = (e as { code?: string }).code;
    if (code === "23505") return { ok: false, error: "An account already signs in with that address." };
    if (code === "P0002") return { ok: false, error: "That request was already decided." };
    console.error("[puls-web] approval failed:", e instanceof Error ? e.message : e);
    return { ok: false, error: "Approval failed; see the server log." };
  }
}

async function deliver(approval: Approval): Promise<ApproveState> {
  const emailed = await sendApproval({ ...approval, days: APPROVAL_INVITE_DAYS });
  return emailed
    ? { ok: true, email: approval.email, emailed }
    : { ok: true, email: approval.email, emailed, inviteUrl: `${await viewerOrigin()}/invite/${approval.inviteToken}` };
}

// WEB_PUBLIC_URL, else the origin this request came in on.
async function viewerOrigin(): Promise<string> {
  const configured = publicBase();
  if (configured) return configured;
  const h = await headers();
  return publicOrigin(new URL(`http://${h.get("host") ?? "localhost"}`), h, trustProxyHeaders(), undefined);
}
