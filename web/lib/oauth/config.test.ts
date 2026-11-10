import { afterEach, describe, expect, it, vi } from "vitest";

import { isOAuthPath, oauthConfig, readOAuthConfig } from "./config";
import { oauthKeyFixture } from "./keyFixture";

const keys = oauthKeyFixture();
const ON = {
  WEB_ACCOUNTS: "true",
  WEB_PUBLIC_URL: "https://viewer.example.com/",
  PULS_MCP_URL: "https://mcp.example.com/mcp",
  ...keys.env,
};

afterEach(() => vi.restoreAllMocks());

describe("readOAuthConfig", () => {
  it("loads a matching Ed25519 signer and public-only verifier configuration", () => {
    const setting = readOAuthConfig(ON);
    expect(setting.state).toBe("on");
    if (setting.state !== "on") return;
    expect(setting.config.issuer).toBe("https://viewer.example.com");
    expect(setting.config.resource).toBe("https://mcp.example.com/mcp");
    expect(setting.config.signingKey.id).toBe(keys.signingKey.id);
    expect(setting.config.signingKey.privateKey.type).toBe("private");
    expect([...setting.config.publicKeys.values()].every((key) => key.type === "public")).toBe(true);
    expect(Object.hasOwn(setting.config, "secret")).toBe(false);
  });

  it("rejects retired modes and stays off without OAuth keys or resources", () => {
    expect(readOAuthConfig({ ...ON, WEB_ACCOUNTS: "false" }).state).toBe("invalid");
    expect(readOAuthConfig({ WEB_ACCOUNTS: "true", WEB_PUBLIC_URL: ON.WEB_PUBLIC_URL }).state).toBe("off");
  });

  it("rejects retired shared secrets, partial keys, unknown ids and invalid URLs", () => {
    for (const env of [
      { ...ON, PULS_MCP_OAUTH_SECRET: "retired-test-secret" },
      { ...ON, PULS_AUTH_PRIVATE_KEY_FILE: undefined },
      { ...ON, PULS_AUTH_PUBLIC_KEYS_FILE: undefined },
      { ...ON, PULS_AUTH_KEY_ID: undefined },
      { ...ON, PULS_AUTH_KEY_ID: "unknown" },
      { ...ON, PULS_AUTH_PRIVATE_KEY_FILE: "synthetic-missing-key" },
      { ...ON, PULS_MCP_URL: undefined },
      { ...ON, WEB_PUBLIC_URL: undefined },
      { ...ON, PULS_MCP_URL: "http://mcp.example.com/mcp" },
      { ...ON, PULS_MCP_URL: "https://mcp.example.com/mcp?x=1" },
      { ...ON, PULS_MCP_URL: "not a url" },
      { ...ON, WEB_PUBLIC_URL: "http://viewer.example.com" },
      { ...ON, WEB_PUBLIC_URL: "https://viewer.example.com/app" },
      { ...ON, PULS_MCP_URL: "https://mcp.example.com" },
    ]) expect(readOAuthConfig(env).state).toBe("invalid");
    expect(readOAuthConfig({ ...ON, WEB_PUBLIC_URL: "http://localhost:3000", PULS_MCP_URL: "http://127.0.0.1:8090/mcp" }).state).toBe("on");
  });

  it("reports a fixed failure once, without exposing obsolete secret contents", () => {
    const error = vi.spyOn(console, "error").mockImplementation(() => {});
    const bad = { ...ON, PULS_MCP_OAUTH_SECRET: "synthetic-retired-secret-value" };
    expect(oauthConfig(bad)).toBeNull();
    expect(oauthConfig(bad)).toBeNull();
    expect(error).toHaveBeenCalledTimes(1);
    expect(String(error.mock.calls[0][0])).toContain("PULS_MCP_OAUTH_SECRET");
    expect(String(error.mock.calls[0][0])).not.toContain(bad.PULS_MCP_OAUTH_SECRET);
    expect(oauthConfig(ON)).not.toBeNull();
  });
});

describe("isOAuthPath", () => {
  it("names exactly the OAuth endpoints", () => {
    for (const path of [
      "/.well-known/oauth-authorization-server", "/oauth/register", "/oauth/authorize",
      "/oauth/authorize/decision", "/oauth/token", "/oauth/revoke",
    ]) expect(isOAuthPath(path)).toBe(true);
    for (const path of ["/oauth", "/oauth/", "/oauth/token/x", "/.well-known/openid-configuration", "/api/auth/assistants"]) {
      expect(isOAuthPath(path)).toBe(false);
    }
  });
});
