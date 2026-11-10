// Personal reads never substitute generated health values.

import { healthQuery } from "../db";
import { viewerMode } from "../mode";
import type { DataSourceInfo } from "../types";
import { DataUnavailableError } from "./unavailable";


// ── data source detection (short TTL cache) ──────────────────────────────
let srcCache: { info: DataSourceInfo; at: number } | null = null;
let srcInFlight: Promise<DataSourceInfo> | null = null;
const SRC_TTL = 30_000;
// Recover promptly after a transient outage instead of caching it for 30 s.
const ERROR_TTL = 1000;

export async function getDataSource(): Promise<DataSourceInfo> {
  if (!process.env.DATABASE_URL_FILE) {
    return { source: "error", detail: "Personal storage is not configured" };
  }
  if (srcCache && Date.now() - srcCache.at < (srcCache.info.source === "live" ? SRC_TTL : ERROR_TTL)) return srcCache.info;
  if (srcInFlight) return srcInFlight;
  srcInFlight = checkDataSource();
  try {
    const info = await srcInFlight;
    srcCache = { info, at: Date.now() };
    return info;
  } finally {
    srcInFlight = null;
  }
}

async function checkDataSource(): Promise<DataSourceInfo> {
  let info: DataSourceInfo;
  try {
    viewerMode();
    const rows = await healthQuery<{ role: string; direct: boolean }>(
      `SELECT current_user AS role, has_any_column_privilege('public.quantity_samples', 'SELECT')
           OR has_any_column_privilege('public.users', 'SELECT') AS direct`,
    );
    if (rows[0]?.role !== "web_app" || rows[0].direct) {
      warnOnce("accounts-role", "[queries] Personal reads require the restricted web_app role.");
      return { source: "error", detail: "Personal storage is unavailable" };
    }
    info = { source: "live", detail: "Connected to TimescaleDB" };
  } catch {
    info = { source: "error", detail: "Database unreachable" };
    console.error("[queries] database unreachable");
  }
  return info;
}

// A live read failed. Besides logging it, drop the cached check so the next
// getDataSource() probes again instead of reporting "live" for up to SRC_TTL:
// a lost database fails that probe too and becomes the
// "error" source (the sidebar's "Database unavailable", /api/healthz's 503).
// The probe has a reserved connection, so chart-pool saturation stays local.
// A failure confined to one query (a statement timeout on one person's All
// Time chart) passes the probe, so it does not blank the viewer for
// everyone — only the page that hit it, through liveRead's throw.
export function readFailed(what: string): void {
  console.error(`[queries] ${what} failed`);
  srcCache = null;
}

/** A failed or unconfigured read stays unavailable, never synthetic or empty. */
export async function liveRead<T>(what: string, read: () => Promise<T>): Promise<T> {
  const info = await getDataSource();
  if (info.source !== "live") throw new DataUnavailableError(info.detail);
  try {
    return await read();
  } catch (e) {
    if (e instanceof DataUnavailableError) throw e;
    readFailed(what);
    throw new DataUnavailableError("A read failed; the database may be unreachable or the query timed out");
  }
}

// Log a given warning once per process; the zone checks run on every request
// and would otherwise flood the log with the same line.
const warnedOnce = new Set<string>();
export function warnOnce(key: string, ...args: unknown[]): void {
  if (warnedOnce.has(key)) return;
  warnedOnce.add(key);
  console.warn(...args);
}
