// Password recovery is separate from invitation approval. Never logs tokens or addresses.
import { createHash } from "node:crypto";
import { query, transaction, type QueryFn } from "../db";
import { mailConfig, sendMail } from "../email";
import { revokeAllGrants } from "../oauth/store";
import { publicBase } from "./mail";
import { newToken, tokenHash } from "./session";

export function recoveryAvailable(): boolean {
  return Boolean(mailConfig() && publicBase());
}

async function charge(q: QueryFn, key: string, limit: number): Promise<boolean> {
  const rows = await q<{ attempts: number }>(
    `INSERT INTO auth.password_reset_limits (key, started_at, attempts) VALUES ($1, now(), 1)
     ON CONFLICT (key) DO UPDATE SET
       attempts = CASE WHEN auth.password_reset_limits.started_at <= now() - interval '1 hour' THEN 1
                       ELSE auth.password_reset_limits.attempts + 1 END,
       started_at = CASE WHEN auth.password_reset_limits.started_at <= now() - interval '1 hour' THEN now()
                         ELSE auth.password_reset_limits.started_at END
     RETURNING attempts`, [createHash("sha256").update(key).digest()],
  );
  return rows[0].attempts <= limit;
}

/** All requests, including unknown addresses, consume the same durable budgets. */
export async function requestPasswordReset(email: string, ip: string): Promise<{ email: string; token: string } | null> {
  return transaction(async (q) => {
    // Global-first serializes admission and bounds the table to ~1,001 rows/hour.
    if (!(await charge(q, "global", 500))) return null;
    await q("DELETE FROM auth.password_reset_limits WHERE started_at < now() - interval '1 hour'");
    await q("DELETE FROM auth.password_resets WHERE expires_at <= now()");
    const ipAllowed = await charge(q, `ip:${ip}`, 10);
    const emailAllowed = await charge(q, `email:${email}`, 3);
    if (!ipAllowed || !emailAllowed) return null;
    const accounts = await q<{ id: string; email: string; password_hash: string }>(
      `SELECT id::text, email, password_hash FROM auth.accounts
       WHERE email = $1 AND disabled_at IS NULL AND password_hash IS NOT NULL
       FOR UPDATE`, [email],
    );
    const account = accounts[0];
    if (!account) return null;
    const token = newToken();
    // Do not invalidate earlier emailed links: anyone can request a new one.
    await q(`INSERT INTO auth.password_resets (token_hash, account_id, password_hash, password_changed_at, expires_at)
             SELECT $1, id, password_hash, password_changed_at, now() + interval '30 minutes'
             FROM auth.accounts WHERE id = $2`, [tokenHash(token), account.id]);
    return { email: account.email, token };
  });
}

export async function sendPasswordReset(reset: { email: string; token: string }): Promise<boolean> {
  const config = mailConfig();
  const base = publicBase();
  if (!config || !base) return false;
  return sendMail({ to: reset.email, subject: "Reset your PulsHealth password", replyTo: config.admin ?? undefined,
    text: `Use this link to choose a new password:\n\n${base}/reset-password/${reset.token}\n\nThis link works once and expires in 30 minutes. Changing your password signs out all browsers and disconnects AI assistants (existing access tokens expire within 30 minutes). Your iPhone sync connection is unchanged.\n\nIf you did not ask for this, ignore this message. Your password has not changed.`,
  }, config);
}

export async function usablePasswordReset(token: string): Promise<boolean> {
  const hash = tokenHash(token);
  if (!hash) return false;
  return (await query(`SELECT 1 FROM auth.password_resets r JOIN auth.accounts a ON a.id = r.account_id
    WHERE r.token_hash = $1 AND r.expires_at > now() AND a.disabled_at IS NULL
      AND a.password_hash = r.password_hash AND a.password_changed_at IS NOT DISTINCT FROM r.password_changed_at
      `, [hash])).length > 0;
}

/** Account lock first, like login/password/OAuth; concurrent use has one winner. */
export async function completePasswordReset(token: string, passwordHash: string): Promise<boolean> {
  const hash = tokenHash(token);
  if (!hash) return false;
  return transaction(async (q) => {
    const accounts = await q<{ id: string }>(`SELECT a.id::text FROM auth.accounts a
      WHERE a.id = (SELECT account_id FROM auth.password_resets WHERE token_hash = $1)
        AND a.disabled_at IS NULL FOR UPDATE`, [hash]);
    if (!accounts[0]) return false;
    const accountId = accounts[0].id;
    const used = await q(`DELETE FROM auth.password_resets r USING auth.accounts a
      WHERE r.token_hash = $1 AND r.account_id = a.id AND r.expires_at > now()
        AND a.password_hash = r.password_hash AND a.password_changed_at IS NOT DISTINCT FROM r.password_changed_at
      RETURNING r.account_id`, [hash]);
    if (!used.length) return false;
    await q("UPDATE auth.accounts SET password_hash = $2, password_changed_at = clock_timestamp() WHERE id = $1", [accountId, passwordHash]);
    await q("DELETE FROM auth.password_resets WHERE account_id = $1", [accountId]);
    await q("DELETE FROM auth.sessions WHERE account_id = $1", [accountId]);
    await revokeAllGrants(q, accountId);
    return true;
  });
}
