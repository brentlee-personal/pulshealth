// Signed-in sessions for accounts mode.
//
// The cookie carries 32 random bytes (base64url); auth.sessions stores only
// their SHA-256, so the table signs nobody in. The cookie is
// `__Host-`-prefixed: the browser accepts it only with Secure, Path=/ and no
// Domain, so no other host or path can plant or shadow it. HttpOnly keeps it
// from scripts; SameSite=Lax keeps it off cross-site POSTs (proxy.ts also
// checks Origin). Sessions last 30 days from last use: an active one slides
// forward at most once an hour — and 90 days from sign-in at most, so a
// stolen cookie that is kept warm still dies.
//
// Server-only (node:crypto, the database).

import { createHash, randomBytes } from "node:crypto";
import { isIP } from "node:net";
import { query, type QueryFn } from "../db";

export const SESSION_COOKIE = "__Host-puls-session";
export const SESSION_DAYS = 30;
/** Absolute lifetime from sign-in; sliding never extends a session past it. */
export const SESSION_ABSOLUTE_DAYS = 90;
const SESSION_MAX_AGE_SECONDS = SESSION_DAYS * 86_400;
const TOKEN_BYTES = 32;
// 32 bytes in unpadded base64url.
const TOKEN_SHAPE = /^[A-Za-z0-9_-]{43}$/;

export function newToken(): string {
  return randomBytes(TOKEN_BYTES).toString("base64url");
}

/**
 * The stored form of a session or invite token, or null for a value that is
 * not one (so junk never reaches the database).
 */
export function tokenHash(token: string | null | undefined): Buffer | null {
  if (!token || !TOKEN_SHAPE.test(token)) return null;
  return createHash("sha256").update(Buffer.from(token, "base64url")).digest();
}

export interface CookieAttributes {
  httpOnly: true;
  secure: true;
  sameSite: "lax";
  path: "/";
  maxAge: number;
}

/** Attributes for a session cookie that lives `maxAge` seconds (0 clears it). */
export function sessionCookieAttributes(maxAge = SESSION_MAX_AGE_SECONDS): CookieAttributes {
  return { httpOnly: true, secure: true, sameSite: "lax", path: "/", maxAge };
}

export interface Session {
  /** auth.sessions.id: the SHA-256 of the cookie's token. */
  id: Buffer;
  accountId: string;
  userId: string;
  email: string;
  isAdmin: boolean;
  /** Made by an approved sign-up: may delete itself here. */
  selfService: boolean;
  /** Database-authorized erasure: approved sign-up or operator-marked personal account. */
  canDelete?: boolean;
  /** True when this lookup slid the expiry forward (re-send the cookie). */
  refreshed: boolean;
}

/**
 * The live session a cookie names, or null: unknown, expired (sliding or
 * absolute), or its account disabled or without a password. `touch` slides
 * an active session's expiry (proxy.ts does, once an hour at most); pages
 * only read.
 */
export async function findSession(token: string | null | undefined, touch = false): Promise<Session | null> {
  const id = tokenHash(token);
  if (!id) return null;
  const rows = await query<{
    account_id: string;
    user_id: string;
    email: string;
    is_admin: boolean;
    self_service: boolean;
    can_delete: boolean;
    stale: boolean;
  }>(
    `SELECT s.account_id::text, a.user_id::text, a.email, a.is_admin,
            EXISTS (SELECT 1 FROM auth.self_service_users ss WHERE ss.user_id = a.user_id) AS self_service,
            auth.can_delete_account(s.id) AS can_delete,
            s.last_seen_at < now() - interval '1 hour' AS stale
       FROM auth.sessions s
       JOIN auth.accounts a ON a.id = s.account_id
      WHERE s.id = $1
        AND s.purpose = 'personal'
        AND s.expires_at > now()
        AND s.created_at > now() - make_interval(days => $2)
        AND a.disabled_at IS NULL
        AND a.password_hash IS NOT NULL`,
    [id, SESSION_ABSOLUTE_DAYS],
  );
  const row = rows[0];
  if (!row) return null;
  let refreshed = false;
  if (touch && row.stale) {
    const slid = await query(
      `UPDATE auth.sessions
          SET last_seen_at = now(), expires_at = now() + make_interval(days => $2)
        WHERE id = $1 AND purpose = 'personal' AND expires_at > now()
          AND last_seen_at < now() - interval '1 hour'
      RETURNING 1`,
      [id, SESSION_DAYS],
    );
    refreshed = slid.length > 0;
  }
  return {
    id,
    accountId: row.account_id,
    userId: row.user_id,
    email: row.email,
    isAdmin: row.is_admin,
    selfService: row.self_service,
    canDelete: row.can_delete,
    refreshed,
  };
}

/** What the account page shows about a browser, beside its id. */
export interface SessionMeta {
  userAgent: string | null;
  ip: string | null;
}

/** Starts a session for `accountId`; returns the cookie's token. */
export async function createSession(accountId: string, meta: SessionMeta, q: QueryFn = query): Promise<string> {
  const token = newToken();
  await q(
    `INSERT INTO auth.sessions (id, account_id, expires_at, user_agent, ip, purpose)
     VALUES ($1, $2, now() + make_interval(days => $3), $4, $5, 'personal')`,
    [tokenHash(token), accountId, SESSION_DAYS, meta.userAgent?.slice(0, 300) ?? null, meta.ip && isIP(meta.ip) ? meta.ip : null],
  );
  // Expired rows (sliding or absolute) are deleted by the database's hourly
  // job (auth.prune_signups, 018_web_accounts_hardening.sql), not here: a
  // sweep on every sign-in scanned the whole table on the request path.
  return token;
}

/**
 * Whether `ip` is an address one of the account's live sessions signed in
 * from — an address its owner has proved recently. Sign-in uses it to spare
 * the owner the per-email failure bucket (lib/accounts/ratelimit.ts,
 * `failureKeys`). False for anything that is not an IP address (a client
 * without trusted proxy headers is "direct"), so the shared bucket of
 * unidentified clients never counts as known.
 */
export async function signedInFrom(email: string, ip: string): Promise<boolean> {
  if (!isIP(ip)) return false;
  const rows = await query(
    `SELECT 1
       FROM auth.sessions s
       JOIN auth.accounts a ON a.id = s.account_id
      WHERE a.email = $1 AND s.ip = $2::inet AND s.purpose = 'personal'
        AND s.expires_at > now()
        AND s.created_at > now() - make_interval(days => $3)
      LIMIT 1`,
    [email, ip, SESSION_ABSOLUTE_DAYS],
  );
  return rows.length > 0;
}

export async function deleteSession(id: Buffer): Promise<void> {
  await query("DELETE FROM auth.sessions WHERE id = $1", [id]);
}

export interface SessionSummary {
  /** Hex of the stored hash: names a row without being a credential. */
  id: string;
  createdAt: number;
  lastSeenAt: number;
  userAgent: string | null;
  ip: string | null;
  current: boolean;
}

export async function listSessions(accountId: string, currentId: Buffer): Promise<SessionSummary[]> {
  const rows = await query<{
    id: Buffer;
    created_at: Date;
    last_seen_at: Date;
    user_agent: string | null;
    ip: string | null;
  }>(
    `SELECT id, created_at, last_seen_at, user_agent, host(ip) AS ip
       FROM auth.sessions
      WHERE account_id = $1 AND purpose = 'personal' AND expires_at > now()
        AND created_at > now() - make_interval(days => $2)
      ORDER BY last_seen_at DESC`,
    [accountId, SESSION_ABSOLUTE_DAYS],
  );
  return rows.map((r) => ({
    id: r.id.toString("hex"),
    createdAt: r.created_at.getTime(),
    lastSeenAt: r.last_seen_at.getTime(),
    userAgent: r.user_agent,
    ip: r.ip,
    current: r.id.equals(currentId),
  }));
}

/** Signs out one of the account's own sessions, by the id the account page lists. */
export async function deleteAccountSession(accountId: string, idHex: string): Promise<void> {
  if (!/^[0-9a-f]{64}$/.test(idHex)) return;
  await query("DELETE FROM auth.sessions WHERE account_id = $1 AND id = $2", [accountId, Buffer.from(idHex, "hex")]);
}

/** Signs out every session of the account except `keep`. */
export async function deleteOtherSessions(accountId: string, keep: Buffer): Promise<void> {
  await query("DELETE FROM auth.sessions WHERE account_id = $1 AND id <> $2", [accountId, keep]);
}
