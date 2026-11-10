import type { Metadata } from "next";
import { headers } from "next/headers";
import { notFound } from "next/navigation";

import { AuthCard } from "@/components/AuthCard";
import { errorMessage, param } from "@/lib/accounts/messages";
import { PASSWORD_MIN_LENGTH } from "@/lib/accounts/password";
import { authFailures, checkAll, failureKeys } from "@/lib/accounts/ratelimit";
import { clientIp } from "@/lib/accounts/request";
import { findInvite, type PendingInvite } from "@/lib/accounts/store";
import { trustProxyHeaders, viewerMode } from "@/lib/mode";

// An invite link (accounts mode only): `make web-invite` prints
// https://<host>/invite/<token>. The page shows who the invite is for and
// asks for a password; the form posts to /api/auth/invite, which is where a
// bad token is charged to the client's address. This page only checks the
// throttle and never charges it: any site can make a browser GET it (an
// <img> will do), and that must not spend the visitor's sign-in attempts.
// The token is in the path, so the page's Referrer-Policy (same-origin, set
// in next.config.ts) matters: no other site is ever sent this URL.
export const dynamic = "force-dynamic";
export const metadata: Metadata = { title: "Accept invite — PulsHealth" };

type Params = Promise<{ token: string }>;
type Search = Promise<Record<string, string | string[] | undefined>>;

export default async function InvitePage({ params, searchParams }: { params: Params; searchParams: Search }) {
  if (viewerMode() !== "accounts") notFound();
  const { token } = await params;
  const search = await searchParams;

  const keys = failureKeys(clientIp(await headers(), trustProxyHeaders()));
  if (!checkAll(authFailures, keys).allowed) {
    return <AuthCard title="Too many attempts" subtitle="Wait a minute, then open the link again." />;
  }

  let invite: PendingInvite | null;
  try {
    invite = await findInvite(token);
  } catch {
    return <AuthCard title="Something went wrong" error={errorMessage("unavailable")} />;
  }
  if (!invite) {
    return (
      <AuthCard
        title="This invitation cannot be used"
        subtitle="Invitations create new accounts. Existing members can sign in or use password recovery."
      />
    );
  }

  return (
    <AuthCard
      title="Create your account"
      subtitle="You have been invited to view your own health records here."
      error={errorMessage(param(search.error), { invalid: "This invite link has expired or was already used." })}
    >
      <form method="post" action="/api/auth/invite">
        <input type="hidden" name="token" value={token} />
        <div className="form-field">
          <label htmlFor="email">Email</label>
          <input id="email" type="email" value={invite.email} autoComplete="username" readOnly />
        </div>
        <div className="form-field">
          <label htmlFor="password">New password</label>
          <input
            id="password"
            name="password"
            type="password"
            autoComplete="new-password"
            minLength={PASSWORD_MIN_LENGTH}
            required
            autoFocus
          />
          <span className="form-hint">At least {PASSWORD_MIN_LENGTH} characters. A passphrase is fine.</span>
        </div>
        <div className="form-field">
          <label htmlFor="confirm">Repeat the password</label>
          <input id="confirm" name="confirm" type="password" autoComplete="new-password" minLength={PASSWORD_MIN_LENGTH} required />
        </div>
        <button type="submit" className="btn btn-primary btn-block" style={{ marginTop: 6 }}>
          Create account
        </button>
      </form>
    </AuthCard>
  );
}
