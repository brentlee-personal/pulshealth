import { createHash } from "node:crypto";
import { NextResponse, type NextRequest } from "next/server";
import { accountsOnly } from "@/lib/accounts/http";
import { findSession, deleteSession, createSession, tokenHash, type Session } from "@/lib/accounts/session";
import { query, scoped, transaction } from "@/lib/db";
import { normalizeEmail, findPasswordHash } from "@/lib/accounts/store";
import { verifyPassword } from "@/lib/accounts/password";
import { readBodyCapped } from "@/lib/oauth/http";
import { authFailures, takeAll, refundAll } from "@/lib/accounts/ratelimit";
export const dynamic = "force-dynamic";
const answer = (status: number, body: unknown) => NextResponse.json(body, { status, headers: { "Cache-Control": "no-store" } });
async function authenticate(request: NextRequest): Promise<Session | null> {
  const authorization = request.headers.get("authorization") ?? "";
  // Cookies are deliberately ignored: this machine endpoint has no CSRF exemption for cookies.
  const token = /^Bearer ([A-Za-z0-9_-]{43})$/.exec(authorization)?.[1];
  return token ? findSession(token, true) : null;
}
async function profile(session: Session) {
  const rows = await scoped(session.userId, q => q<{ name: string | null; dateOfBirth: number | null; biologicalSex: string | null }>(
    `SELECT name, (extract(epoch FROM dob::timestamp AT TIME ZONE 'UTC') * 1000)::float8 AS "dateOfBirth",
            biological_sex AS "biologicalSex" FROM users WHERE id = $1`, [session.userId]));
  return { userID: session.userId, email: session.email, name: rows[0]?.name ?? null,
    dateOfBirth: rows[0]?.dateOfBirth ?? null, biologicalSex: rows[0]?.biologicalSex ?? null };
}
async function handle(request: NextRequest, method: "GET" | "PUT" | "DELETE") {
  const off = accountsOnly(); if (off) return off;
  try {
    const session = await authenticate(request);
    if (!session) return answer(401, { error: "Sign in to your personal account again." });
    if (method === "DELETE") { await deleteSession(session.id); return answer(200, {}); }
    if (method === "GET") return answer(200, await profile(session));
    const raw = await readBodyCapped(request);
    if (raw === null) return answer(413, { error: "Profile request is too large." });
    const body = JSON.parse(raw);
    if (!body || typeof body !== "object" || Array.isArray(body)) return answer(400, { error: "Check your profile details." });
    const email = typeof body.email === "string" ? normalizeEmail(body.email) : null;
    const name = body.name === null ? null : typeof body.name === "string" ? body.name.trim() : undefined;
    const dob = body.dateOfBirth;
    const sex = body.biologicalSex;
    if (!email || name === undefined || (name?.length ?? 0) > 200 ||
        (dob !== null && (typeof dob !== "number" || !Number.isFinite(dob) || dob < -2208988800000 || dob > Date.now())) ||
        (sex !== null && !["female", "male", "other"].includes(sex))) return answer(400, { error: "Check your profile details." });
    let passwordHash: string | null = null;
    if (email !== session.email) {
      const keys = [`native-email:${session.accountId}`];
      if (!takeAll(authFailures, keys).allowed) return answer(429, { error: "Wait before trying your password again." });
      passwordHash = await findPasswordHash(session.accountId);
      if (!passwordHash || typeof body.currentPassword !== "string" || !(await verifyPassword(body.currentPassword, passwordHash)))
        return answer(403, { error: "Enter your current password to change your sign-in email." });
      refundAll(authFailures, keys);
    }
    await query("SELECT auth.set_my_profile($1, $2, $3, $4, $5, $6)",
      [session.id, name, email, dob === null ? null : new Date(dob).toISOString().slice(0, 10), sex, passwordHash]);
    return answer(200, await profile({ ...session, email }));
  } catch (error) {
    if (error instanceof SyntaxError || error instanceof TypeError) return answer(400, { error: "Check your profile details." });
    const code = (error as { code?: string }).code;
    if (code === "23505") return answer(409, { error: "That email cannot be used. Choose another address." });
    if (code === "42501") return answer(403, { error: "Sign in again before saving your profile." });
    return answer(503, { error: "Your account could not be reached. Try again." });
  }
}
export const GET = (request: NextRequest) => handle(request, "GET");
export const PUT = (request: NextRequest) => handle(request, "PUT");
export const DELETE = (request: NextRequest) => handle(request, "DELETE");

/** Exchange a one-use code only with the verifier held by the app that started sign-in. */
export async function POST(request: NextRequest) {
  const off = accountsOnly(); if (off) return off;
  try {
    const raw = await readBodyCapped(request);
    if (raw === null) return answer(413, { error: "Sign-in request is too large." });
    const { code, verifier } = JSON.parse(raw) ?? {};
    if (typeof code !== "string" || !tokenHash(code) || typeof verifier !== "string" || !/^[A-Za-z0-9_-]{43,128}$/.test(verifier))
      return answer(400, { error: "Sign-in could not be completed." });
    const challenge = createHash("sha256").update(verifier).digest("base64url");
    const token = await transaction(async q => {
      // Password changes/resets lock this same account before deleting sessions.
      // Lock first, then recheck the source session in a fresh statement.
      const locked = await q(
        `SELECT a.id FROM auth.native_handoffs h JOIN auth.accounts a ON a.id = h.account_id
          WHERE h.code_hash = $1 AND h.challenge = $2 AND h.expires_at > now()
          FOR UPDATE OF a`, [tokenHash(code), challenge]);
      if (!locked.length) return null;
      const rows = await q<{ account_id: string }>(
        `DELETE FROM auth.native_handoffs h USING auth.accounts a, auth.sessions s
          WHERE h.code_hash = $1 AND h.challenge = $2 AND h.expires_at > now()
            AND a.id = h.account_id AND a.disabled_at IS NULL AND a.password_hash IS NOT NULL
            AND s.id = h.source_session AND s.account_id = a.id
            AND s.purpose = 'personal' AND s.expires_at > now() AND s.created_at > now() - interval '90 days'
          RETURNING h.account_id::text`, [tokenHash(code), challenge]);
      return rows[0] ? createSession(rows[0].account_id, { userAgent: "PulsHealth iPhone account", ip: null }, q) : null;
    });
    if (!token) return answer(401, { error: "Sign-in expired. Try again." });
    return answer(200, { token });
  } catch { return answer(503, { error: "Sign-in could not be completed. Try again." }); }
}
