import type { NextRequest } from "next/server";
import { isUuid } from "@/lib/uuid";
import { query } from "@/lib/db";
import { healthResource, readOAuthConfig } from "@/lib/oauth/config";
import { verifyAccessToken } from "@/lib/oauth/jwt";

export const dynamic = "force-dynamic";
const routes: Record<string, readonly string[]> = {
  users: [],
  "catalog/types": [],
  "metrics/daily": ["types", "start", "end"],
  "activity/summary": ["start", "end"],
  "sleep/daily": ["start", "end"],
  workouts: ["limit"],
};
const answer = (status: number, body: unknown) => Response.json(body, {
  status, headers: { "Cache-Control": "no-store" },
});

/** Public bearer-only API. The account comes exclusively from the verified token. */
export async function GET(request: NextRequest, context: { params: Promise<{ path: string[] }> }) {
  const setting = readOAuthConfig();
  if (setting.state === "off") return answer(404, { error: "not_found" });
  if (setting.state === "invalid") return answer(503, { error: "authorization_unavailable" });
  const config = setting.config;
  const authorization = request.headers.get("authorization") ?? "";
  const claims = authorization.startsWith("Bearer ")
    ? verifyAccessToken(authorization.slice(7), { ...config, resource: healthResource(config) }) : null;
  if (!claims || !isUuid(claims.grant_id)) return answer(401, { error: "invalid_token" });
  const path = (await context.params).path.join("/");
  const allowed = Object.hasOwn(routes, path) ? routes[path] : undefined;
  if (!allowed) return answer(404, { error: "not_found" });
  const params = request.nextUrl.searchParams;
  for (const key of params.keys()) {
    if (key === "user") {
      // Compatible with clients that pin their returned account; never allow switching.
      if (params.getAll(key).length !== 1 || params.get(key) !== claims.sub) return answer(403, { error: "account_mismatch" });
    } else if (!allowed.includes(key) || params.getAll(key).length !== 1 || (params.get(key)?.length ?? 0) > 512) {
      return answer(400, { error: "invalid_query" });
    }
  }
  if (path === "workouts" && params.has("limit") && !/^(?:[1-9]|[1-4][0-9]|50)$/.test(params.get("limit")!)) {
    return answer(400, { error: "invalid_query" });
  }
  if (["metrics/daily", "activity/summary", "sleep/daily"].includes(path)) {
    const start = params.get("start"), end = params.get("end");
    if (!start || !end || !/^\d{1,16}$/.test(start) || !/^\d{1,16}$/.test(end) ||
        !Number.isSafeInteger(Number(start)) || !Number.isSafeInteger(Number(end)) ||
        Number(end) <= Number(start) || Number(end) - Number(start) > 31 * 86400_000) {
      return answer(400, { error: "invalid_query" });
    }
  }
  try {
    // A disabled/deleted account or revoked grant stops API reads immediately.
    const grants = await query<{ id: string }>(
      `SELECT g.id::text FROM auth.oauth_grants g JOIN auth.accounts a ON a.id = g.account_id
       WHERE a.user_id = $1 AND g.client_id = $2 AND g.resource = $3
         AND g.id = $4 AND g.revoked_at IS NULL AND g.refresh_expires_at > now()
         AND a.disabled_at IS NULL AND a.password_hash IS NOT NULL LIMIT 1`,
      [claims.sub, claims.client_id, healthResource(config), claims.grant_id],
    );
    if (!grants.length) return answer(401, { error: "invalid_token" });
    const base = process.env.PULS_API_URL;
    const token = process.env.PULS_API_TOKEN;
    if (!base || !token) return answer(503, { error: "unavailable" });
    const upstream = new URL(`${base.replace(/\/+$/, "")}/v1/${path}`);
    upstream.search = params.toString();
    upstream.searchParams.set("user", claims.sub);
    const response = await fetch(upstream, {
      headers: { Authorization: `Bearer ${token}` }, redirect: "error", cache: "no-store",
      signal: AbortSignal.timeout(30_000),
    });
    if (!response.ok) return answer(response.status === 404 ? 404 : 503, { error: "unavailable" });
    const body = await response.json();
    if (path === "users") {
      const user = body.users?.find((u: { userID?: string }) => u.userID === claims.sub);
      if (!user) return answer(401, { error: "invalid_token" });
      return answer(200, { users: [user], default: claims.sub, multiUser: false });
    }
    return answer(200, body);
  } catch {
    return answer(503, { error: "unavailable" });
  }
}
