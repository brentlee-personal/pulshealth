import { describe, expect, it } from "vitest";
import { isTrue, parseFlag, signupsOpen, trustProxyHeaders, viewerMode } from "./mode";

describe("personal viewer mode", () => {
  it("requires personal accounts without a mode flag", () => {
    expect(viewerMode({})).toBe("accounts");
    expect(viewerMode({ WEB_ACCOUNTS: "true" })).toBe("accounts");
    expect(() => viewerMode({ WEB_ACCOUNTS: "false" })).toThrow(/personal accounts are required/);
  });

  it("refuses each shared identity setting without echoing its value", () => {
    for (const name of ["WEB_AUTH_PASSWORD", "WEB_DEMO_USER", "PULS_USER_ID", "PULS_MULTI_USER", "PULS_API_TOKEN"]) {
      expect(() => viewerMode({ [name]: "synthetic-private-setting" })).toThrow(`${name} is retired`);
      try {
        viewerMode({ [name]: "synthetic-private-setting" });
      } catch (error) {
        expect(String(error)).not.toContain("synthetic-private-setting");
      }
    }
  });

  it("reads flags the way the Go services do", () => {
    for (const on of ["true", "TRUE", "1", "yes", "on", " true "]) expect(isTrue(on), on).toBe(true);
    for (const off of [undefined, "", "false", "0", "no", "off", "truthy"]) expect(isTrue(off), String(off)).toBe(false);
    expect(trustProxyHeaders({ TRUST_PROXY_HEADERS: "true" })).toBe(true);
    expect(trustProxyHeaders({})).toBe(false);
  });

  it("parses TRUST_PROXY_HEADERS exactly as ingest, the API and the MCP server do", () => {
    for (const on of ["true", "TRUE", "True", "1", "yes", "YES", "on", "On", " true "]) {
      expect(parseFlag("X", on), on).toBe(true);
      expect(trustProxyHeaders({ TRUST_PROXY_HEADERS: on }), on).toBe(true);
    }
    for (const off of ["false", "FALSE", "0", "no", "off", " Off "]) expect(parseFlag("X", off), off).toBe(false);
    // Empty or unset is the default (false for this one).
    for (const empty of [undefined, "", "  "]) {
      expect(parseFlag("X", empty), String(empty)).toBe(false);
      expect(parseFlag("X", empty, true), String(empty)).toBe(true);
    }
    // Anything else is an error naming the variable, never a quiet "off".
    for (const bad of ["truthy", "t", "f", "y", "2", "enabled", "tru e"]) {
      expect(() => parseFlag("TRUST_PROXY_HEADERS", bad), bad).toThrow(/^TRUST_PROXY_HEADERS must be true or false/);
    }
    expect(() => parseFlag("TRUST_PROXY_HEADERS", " ture ")).toThrow('got "ture"');
    // At request time (startup already refused it) an unparseable value is
    // the safe reading: do not trust.
    expect(trustProxyHeaders({ TRUST_PROXY_HEADERS: "ture" })).toBe(false);
  });

  it("opens account requests only when explicitly enabled", () => {
    expect(signupsOpen({})).toBe(false);
    expect(signupsOpen({ WEB_SIGNUPS: "true" })).toBe(true);
    expect(signupsOpen({ WEB_SIGNUPS: "false" })).toBe(false);
    expect(() => signupsOpen({ WEB_AUTH_PASSWORD: "shared" })).toThrow(/retired/);
  });
});
