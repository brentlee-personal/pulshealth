import type { NextRequest } from "next/server";
import { isUuid } from "@/lib/uuid";
import { query } from "@/lib/db";
import { healthResource, readOAuthConfig } from "@/lib/oauth/config";
import { verifyAccessToken } from "@/lib/oauth/jwt";
import { defaultReadWindow, readRequestAllowed, type ReadPermissions } from "@/lib/oauth/permissions";
import { createAPICredential } from "@/lib/oauth/apiCredential";
import { CATALOG } from "@/lib/catalog";

export const dynamic = "force-dynamic";
const routes: Record<string, readonly string[]> = {
  users: [],
  "catalog/types": [],
  "metrics/daily": ["types", "start", "end"],
  "activity/summary": ["start", "end"],
  "sleep/daily": ["start", "end"],
  workouts: ["limit", "start", "end"],
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
  if (!claims || !isUuid(claims.grant_id) || !claims.principal) return answer(401, { error: "invalid_token" });
  const principal = claims.principal;
  const permissions = { operations: principal.allowed_operations, record_types: principal.allowed_types, time_range: principal.allowed_time_range };
  let upstreamPermissions: ReadPermissions = permissions;
  const path = (await context.params).path.join("/");
  const allowed = Object.hasOwn(routes, path) ? routes[path] : undefined;
  if (!allowed) return answer(404, { error: "not_found" });
  const params = request.nextUrl.searchParams;
  for (const key of params.keys()) {
    if (key === "user") {
      return answer(403, { error: "account_selection_forbidden" });
    } else if (!allowed.includes(key) || params.getAll(key).length !== 1 || (params.get(key)?.length ?? 0) > 512) {
      return answer(400, { error: "invalid_query" });
    }
  }
  if (path === "workouts" && params.has("limit") && !/^(?:[1-9]|[1-4][0-9]|50)$/.test(params.get("limit")!)) {
    return answer(400, { error: "invalid_query" });
  }
  if (path === "workouts" && !params.has("start") && !params.has("end")) {
    const window = defaultReadWindow(permissions);
    if (!window) return answer(403, { error: "outside_grant" });
    params.set("start", String(window.start));
    params.set("end", String(window.end));
  }
  if (["metrics/daily", "activity/summary", "sleep/daily", "workouts"].includes(path)) {
    const start = params.get("start"), end = params.get("end");
    if (!start || !end || !/^\d{1,16}$/.test(start) || !/^\d{1,16}$/.test(end) ||
        !Number.isSafeInteger(Number(start)) || !Number.isSafeInteger(Number(end)) ||
        Number(end) <= Number(start) || Number(end) - Number(start) > 31 * 86400_000) {
      return answer(400, { error: "invalid_query" });
    }
    const types = path === "metrics/daily" ? (params.get("types") ?? "").split(",").map((s) => s.trim()) :
      [path === "activity/summary" ? "HKActivitySummaryTypeIdentifier" : path === "sleep/daily" ? "HKCategoryTypeIdentifierSleepAnalysis" : "HKWorkoutTypeIdentifier"];
    if (types.some((value) => !value) || new Set(types).size !== types.length || types.length > 50) return answer(400, { error: "invalid_query" });
    if (!readRequestAllowed(permissions, "metrics:read", types, Number(start), Number(end))) return answer(403, { error: "outside_grant" });
    upstreamPermissions = { operations: ["metrics:read"], record_types: types,
      time_range: { from: new Date(Number(start)).toISOString(), until: new Date(Number(end)).toISOString() } };
    if (path === "metrics/daily") params.set("types", types.join(","));
  }
  try {
    // A disabled/deleted account or revoked grant stops API reads immediately.
    const grants = await query<{ id: string; time_zone: string }>(
      `SELECT g.id::text, puls_user_time_zone(a.user_id) AS time_zone FROM auth.oauth_grants g JOIN auth.accounts a ON a.id = g.account_id
       WHERE a.user_id = $1 AND g.client_id = $2 AND g.resource = $3
         AND g.id = $4 AND g.revoked_at IS NULL AND g.refresh_expires_at > now()
         AND g.authorization_version = $5 AND g.permissions = $6::jsonb
         AND a.disabled_at IS NULL AND a.password_hash IS NOT NULL LIMIT 1`,
      [claims.sub, claims.client_id, healthResource(config), claims.grant_id, principal.authorization_version,
        JSON.stringify(permissions)],
    );
    if (!grants.length) return answer(401, { error: "invalid_token" });
    if (path === "users") return answer(200, { users: [{ userID: claims.sub, timeZone: grants[0].time_zone }], default: claims.sub, multiUser: false });
    if (path === "catalog/types") {
      const types = permissions.record_types.map((identifier) => {
        const type = CATALOG.find((entry) => entry.identifier === identifier);
        return type ? { identifier, kind: type.kind, unit: type.unit } : { identifier };
      });
      return answer(200, { types });
    }
    const base = process.env.PULS_API_URL;
    if (!base) return answer(503, { error: "unavailable" });
    const credential = await createAPICredential(config, { kind: "grant", subjectToken: authorization.slice(7), permissions: upstreamPermissions });
    if (!credential) return answer(401, { error: "invalid_token" });
    const upstream = new URL(`${base.replace(/\/+$/, "")}/v1/${path}`);
    upstream.search = params.toString();
    const response = await fetch(upstream, {
      headers: { Authorization: `Bearer ${credential.token}` }, redirect: "error", cache: "no-store",
      signal: AbortSignal.timeout(30_000),
    });
    if (!response.ok) return answer([400, 401, 403, 404].includes(response.status) ? response.status : 503, { error: "unavailable" });
    const body = await response.json();
    return answer(200, body);
  } catch {
    return answer(503, { error: "unavailable" });
  }
}
