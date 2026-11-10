// OAuth signing configuration. Only the authorization module holds the private file.
// Invalid or partial settings fail closed; startup rejects them.

import { type KeyObject } from "node:crypto";
import { type Env, viewerMode } from "../mode";
import { readPublicKeys, readSigningKey, type SigningKey } from "./keys";

export interface OAuthConfig {
  /** WEB_PUBLIC_URL without a trailing slash: the `iss` of every token. */
  issuer: string;
  /** PULS_MCP_URL exactly: the `aud` of every token. */
  resource: string;
  signingKey: SigningKey;
  publicKeys: ReadonlyMap<string, KeyObject>;
}

export type OAuthSetting =
  | { state: "on"; config: OAuthConfig }
  | { state: "off" }
  | { state: "invalid"; problem: string };

const LOOPBACK = new Set(["localhost", "127.0.0.1", "[::1]"]);

/** An https URL, or http on a loopback host (development); null if neither. */
function webUrl(raw: string): URL | null {
  try {
    const url = new URL(raw);
    if (url.hash || url.username || url.password) return null;
    if (url.protocol === "https:") return url;
    if (url.protocol === "http:" && LOOPBACK.has(url.hostname)) return url;
    return null;
  } catch {
    return null;
  }
}

export function readOAuthConfig(env: Env = process.env): OAuthSetting {
  if (env.PULS_MCP_OAUTH_SECRET) return { state: "invalid", problem: "PULS_MCP_OAUTH_SECRET is retired; use separate Ed25519 key files" };
  try { viewerMode(env); } catch { return { state: "invalid", problem: "Personal accounts are required; remove retired access settings" }; }
  const privateFile = env.PULS_AUTH_PRIVATE_KEY_FILE ?? "";
  const publicFile = env.PULS_AUTH_PUBLIC_KEYS_FILE ?? "";
  const keyId = env.PULS_AUTH_KEY_ID ?? "";
  const resource = (env.PULS_MCP_URL ?? "").trim();
  const publicUrl = (env.WEB_PUBLIC_URL ?? "").trim();
  if (!privateFile && !publicFile && !keyId && !resource) return { state: "off" };
  if (!privateFile || !publicFile || !keyId || !resource || !publicUrl) {
    return {
      state: "invalid",
      problem: "OAuth needs PULS_AUTH_PRIVATE_KEY_FILE, PULS_AUTH_PUBLIC_KEYS_FILE, PULS_AUTH_KEY_ID, PULS_MCP_URL and WEB_PUBLIC_URL",
    };
  }
  const resourceUrl = webUrl(resource);
  if (!resourceUrl || resourceUrl.search || resourceUrl.pathname === "/") {
    return { state: "invalid", problem: "PULS_MCP_URL must be the MCP server's https URL, path included (https://mcp.example.com/mcp)" };
  }
  const issuerUrl = webUrl(publicUrl);
  if (!issuerUrl || issuerUrl.search || issuerUrl.pathname !== "/") {
    return { state: "invalid", problem: "WEB_PUBLIC_URL must be the viewer's https URL to issue OAuth tokens" };
  }
  try {
    const publicKeys = readPublicKeys(publicFile);
    const signingKey = readSigningKey(privateFile, keyId, publicKeys);
    return { state: "on", config: { issuer: publicUrl.replace(/\/+$/, ""), resource, signingKey, publicKeys } };
  } catch (error) {
    return { state: "invalid", problem: error instanceof Error ? error.message : "OAuth key files are unavailable" };
  }
}

// The problem logged last, so a broken setting is said once, not per request.
const globalForOAuth = globalThis as typeof globalThis & { __pulsOAuthProblem?: string };

/** The settings when OAuth is on; null (and one logged error) otherwise. */
export function oauthConfig(env: Env = process.env): OAuthConfig | null {
  const setting = readOAuthConfig(env);
  if (setting.state === "on") return setting.config;
  if (setting.state === "invalid" && globalForOAuth.__pulsOAuthProblem !== setting.problem) {
    globalForOAuth.__pulsOAuthProblem = setting.problem;
    console.error(`[puls-web] ${setting.problem}; OAuth authorization is unavailable.`);
  }
  return null;
}

/** The OAuth endpoints machines call: no session, no Origin check, CORS *. */
export const OAUTH_MACHINE_PATHS = [
  "/.well-known/oauth-authorization-server",
  "/oauth/register",
  "/oauth/token",
  "/oauth/revoke",
] as const;

/** The consent page and the route its form reaches (proxy.ts rewrites POSTs to it). */
export const OAUTH_AUTHORIZE_PATH = "/oauth/authorize";
export const OAUTH_DECISION_PATH = "/oauth/authorize/decision";

/** Every path that exists only while OAuth is on (a 404 otherwise). */
export function isOAuthPath(pathname: string): boolean {
  return (
    (OAUTH_MACHINE_PATHS as readonly string[]).includes(pathname) ||
    pathname.startsWith("/api/health/") ||
    pathname === OAUTH_AUTHORIZE_PATH ||
    pathname === OAUTH_DECISION_PATH
  );
}

/** The one scope there is: read everything on the signed-in account. */
export const OAUTH_SCOPE = "health:read";
/** Access-token lifetime: what a revocation can lag by. */
export const ACCESS_TOKEN_SECONDS = 1800;
/** A refresh token dies this long after its grant was last used. */
export const REFRESH_TOKEN_DAYS = 60;
/** An authorization code works once, within this. */
export const CODE_SECONDS = 300;

/** Separate audience for the public, account-scoped health API. */
export function healthResource(config: OAuthConfig): string {
  return `${config.issuer}/api/health`;
}

export function oauthResources(config: OAuthConfig): string[] {
  return [config.resource, healthResource(config)];
}
