import type { NextRequest } from "next/server";
import { accountsOnly, field, readForm, requestSession, seeOther } from "@/lib/accounts/http";
import { newToken, tokenHash } from "@/lib/accounts/session";
import { query } from "@/lib/db";
export const dynamic = "force-dynamic";
export async function POST(request: NextRequest) {
  const off = accountsOnly(); if (off) return off;
  const session = await requestSession(request);
  if (!session) return seeOther("/login?next=%2Faccount");
  const form = await readForm(request);
  const state = field(form, "state");
  const challenge = field(form, "challenge");
  if (!/^[a-f0-9]{64}$/.test(state) || !/^[A-Za-z0-9_-]{43}$/.test(challenge)) return new Response(null, { status: 400 });
  const code = newToken();
  await query("DELETE FROM auth.native_handoffs WHERE expires_at <= now()");
  await query("INSERT INTO auth.native_handoffs (code_hash, challenge, account_id, source_session) VALUES ($1, $2, $3, $4)", [tokenHash(code), challenge, session.accountId, session.id]);
  const callback = new URL("puls://account");
  callback.searchParams.set("state", state);
  callback.searchParams.set("code", code);
  return seeOther(callback.toString());
}
