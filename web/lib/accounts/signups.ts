// Sign-up requests, the administrator's decisions, and a person's own phone
// and account (016_web_signups.sql). Everything that writes beyond schema
// auth goes through the SECURITY DEFINER functions there, which act only on
// self-service users (those an approved sign-up made) and take the caller's
// session as auth.sessions stores it — its SHA-256, `Session.id` — so the
// plaintext cookie never reaches the database or its logs. Server-only.

import { createHash, randomBytes } from "node:crypto";
import { isIP } from "node:net";

import { longStatement, query, transaction } from "../db";
import { newToken, tokenHash } from "./session";
import { writeDeletionLedger } from "./deletionLedger";

/** How long an approval's invite link works. */
export const APPROVAL_INVITE_DAYS = 7;

export interface SignupInput {
  email: string;
  name: string;
  note: string;
  ip: string | null;
  userAgent: string | null;
}

/**
 * The waitlist's bounds. Undecided requests are a waitlist now and are kept
 * until an administrator decides (020_waitlist_retention.sql), so nothing
 * drains a flood by itself; instead at most SIGNUP_DAILY_CAP requests are
 * stored in any 24 hours, and none once SIGNUP_PENDING_CAP wait. Past
 * either, a new one is dropped (the form still answers "on the waitlist",
 * so it reveals nothing). A flood then takes weeks to fill the list, cannot
 * keep mailing the operator (that has its own daily cap, mail.ts), and
 * /admin can decline it in bulk; real people are not turned away for being
 * on a long list. Checked here, not by a constraint: a burst of concurrent
 * requests can pass either by a few.
 */
export const SIGNUP_PENDING_CAP = 10_000;
export const SIGNUP_DAILY_CAP = 500;

/** How many waiting requests /admin lists at once, oldest first. */
export const SIGNUP_LIST_LIMIT = 500;

/**
 * What became of a request: `created`; `exists`, already pending for this
 * address; or `full`, dropped at SIGNUP_DAILY_CAP or SIGNUP_PENDING_CAP.
 * Only `created` stored anything, and the requester is told the same in all
 * three cases.
 */
export type SignupOutcome = "created" | "exists" | "full";

/**
 * Records a request. Asking again while one is pending changes nothing and
 * is not an error (the person sees the same confirmation either way, so the
 * form does not reveal who has asked).
 */
export async function createSignupRequest(input: SignupInput): Promise<SignupOutcome> {
  const [row] = await query<{ pending: string; today: string; created: string }>(
    `WITH waiting AS (
       SELECT count(*) FILTER (WHERE status = 'pending') AS n,
              count(*) FILTER (WHERE created_at > now() - interval '1 day') AS today
         FROM auth.signup_requests
     ), ins AS (
       INSERT INTO auth.signup_requests (email, name, note, ip, user_agent)
       SELECT $1::text, $2::text, $3::text, $4::inet, $5::text FROM waiting
        WHERE waiting.n < $6::bigint AND waiting.today < $7::bigint
       ON CONFLICT (email) WHERE status = 'pending' DO NOTHING
       RETURNING 1
     )
     SELECT (SELECT n FROM waiting) AS pending, (SELECT today FROM waiting) AS today,
            (SELECT count(*) FROM ins) AS created`,
    [
      input.email,
      input.name.slice(0, 200),
      input.note.slice(0, 2000),
      input.ip && isIP(input.ip) ? input.ip : null,
      input.userAgent?.slice(0, 300) ?? null,
      SIGNUP_PENDING_CAP,
      SIGNUP_DAILY_CAP,
    ],
  );
  if (Number(row?.created) > 0) return "created";
  return Number(row?.pending) >= SIGNUP_PENDING_CAP || Number(row?.today) >= SIGNUP_DAILY_CAP ? "full" : "exists";
}

/** How many requests wait for a decision (all of them, not only those /admin lists). */
export async function pendingSignupCount(): Promise<number> {
  const [row] = await query<{ n: string }>("SELECT count(*) AS n FROM auth.signup_requests WHERE status = 'pending'");
  return Number(row?.n ?? 0);
}

/** How many requests were stored in the last 24 hours (SIGNUP_DAILY_CAP counts these). */
export async function signupsToday(): Promise<number> {
  const [row] = await query<{ n: string }>("SELECT count(*) AS n FROM auth.signup_requests WHERE created_at > now() - interval '1 day'");
  return Number(row?.n ?? 0);
}

/** Whether an account already signs in with this address. */
export async function accountExists(email: string): Promise<boolean> {
  const rows = await query("SELECT 1 FROM auth.accounts WHERE email = $1", [email]);
  return rows.length > 0;
}

export interface SignupRequest {
  id: string;
  email: string;
  name: string;
  note: string;
  status: "pending" | "approved" | "denied";
  createdAt: number;
  decidedAt: number | null;
  ip: string | null;
  userAgent: string | null;
}

export async function listSignupRequests(): Promise<SignupRequest[]> {
  // The waitlist, oldest first (SIGNUP_LIST_LIMIT of it): pending requests
  // are kept until decided (020_waitlist_retention.sql). Then the 200 latest
  // decisions: the database deletes an approved request 30 days after the
  // decision (auth.prune_signups, hourly), and the filter only keeps the page
  // from showing one in the hour before it goes. Declined ones were deleted
  // when declined.
  const rows = await query<{
    id: string;
    email: string;
    name: string;
    note: string;
    status: SignupRequest["status"];
    created_at: Date;
    decided_at: Date | null;
    ip: string | null;
    user_agent: string | null;
  }>(
    `(SELECT id::text, email, name, note, status, created_at, decided_at, host(ip) AS ip, user_agent
        FROM auth.signup_requests
       WHERE status = 'pending'
       ORDER BY created_at
       LIMIT $1)
     UNION ALL
     (SELECT id::text, email, name, note, status, created_at, decided_at, host(ip) AS ip, user_agent
        FROM auth.signup_requests
       WHERE status <> 'pending' AND decided_at > now() - interval '30 days'
       ORDER BY decided_at DESC
       LIMIT 200)`,
    [SIGNUP_LIST_LIMIT],
  );
  return rows.map((r) => ({
    id: r.id,
    email: r.email,
    name: r.name,
    note: r.note,
    status: r.status,
    createdAt: r.created_at.getTime(),
    decidedAt: r.decided_at?.getTime() ?? null,
    ip: r.ip,
    userAgent: r.user_agent,
  }));
}

export interface Approval {
  userId: string;
  email: string;
  name: string;
  /** The invite link's token, plaintext, for the approval email. */
  inviteToken: string;
}

/**
 * Approves a request: the database creates the person's user (checking the
 * administrator's session itself), and an invite for them goes in the same
 * transaction, so an approval never exists without its invite.
 */
export async function approveSignup(session: Buffer, adminAccountId: string, requestId: string): Promise<Approval> {
  const inviteToken = newToken();
  return transaction(async (q) => {
    const [{ user_id: userId }] = await q<{ user_id: string }>(
      "SELECT auth.approve_signup($1, $2::uuid)::text AS user_id",
      [session, requestId],
    );
    const [request] = await q<{ email: string; name: string }>(
      "SELECT email, name FROM auth.signup_requests WHERE id = $1::uuid",
      [requestId],
    );
    await q(
      `INSERT INTO auth.invites (token_hash, user_id, email, created_by, expires_at)
       VALUES ($1, $2::uuid, $3, $4::uuid, now() + make_interval(days => $5))`,
      [tokenHash(inviteToken), userId, request.email, adminAccountId, APPROVAL_INVITE_DAYS],
    );
    return { userId, email: request.email, name: request.name, inviteToken };
  });
}

/** At most this many requests are declined in one go (auth.decline_signups refuses more). */
export const DECLINE_MAX = 1000;

/**
 * Declines pending requests by deleting them outright; how many went. No
 * email is sent. The database checks the administrator's session
 * (auth.decline_signups, 018_web_accounts_hardening.sql) and leaves a
 * decided request alone.
 */
export async function declineSignups(session: Buffer, requestIds: string[]): Promise<number> {
  if (requestIds.length === 0) return 0;
  const [row] = await query<{ n: number }>("SELECT auth.decline_signups($1, $2::uuid[]) AS n", [session, requestIds]);
  return Number(row?.n ?? 0);
}

export interface AdminAccount {
  id: string;
  userId: string;
  email: string;
  isAdmin: boolean;
  selfService: boolean;
  createdAt: number;
  disabledAt: number | null;
  deletionRequestedAt: number | null;
  lastSeenAt: number | null;
}

export async function listAccounts(): Promise<AdminAccount[]> {
  const rows = await query<{
    id: string;
    user_id: string;
    email: string;
    is_admin: boolean;
    self_service: boolean;
    created_at: Date;
    disabled_at: Date | null;
    deletion_requested_at: Date | null;
    last_seen_at: Date | null;
  }>(
    `SELECT a.id::text, a.user_id::text, a.email, a.is_admin,
            EXISTS (SELECT 1 FROM auth.self_service_users ss WHERE ss.user_id = a.user_id) AS self_service,
            a.created_at, a.disabled_at, a.deletion_requested_at, max(s.last_seen_at) AS last_seen_at
       FROM auth.accounts a
       LEFT JOIN auth.sessions s ON s.account_id = a.id
      GROUP BY a.id
      ORDER BY a.deletion_requested_at IS NOT NULL DESC, a.created_at`,
  );
  return rows.map((r) => ({
    id: r.id,
    userId: r.user_id,
    email: r.email,
    isAdmin: r.is_admin,
    selfService: r.self_service,
    createdAt: r.created_at.getTime(),
    disabledAt: r.disabled_at?.getTime() ?? null,
    deletionRequestedAt: r.deletion_requested_at?.getTime() ?? null,
    lastSeenAt: r.last_seen_at?.getTime() ?? null,
  }));
}

/**
 * Approved people who never used their invite: a self-service user and no
 * account. The address comes from their latest invite, which every approval
 * writes, so the list outlives the request (pruned 30 days after the
 * decision); the database removes the user itself once no invite has been
 * sent for 30 days (auth.prune_signups).
 */
export interface UnusedApproval {
  userId: string;
  email: string;
  name: string;
  invitedAt: number;
}

export async function listUnusedApprovals(): Promise<UnusedApproval[]> {
  const rows = await query<{ user_id: string; email: string; name: string; invited_at: Date }>(
    `SELECT ss.user_id::text, i.email, coalesce(r.name, '') AS name, i.created_at AS invited_at
       FROM auth.self_service_users ss
       CROSS JOIN LATERAL (SELECT email, created_at FROM auth.invites
                            WHERE user_id = ss.user_id ORDER BY created_at DESC LIMIT 1) i
       LEFT JOIN LATERAL (SELECT name FROM auth.signup_requests
                           WHERE user_id = ss.user_id ORDER BY decided_at DESC LIMIT 1) r ON true
      WHERE NOT EXISTS (SELECT 1 FROM auth.accounts a WHERE a.user_id = ss.user_id)
      ORDER BY i.created_at DESC
      LIMIT 200`,
  );
  return rows.map((r) => ({ userId: r.user_id, email: r.email, name: r.name, invitedAt: r.invited_at.getTime() }));
}

/** A resend creates a new member; the earlier member's data remains sealed. */
export async function reinvite(session: Buffer, userId: string): Promise<Approval | null> {
  const inviteToken = newToken();
  const [row] = await query<{ user_id: string; email: string; name: string }>(
    "SELECT * FROM auth.reinvite_new_member($1,$2::uuid,$3,$4)",
    [session, userId, tokenHash(inviteToken), APPROVAL_INVITE_DAYS * 24],
  );
  return row ? { userId: row.user_id, email: row.email, name: row.name, inviteToken } : null;
}

/**
 * Why /admin may not disable or enable this account, or null when it may:
 * `own` for the administrator's own account, `admin` for any administrator's
 * (they are managed from the server). The database refuses both too
 * (auth.set_account_disabled); this is the clear answer before it does.
 */
export async function disableRefusal(adminAccountId: string, accountId: string): Promise<"own" | "admin" | null> {
  if (accountId.toLowerCase() === adminAccountId.toLowerCase()) return "own";
  const rows = await query<{ is_admin: boolean }>("SELECT is_admin FROM auth.accounts WHERE id = $1::uuid", [accountId]);
  return rows[0]?.is_admin ? "admin" : null;
}

export async function setAccountDisabled(session: Buffer, accountId: string, disabled: boolean): Promise<void> {
  await query("SELECT auth.set_account_disabled($1, $2::uuid, $3)", [session, accountId, disabled]);
}

/** How long a purge may run: it unpacks the compressed batches the user's rows share. */
export const PURGE_MINUTES = 30;

/** Deletes everything stored for the user; rows removed per table. */
export async function purgeUser(session: Buffer, userId: string): Promise<Record<string, number>> {
  const [row] = await longStatement<{ counts: Record<string, number> }>(
    "SELECT auth.purge_user($1, $2::uuid) AS counts",
    [session, userId],
    PURGE_MINUTES,
    () => writeDeletionLedger(userId),
  );
  return row.counts;
}

// ── the signed-in person's own phone and account ────────────────────────────

export interface DeviceToken {
  id: string;
  /** The plaintext, for the pairing code. Shown once; never stored. */
  token: string;
}

/**
 * Mints a sync token for the signed-in person's own user: 32 random bytes,
 * hex — the shape `ingest devices issue` makes — of which the database keeps
 * only the SHA-256 over the hex text, as ingest looks it up.
 */
export async function issueDeviceToken(session: Buffer, name: string): Promise<DeviceToken> {
  const token = randomBytes(32).toString("hex");
  const hash = createHash("sha256").update(token, "ascii").digest();
  const [row] = await query<{ id: string }>(
    "SELECT auth.issue_device_token($1, $2, $3, $4)::text AS id",
    [session, hash, token.slice(0, 8), name.slice(0, 100)],
  );
  return { id: row.id, token };
}

export interface MyDevice {
  id: string;
  name: string;
  prefix: string;
  active: boolean;
  createdAt: number;
  lastSeenAt: number | null;
}

export async function myDevices(session: Buffer): Promise<MyDevice[]> {
  const rows = await query<{
    id: string;
    name: string;
    token_prefix: string;
    status: string;
    created_at: Date;
    last_seen_at: Date | null;
  }>("SELECT id::text, name, token_prefix, status, created_at, last_seen_at FROM auth.my_devices($1)", [session]);
  return rows.map((r) => ({
    id: r.id,
    name: r.name,
    prefix: r.token_prefix,
    active: r.status === "active",
    createdAt: r.created_at.getTime(),
    lastSeenAt: r.last_seen_at?.getTime() ?? null,
  }));
}

export async function revokeMyDevice(session: Buffer, id: string): Promise<boolean> {
  if (!/^\d{1,18}$/.test(id)) return false;
  const [row] = await query<{ revoked: boolean }>("SELECT auth.revoke_my_device($1, $2::bigint) AS revoked", [session, id]);
  return row.revoked;
}

/** Disables the caller's own account and revokes its tokens; the user id, for the operator's notice. */
export async function deleteMyAccount(session: Buffer): Promise<string> {
  const [row] = await query<{ user_id: string }>("SELECT auth.delete_my_account($1)::text AS user_id", [session]);
  return row.user_id;
}

/** The pairing code for the app: the same payload ingest prints (pairing.go). */
export function pairingLink(serverUrl: string, token: string, userId: string): string {
  const escape = (s: string) =>
    Array.from(new TextEncoder().encode(s))
      .map((b) => {
        const c = String.fromCharCode(b);
        return /[A-Za-z0-9\-._~]/.test(c) ? c : `%${b.toString(16).toUpperCase().padStart(2, "0")}`;
      })
      .join("");
  return `puls://pair?url=${escape(serverUrl)}&token=${escape(token)}&user=${escape(userId)}`;
}
