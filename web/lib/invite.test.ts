import { secretFixture } from "./secretFixture";
import { createHash } from "node:crypto";
import { describe, expect, it, vi } from "vitest";

import { inviteMessage, newInviteToken, normalizeEmail, parseArgs, sesSend } from "../scripts/invite.mjs";

const mailSecret = secretFixture({ WEB_SES_ACCESS_KEY_ID: "AKID", WEB_SES_SECRET_ACCESS_KEY: "secret" });
const USER = "11111111-1111-4111-8111-111111111111";

describe("scripts/invite.mjs", () => {
  it("parses a complete invite", () => {
    expect(parseArgs(["--email", " Ann@Example.com ", "--admin"], {})).toEqual({
      options: { email: "ann@example.com", admin: true, url: null, hours: 48, send: false },
    });
  });

  it("takes the base URL from WEB_PUBLIC_URL unless --url says otherwise", () => {
    expect(parseArgs(["--email", "a@example.com"], { WEB_PUBLIC_URL: "https://viewer.example/x" }).options?.url)
      .toBe("https://viewer.example");
    expect(
      parseArgs(["--email", "a@example.com", "--url", "https://other.example"], { WEB_PUBLIC_URL: "https://viewer.example" })
        .options?.url,
    ).toBe("https://other.example");
  });

  it("refuses what would make a broken or unsafe link", () => {
    expect(parseArgs([], {}).error).toMatch(/--email/);
    expect(parseArgs(["--user", USER, "--email", "a@example.com"], {}).error).toMatch(/unknown argument: --user/);
    expect(parseArgs(["--email", "not-an-email"], {}).error).toMatch(/--email/);
    expect(parseArgs(["--email", "a@example.com", "--url", "http://viewer.example"], {}).error).toMatch(/https/);
    expect(parseArgs(["--email", "a@example.com", "--hours", "0"], {}).error).toMatch(/--hours/);
    expect(parseArgs(["--email"], {}).error).toMatch(/needs a value/);
    expect(parseArgs(["--bogus"], {}).error).toMatch(/unknown/);
  });

  it("normalises email the way the viewer does", () => {
    expect(normalizeEmail("  A@B.co ")).toBe("a@b.co");
    expect(normalizeEmail("a b@c")).toBeNull();
  });

  it("stores the SHA-256 of the token's bytes, the same as the viewer looks up", () => {
    const { token, hash } = newInviteToken();
    expect(token).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(hash.equals(createHash("sha256").update(Buffer.from(token, "base64url")).digest())).toBe(true);
  });

  it("emails the link only with a full viewer address to put in it", () => {
    expect(parseArgs(["--email", "a@example.com", "--send"], {}).error).toMatch(/--send needs/);
    expect(parseArgs(["--email", "a@example.com", "--send"], { WEB_PUBLIC_URL: "https://viewer.example" }).options?.send).toBe(true);
    const m = inviteMessage({ link: "https://viewer.example/invite/abc", expires: new Date("2026-10-04T00:00:00Z") });
    expect(m.text).toContain("https://viewer.example/invite/abc");
    expect(m.subject).toBe("You're invited to the PulsHealth viewer");
  });

  it("signs the SES request the same way as the viewer", async () => {
    const { signV4 } = await import("./email");
    const fetchMock = vi.fn(async () => new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    const now = new Date("2026-10-02T20:00:00Z");
    const env = { ...mailSecret, WEB_MAIL_FROM: "x@example.com" };
    await sesSend({ to: "a@example.com", subject: "S", text: "T" }, env, now);
    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    const expected = signV4({
      method: "POST", host: "email.us-east-1.amazonaws.com", path: "/v2/email/outbound-emails",
      headers: { "content-type": "application/json" }, body: String(init.body), service: "ses", region: "us-east-1",
      accessKeyId: "AKID", secretAccessKey: "secret", now,
    });
    expect((init.headers as Record<string, string>).Authorization).toBe(expected.authorization);
    vi.unstubAllGlobals();
    await expect(sesSend({ to: "a", subject: "s", text: "t" }, {})).rejects.toThrow(/not set up/);
  });
});
