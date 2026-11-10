import { secretFixture } from "../secretFixture";
import { afterEach, describe, expect, it, vi } from "vitest";

import { DailyCap, approvalMessage, newRequestNotice, publicBase, sendApproval } from "./mail";

const MAIL_ENV = {
  ...secretFixture({ WEB_SES_ACCESS_KEY_ID: "AKID", WEB_SES_SECRET_ACCESS_KEY: "secret" }),
  WEB_MAIL_FROM: "noreply@example.com",
  WEB_ADMIN_EMAIL: "operator@example.com",
};

afterEach(() => {
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
});

describe("publicBase", () => {
  it("is the origin of an http(s) WEB_PUBLIC_URL, else empty", () => {
    expect(publicBase({ WEB_PUBLIC_URL: "https://viewer.example/some/path" })).toBe("https://viewer.example");
    expect(publicBase({})).toBe("");
    expect(publicBase({ WEB_PUBLIC_URL: "javascript:alert(1)" })).toBe("");
  });
});

describe("DailyCap", () => {
  it("allows the limit in a day, then refuses until the oldest is a day old", () => {
    const cap = new DailyCap(2);
    expect([cap.allow(0), cap.allow(1), cap.allow(2)]).toEqual([true, true, false]);
    expect(cap.allow(86_400_001)).toBe(true);
  });
});

describe("messages", () => {
  it("never points a link at a missing host", () => {
    const notice = newRequestNotice({ name: "Pat", email: "pat@example.com", note: "", ip: null }, "");
    expect(notice.text).toContain("Approve or decline: the viewer's /admin page");
    expect(approvalMessage({ name: "Pat", email: "pat@example.com", inviteToken: "t", days: 7 }, "https://viewer.example").text).toContain(
      "https://viewer.example/invite/t",
    );
  });

  it("does not send an approval without WEB_PUBLIC_URL, so the administrator gets the link instead", async () => {
    const fetchMock = vi.fn(async () => new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    for (const [k, v] of Object.entries(MAIL_ENV)) vi.stubEnv(k, v);
    vi.stubEnv("WEB_PUBLIC_URL", "");
    expect(await sendApproval({ name: "Pat", email: "pat@example.com", inviteToken: "t", days: 7 })).toBe(false);
    expect(fetchMock).not.toHaveBeenCalled();

    vi.stubEnv("WEB_PUBLIC_URL", "https://viewer.example");
    expect(await sendApproval({ name: "Pat", email: "pat@example.com", inviteToken: "t", days: 7 })).toBe(true);
    expect(fetchMock).toHaveBeenCalledOnce();
  });
});
