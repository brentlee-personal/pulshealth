// Personal accounts are the only viewer identity.

export type ViewerMode = "accounts";

/** The environment, or a stand-in for it in tests. */
export type Env = Record<string, string | undefined>;

/** An environment flag that is on: true/1/yes/on, any case. */
export function isTrue(value: string | undefined): boolean {
  return /^(true|1|yes|on)$/i.test((value ?? "").trim());
}

export function viewerMode(env: Env = process.env): ViewerMode {
  for (const name of ["WEB_AUTH_PASSWORD", "WEB_DEMO_USER", "PULS_USER_ID", "PULS_MULTI_USER", "PULS_API_TOKEN"]) {
    if (env[name]?.trim()) throw new Error(`${name} is retired; use personal accounts`);
  }
  if (!parseFlag("WEB_ACCOUNTS", env.WEB_ACCOUNTS, true)) {
    throw new Error("WEB_ACCOUNTS=false is retired; personal accounts are required");
  }
  return "accounts";
}

/**
 * Whether strangers may ask for an account (/signup). Only in accounts mode,
 * and off unless WEB_SIGNUPS is on: a request creates nothing until an
 * administrator approves it on /admin.
 */
export function signupsOpen(env: Env = process.env): boolean {
  return viewerMode(env) === "accounts" && isTrue(env.WEB_SIGNUPS);
}

/**
 * A boolean setting the way ingest, the product API and the MCP server read
 * one (parseBoolEnv in each): true/1/yes/on or false/0/no/off, any case,
 * surrounding space ignored; empty or unset is `fallback`. Anything else
 * throws, naming the variable — a typo must not quietly mean "off" here and
 * "on" in a Go service.
 */
export function parseFlag(name: string, value: string | undefined, fallback = false): boolean {
  const v = (value ?? "").trim().toLowerCase();
  if (v === "") return fallback;
  if (v === "true" || v === "1" || v === "yes" || v === "on") return true;
  if (v === "false" || v === "0" || v === "no" || v === "off") return false;
  throw new Error(
    `${name} must be true or false (also accepted: 1/0, yes/no, on/off), got ${JSON.stringify((value ?? "").trim())}`,
  );
}

/**
 * Whether X-Forwarded-* and CF-Connecting-IP may be believed. Same switch,
 * same spelling rule (parseFlag), same default and same caveat as ingest and
 * the product API: only when a trusted proxy is the ONLY way to reach the
 * port (the client IP is then the last entry of the header, the one that
 * proxy appended). An unparseable value stops the server at startup
 * (instrumentation.ts); should one reach a request anyway, it means "do not
 * trust", the safe reading.
 */
export function trustProxyHeaders(env: Env = process.env): boolean {
  try {
    return parseFlag("TRUST_PROXY_HEADERS", env.TRUST_PROXY_HEADERS);
  } catch {
    return false;
  }
}
