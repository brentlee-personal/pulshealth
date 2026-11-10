export async function register() {
  if (process.env.NEXT_RUNTIME !== "nodejs") return;
  const { parseFlag, viewerMode } = await import("./lib/mode");
  viewerMode();
  const { readSecret } = await import("./scripts/secrets.mjs");
  readSecret("DATABASE_URL");
  const trustProxy = parseFlag("TRUST_PROXY_HEADERS", process.env.TRUST_PROXY_HEADERS);
  const { clientIpHeaderWarning } = await import("./lib/accounts/request");
  const warning = clientIpHeaderWarning(trustProxy);
  if (warning) console.warn(`[puls-web] ${warning}`);
  if (process.env.NODE_ENV === "production" && !trustProxy) {
    console.warn("[puls-web] Personal accounts require HTTPS through the configured trusted proxy.");
  }
  const { readOAuthConfig } = await import("./lib/oauth/config");
  const setting = readOAuthConfig();
  if (setting.state === "invalid") throw new Error(setting.problem);
  console.log("[puls-web] Personal accounts are required. Health checks do not expose health records.");
}
