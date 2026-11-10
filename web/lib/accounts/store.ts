// The account rows behind accounts mode (schema auth, 015_web_accounts.sql).
// Account identity lives here and never in `users`, whose name and email are
// the phone's HealthKit profile. Server-only.

import { query, transaction } from "../db";
import { revokeAllGrants } from "../oauth/store";
import { createSession, SESSION_ABSOLUTE_DAYS, tokenHash, type SessionMeta } from "./session";

/**
 * The stored form of an email address: trimmed and lower-cased (the table's
 * CHECK insists), or null for something that is not plausibly one.
 */
export function normalizeEmail(raw: string | null | undefined): string | null {
  const email = (raw ?? "").trim().toLowerCase();
  if (email.length < 3 || email.length > 254) return null;
  if (!/^[^\s@]+@[^\s@]+$/.test(email)) return null;
  return email;
}

export interface LoginAccount {
  id: string;
  userId: string;
  passwordHash: string;
}

/** The account an email signs in, if it can sign in at all. */
export async function findAccountForLogin(email: string): Promise<LoginAccount | null> {
  const rows = await query<{ id: string; user_id: string; password_hash: string }>(
    `SELECT id::text, user_id::text, password_hash
       FROM auth.accounts
      WHERE email = $1 AND auth.personal_account_allowed(id)`,
    [email],
  );
  const row = rows[0];
  return row ? { id: row.id, userId: row.user_id, passwordHash: row.password_hash } : null;
}

export async function findPasswordHash(accountId: string): Promise<string | null> {
  const rows = await query<{ password_hash: string | null }>(
    "SELECT password_hash FROM auth.accounts WHERE id = $1 AND disabled_at IS NULL",
    [accountId],
  );
  return rows[0]?.password_hash ?? null;
}

/**
 * Finishes a password login only while the verified credentials are still
 * current. The account lock serializes this with password changes, invite
 * resets and disabling: they either invalidate this attempt, or revoke the
 * session it creates. A legacy rehash cannot restore a replaced password.
 */
export async function finishPasswordLogin(
  account: LoginAccount,
  replacementHash: string | null,
  meta: SessionMeta,
  previousSession: Buffer | null,
): Promise<string | null> {
  return transaction(async (q) => {
    const rows = await q(
      "SELECT 1 FROM auth.accounts WHERE id = $1 AND password_hash = $2 AND auth.personal_account_allowed(id) FOR UPDATE",
      [account.id, account.passwordHash],
    );
    if (!rows.length) return null;
    if (replacementHash) {
      await q("UPDATE auth.accounts SET password_hash = $2 WHERE id = $1", [account.id, replacementHash]);
    }
    if (previousSession) await q("DELETE FROM auth.sessions WHERE id = $1", [previousSession]);
    return createSession(account.id, meta, q);
  });
}

/**
 * A new password: stored, and every session of the account ended — the
 * caller's too, which it replaces with a fresh one, so no copy of any cookie
 * survives a password change. Every AI assistant connected to the account
 * (OAuth grant) is revoked with them.
 */
export async function changePassword(accountId: string, hash: string): Promise<void> {
  await transaction(async (q) => {
    await q("UPDATE auth.accounts SET password_hash = $2, password_changed_at = now() WHERE id = $1", [accountId, hash]);
    await q("DELETE FROM auth.sessions WHERE account_id = $1", [accountId]);
    await revokeAllGrants(q, accountId);
  });
}

/** Rechecks the authorizing password and session, changes it and signs in atomically. */
export async function changePasswordAndSession(
  accountId: string,
  verifiedHash: string,
  sessionId: Buffer,
  newHash: string,
  meta: SessionMeta,
): Promise<string | null> {
  return transaction(async (q) => {
    const account = await q(
      "SELECT 1 FROM auth.accounts WHERE id = $1 AND password_hash = $2 AND disabled_at IS NULL FOR UPDATE",
      [accountId, verifiedHash],
    );
    if (!account.length) return null;
    // Check after acquiring the account lock: a reset that preceded this
    // request may have revoked its session even when the password is equal.
    const session = await q(
      `SELECT 1 FROM auth.sessions WHERE id = $1 AND account_id = $2 AND purpose = 'personal'
         AND expires_at > now() AND created_at > now() - make_interval(days => $3)
       FOR UPDATE`,
      [sessionId, accountId, SESSION_ABSOLUTE_DAYS],
    );
    if (!session.length) return null;
    await q("UPDATE auth.accounts SET password_hash = $2, password_changed_at = now() WHERE id = $1", [accountId, newHash]);
    await q("DELETE FROM auth.sessions WHERE account_id = $1", [accountId]);
    await revokeAllGrants(q, accountId);
    return createSession(accountId, meta, q);
  });
}

export interface PendingInvite {
  email: string;
  expiresAt: number;
}

/** The unused, unexpired invite a link's token names, or null. */
export async function findInvite(token: string): Promise<PendingInvite | null> {
  const hash = tokenHash(token);
  if (!hash) return null;
  const rows = await query<{ email: string; expires_at: Date }>(
    `SELECT i.email, i.expires_at
       FROM auth.invites i
      WHERE i.token_hash = $1 AND i.accepted_at IS NULL AND i.expires_at > now()
        AND NOT EXISTS (SELECT 1 FROM auth.accounts a WHERE a.user_id = i.user_id)`,
    [hash],
  );
  const row = rows[0];
  return row ? { email: row.email, expiresAt: row.expires_at.getTime() } : null;
}

export type AcceptInviteResult = { ok: true; accountId: string; sessionToken: string } | { ok: false; reason: "invalid" | "email_taken" };

/**
 * Creates a new account and its first session in one transaction. Invitations
 * never replace an existing owner's credentials or undo a disabled account.
 * A link pressed twice is used once; the unique account key also closes the
 * race between different invitations for the same pending member.
 */
export async function acceptInvite(token: string, passwordHash: string, meta: SessionMeta, previousSession: Buffer | null): Promise<AcceptInviteResult> {
  const hash = tokenHash(token);
  if (!hash) return { ok: false, reason: "invalid" };
  return transaction(async (q) => {
    const invites = await q<{ id: string; user_id: string; email: string; is_admin: boolean }>(
      `SELECT id::text, user_id::text, email, is_admin
         FROM auth.invites
        WHERE token_hash = $1 AND accepted_at IS NULL AND expires_at > now()
        FOR UPDATE`,
      [hash],
    );
    const invite = invites[0];
    if (!invite) return { ok: false, reason: "invalid" } as const;

    const existing = await q("SELECT 1 FROM auth.accounts WHERE user_id = $1", [invite.user_id]);
    if (existing.length) return { ok: false, reason: "invalid" } as const;
    const taken = await q(
      "SELECT 1 FROM auth.accounts WHERE email = $1",
      [invite.email],
    );
    if (taken.length) return { ok: false, reason: "email_taken" } as const;

    const created = await q<{ id: string }>(
      `INSERT INTO auth.accounts (user_id, email, password_hash, is_admin, password_changed_at)
       VALUES ($1, $2, $3, $4, now()) ON CONFLICT DO NOTHING
       RETURNING id::text`,
      [invite.user_id, invite.email, passwordHash, invite.is_admin],
    );
    const account = created.at(0);
    if (!account) return { ok: false, reason: "invalid" } as const;
    const accountId = account.id;
    await q("UPDATE auth.invites SET accepted_at = now() WHERE id = $1", [invite.id]);
    await q(
      `UPDATE auth.invites SET expires_at = now()
        WHERE user_id = $1 AND accepted_at IS NULL AND id <> $2 AND expires_at > now()`,
      [invite.user_id, invite.id],
    );
    if (previousSession) await q("DELETE FROM auth.sessions WHERE id = $1", [previousSession]);
    // Its first session must commit with the account, before later revocations.
    const sessionToken = await createSession(accountId, meta, q);
    return { ok: true, accountId, sessionToken } as const;
  });
}
