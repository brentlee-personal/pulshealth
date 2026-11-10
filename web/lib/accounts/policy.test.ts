import { describe, expect, it } from "vitest";

import { classifyPath, decideAccounts, unauthenticatedAnswer } from "./policy";

describe("classifyPath", () => {
  it("allows exactly the signed Apple machine endpoints over HTTPS without cookies or Origin", () => {
    for (const env of ["production", "sandbox"]) {
      const pathname = `/api/app-store/notifications/${env}`;
      expect(decideAccounts({ pathname, secure: true, sameOrigin: false, development: false })).toBe("pass");
      expect(decideAccounts({ pathname, secure: false, sameOrigin: false, development: false })).toBe("insecure");
    }
    expect(classifyPath("/api/app-store/notifications/production/extra")).toBe("protected");
    expect(classifyPath("/api/app-store/notifications/unknown")).toBe("protected");
  });
  it("sorts every route class", () => {
    expect(classifyPath("/api/healthz")).toBe("health");
    expect(classifyPath("/_next/static/chunks/app.js")).toBe("asset");
    expect(classifyPath("/icon.svg")).toBe("asset");
    expect(classifyPath("/login")).toBe("public");
    expect(classifyPath("/invite/abc")).toBe("public");
    expect(classifyPath("/api/auth/login")).toBe("public");
    expect(classifyPath("/api/auth/invite")).toBe("public");
    expect(classifyPath("/api/auth/logout")).toBe("public");
    expect(classifyPath("/signup")).toBe("public");
    expect(classifyPath("/api/auth/signup")).toBe("public");
    expect(classifyPath("/demo")).toBe("protected");
    for (const path of [
      "/",
      "/workouts",
      "/type/HKQuantityTypeIdentifierStepCount",
      "/account",
      "/settings",
      "/api/user",
      "/api/auth/password",
      "/api/auth/sessions",
      "/api/auth/devices",
      "/api/auth/delete-account",
      "/api/admin",
      "/admin",
      "/login/extra",
      "/demo/x",
      "/demo/",
      "/invite/abc/def",
      "/invite/",
      "/_next/image",
      "/__nextjs_original-stack-frame",
      "/api/healthz/x",
    ]) {
      expect(classifyPath(path), path).toBe("protected");
    }
    expect(classifyPath("/__nextjs_original-stack-frame", true)).toBe("asset");
  });

  it("puts the OAuth machine endpoints in their own class, and the consent page behind a session", () => {
    for (const path of ["/.well-known/oauth-authorization-server", "/oauth/register", "/oauth/token", "/oauth/revoke"]) {
      expect(classifyPath(path), path).toBe("oauth");
    }
    for (const path of ["/oauth/authorize", "/oauth/authorize/decision", "/oauth", "/oauth/token/x", "/.well-known/openid-configuration", "/api/auth/assistants"]) {
      expect(classifyPath(path), path).toBe("protected");
    }
  });
});

describe("decideAccounts", () => {
  const facts = (pathname: string, over: Partial<{ secure: boolean; sameOrigin: boolean }> = {}) => ({
    pathname,
    secure: true,
    sameOrigin: true,
    development: false,
    ...over,
  });

  it("answers the health check and assets whatever the transport", () => {
    expect(decideAccounts(facts("/api/healthz", { secure: false, sameOrigin: false }))).toBe("pass");
    expect(decideAccounts(facts("/_next/static/x.js", { secure: false }))).toBe("pass");
  });

  it("sends plain HTTP that came through the trusted proxy to https", () => {
    expect(decideAccounts({ ...facts("/login", { secure: false }), forwardedHttp: true })).toBe("upgrade");
    expect(decideAccounts({ ...facts("/workouts", { secure: false }), forwardedHttp: true })).toBe("upgrade");
  });

  it("refuses plain HTTP everywhere else, the sign-in page included", () => {
    expect(decideAccounts(facts("/login", { secure: false }))).toBe("insecure");
    expect(decideAccounts(facts("/", { secure: false }))).toBe("insecure");
  });

  it("refuses cross-origin writes before anything else", () => {
    expect(decideAccounts(facts("/api/auth/login", { sameOrigin: false }))).toBe("cross-origin");
    expect(decideAccounts(facts("/api/auth/logout", { sameOrigin: false }))).toBe("cross-origin");
  });

  it("serves public routes and asks for a session everywhere else", () => {
    expect(decideAccounts(facts("/login"))).toBe("pass");
    expect(decideAccounts(facts("/invite/tok"))).toBe("pass");
    expect(decideAccounts(facts("/"))).toBe("session");
    expect(decideAccounts(facts("/api/auth/password"))).toBe("session");
  });

  it("passes OAuth machine endpoints over HTTPS whatever their Origin, and refuses them over HTTP", () => {
    expect(decideAccounts(facts("/oauth/token", { sameOrigin: false }))).toBe("pass");
    expect(decideAccounts(facts("/oauth/register", { sameOrigin: false }))).toBe("pass");
    expect(decideAccounts(facts("/oauth/token", { secure: false }))).toBe("insecure");
    expect(decideAccounts({ ...facts("/oauth/token", { secure: false }), forwardedHttp: true })).toBe("upgrade");
    expect(decideAccounts(facts("/oauth/authorize"))).toBe("session");
    expect(decideAccounts(facts("/oauth/authorize", { sameOrigin: false }))).toBe("cross-origin");
  });

  it("redirects page loads to sign in and answers the rest 401", () => {
    expect(unauthenticatedAnswer("GET", "/workouts")).toBe("redirect");
    expect(unauthenticatedAnswer("HEAD", "/")).toBe("redirect");
    expect(unauthenticatedAnswer("POST", "/account")).toBe("unauthorized");
    expect(unauthenticatedAnswer("GET", "/api/user")).toBe("unauthorized");
  });
});
