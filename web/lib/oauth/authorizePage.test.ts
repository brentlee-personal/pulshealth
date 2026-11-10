import { oauthKeyFixture } from "./keyFixture";
// The consent page (app/oauth/authorize/page.tsx), rendered to HTML with the
// session and the client store mocked: an invalid request from a registered
// client is an error page with a link back, never a redirect (RFC 9700
// §4.11.2 — anyone can register a client, so redirecting would make this an
// open redirector), and a valid one is the consent form.

import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

const oauthKeys = oauthKeyFixture();
const store = vi.hoisted(() => ({ findClient: vi.fn() }));
const viewer = vi.hoisted(() => ({ currentSession: vi.fn() }));
const navigation = vi.hoisted(() => ({
  redirect: vi.fn((to: string) => {
    throw new Error(`redirect:${to}`);
  }),
  notFound: vi.fn(() => {
    throw new Error("notFound");
  }),
}));
vi.mock("@/lib/oauth/store", () => store);
vi.mock("@/lib/viewer", () => viewer);
vi.mock("next/navigation", () => navigation);

const ISSUER = "https://viewer.example.com";
const MCP = "https://mcp.example.com/mcp";
const CLIENT_ID = "pc_AAAAAAAAAAAAAAAAAAAAAAAA";
const REDIRECT = "https://claude.ai/api/mcp/auth_callback";
const CLIENT = { id: CLIENT_ID, name: "Claude", redirectUris: [REDIRECT], grantTypes: ["authorization_code", "refresh_token"] };
const QUERY = {
  client_id: CLIENT_ID,
  redirect_uri: REDIRECT,
  response_type: "code",
  code_challenge: "p_j-Nj_Xa-C0MOoCl6lv9UzGgSvHUvKaouTlEnyXOiE",
  code_challenge_method: "S256",
  scope: "health:read",
  state: "st",
};

const saved = { ...process.env };
beforeAll(() => {
  process.env.WEB_ACCOUNTS = "true";
  process.env.WEB_PUBLIC_URL = ISSUER;
  process.env.PULS_MCP_URL = MCP;
  delete process.env.PULS_MCP_OAUTH_SECRET;
  Object.assign(process.env, oauthKeys.env);
});
afterAll(() => {
  process.env = saved;
});
beforeEach(() => {
  store.findClient.mockReset().mockResolvedValue(CLIENT);
  viewer.currentSession.mockReset().mockResolvedValue({ email: "a@example.com" });
  navigation.redirect.mockClear();
});

async function render(query: Record<string, string>): Promise<string> {
  const { default: AuthorizePage } = await import("@/app/oauth/authorize/page");
  const element = await AuthorizePage({ searchParams: Promise.resolve(query) });
  return renderToStaticMarkup(createElement(() => element));
}

const links = (html: string) => [...html.matchAll(/<a [^>]*href="([^"]*)"/g)].map((m) => m[1].replace(/&amp;/g, "&"));

describe("the consent page", () => {
  it("shows an invalid request's error with a link back, and does not redirect", async () => {
    const html = await render({ ...QUERY, response_type: "bogus" });
    expect(navigation.redirect).not.toHaveBeenCalled();
    expect(html).toContain("only response_type=code is supported");
    expect(html).toContain("Return to");
    expect(html).toContain("claude.ai");
    expect(html).not.toContain("<form");
    const back = links(html).find((href) => href.startsWith(REDIRECT));
    expect(back).toBeDefined();
    expect(Object.fromEntries(new URL(back!).searchParams)).toEqual({
      error: "unsupported_response_type",
      error_description: "only response_type=code is supported",
      state: "st",
      iss: ISSUER,
    });
  });

  it("shows an unknown client or redirect URI with no link back at all", async () => {
    const html = await render({ ...QUERY, redirect_uri: "https://evil.example/cb" });
    expect(navigation.redirect).not.toHaveBeenCalled();
    expect(links(html).some((href) => href.includes("evil.example"))).toBe(false);
  });

  it("shows the consent form for a valid request", async () => {
    const html = await render(QUERY);
    expect(navigation.redirect).not.toHaveBeenCalled();
    expect(html).toContain("Connect Claude?");
    expect(html).toContain('action="/oauth/authorize"');
    expect(html).toContain('name="record_type"');
    expect(html).toContain('name="from"');
    expect(html).toContain('name="until"');
    expect(html).toMatch(/name="operation"[^>]*checked=""[^>]*value="metrics:read"/);
    expect(html).not.toMatch(/name="operation"[^>]*checked=""[^>]*value="samples:read"/);
    expect(html).not.toContain("read all health data");
  });

});
