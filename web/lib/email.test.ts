import { secretFixture } from "./secretFixture";
import { describe, expect, it, vi } from "vitest";

import { amzDates, cleanSubject, mailConfig, redactAddresses, sendMail, signV4 } from "./email";

const mailSecret = secretFixture({ WEB_SES_ACCESS_KEY_ID: "a", WEB_SES_SECRET_ACCESS_KEY: "b" });

describe("signV4", () => {
  it("matches AWS's get-vanilla test vector", () => {
    // From the AWS Signature Version 4 test suite.
    const { authorization, amzDate } = signV4({
      method: "GET",
      host: "example.amazonaws.com",
      path: "/",
      body: "",
      service: "service",
      region: "us-east-1",
      accessKeyId: "AKIDEXAMPLE",
      secretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
      now: new Date("2015-08-30T12:36:00Z"),
    });
    expect(amzDate).toBe("20150830T123600Z");
    expect(authorization).toBe(
      "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
        "SignedHeaders=host;x-amz-date, " +
        "Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
    );
  });

  it("formats dates the way AWS wants them", () => {
    expect(amzDates(new Date("2026-10-02T20:11:58.123Z"))).toEqual({ amzDate: "20261002T201158Z", dateStamp: "20261002" });
  });
});

describe("sendMail", () => {
  const config = { region: "us-east-1", accessKeyId: "AKID", secretAccessKey: "secret", from: "PulsHealth <noreply@example.com>", admin: null };

  it("is off unless the three settings are there", () => {
    expect(mailConfig({})).toBeNull();
    expect(mailConfig(mailSecret)).toBeNull();
    expect(mailConfig({ ...mailSecret, WEB_MAIL_FROM: "x@y.z", WEB_ADMIN_EMAIL: "o@y.z" }))
      .toEqual({ region: "us-east-1", accessKeyId: "a", secretAccessKey: "b", from: "x@y.z", admin: "o@y.z" });
  });

  it("posts a signed SES v2 request and reports the outcome without throwing", async () => {
    const fetchMock = vi.fn(async () => new Response("{}", { status: 200 }));
    expect(await sendMail({ to: "a@example.com", subject: "Hi", text: "Body", replyTo: "r@example.com" }, config, fetchMock as typeof fetch)).toBe(true);
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("https://email.us-east-1.amazonaws.com/v2/email/outbound-emails");
    expect((init.headers as Record<string, string>).Authorization).toMatch(/^AWS4-HMAC-SHA256 Credential=AKID\/\d{8}\/us-east-1\/ses\/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=[0-9a-f]{64}$/);
    expect(JSON.parse(String(init.body))).toMatchObject({
      FromEmailAddress: "PulsHealth <noreply@example.com>",
      Destination: { ToAddresses: ["a@example.com"] },
      ReplyToAddresses: ["r@example.com"],
      Content: { Simple: { Subject: { Data: "Hi" }, Body: { Text: { Data: "Body" } } } },
    });

    vi.spyOn(console, "error").mockImplementation(() => {});
    const failing = vi.fn(async () => new Response("denied", { status: 403 }));
    expect(await sendMail({ to: "a@example.com", subject: "Hi", text: "Body" }, config, failing as typeof fetch)).toBe(false);
    const throwing = vi.fn(async () => { throw new Error("offline"); });
    expect(await sendMail({ to: "a@example.com", subject: "Hi", text: "Body" }, config, throwing as typeof fetch)).toBe(false);
    expect(await sendMail({ to: "a@example.com", subject: "Hi", text: "Body" }, null)).toBe(false);
  });
});

describe("what reaches SES and the log", () => {
  const config = { region: "us-east-1", accessKeyId: "AKID", secretAccessKey: "secret", from: "noreply@example.com", admin: null };

  it("keeps a subject on one line", async () => {
    expect(cleanSubject("Hi\r\nBcc: x@example.com")).toBe("Hi Bcc: x@example.com");
    expect(cleanSubject("a".repeat(500))).toHaveLength(200);
    const fetchMock = vi.fn(async () => new Response("{}", { status: 200 }));
    await sendMail({ to: "a@example.com", subject: "access request from Pat\nX-Evil: 1", text: "Body" }, config, fetchMock as typeof fetch);
    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(JSON.parse(String(init.body)).Content.Simple.Subject.Data).toBe("access request from Pat X-Evil: 1");
  });

  it("never logs an address SES echoes back", async () => {
    expect(redactAddresses("Email address is not verified: person@example.com, <o@x.y>")).toBe(
      "Email address is not verified: <address>, <<address>>",
    );
    const logged = vi.spyOn(console, "error").mockImplementation(() => {});
    const failing = vi.fn(async () => new Response("identities failed the check: stranger@example.org", { status: 400 }));
    expect(await sendMail({ to: "stranger@example.org", subject: "Hi", text: "Body" }, config, failing as typeof fetch)).toBe(false);
    expect(logged.mock.calls.flat().join(" ")).not.toContain("stranger@example.org");
    logged.mockRestore();
  });
});
