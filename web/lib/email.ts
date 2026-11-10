// Outbound email for accounts mode: the operator's notice of a new access
// request or a deletion, and an approved person's invite. Sent through the
// Amazon SES v2 API with a hand-rolled AWS Signature Version 4 (node:crypto),
// so the viewer still has no email or AWS dependency. Server-only.
//
// Off unless configured: WEB_SES_ACCESS_KEY_ID_FILE, WEB_SES_SECRET_ACCESS_KEY_FILE,
// WEB_SES_REGION (default us-east-1) and WEB_MAIL_FROM, an address SES has
// verified. WEB_ADMIN_EMAIL is where the operator's notices go. Unconfigured,
// nothing is sent and the admin page is the only place requests show up.
// A failed send is logged (never the message body or an address) and
// reported to the caller, which carries on: an email is a courtesy, not part
// of a decision.

import { createHash, createHmac } from "node:crypto";
import { readSecret } from "../scripts/secrets.mjs";

import type { Env } from "./mode";

export interface MailConfig {
  region: string;
  accessKeyId: string;
  secretAccessKey: string;
  from: string;
  admin: string | null;
}

export function mailConfig(env: Env = process.env): MailConfig | null {
  const accessKeyId = readSecret("WEB_SES_ACCESS_KEY_ID", env);
  const secretAccessKey = readSecret("WEB_SES_SECRET_ACCESS_KEY", env);
  const from = env.WEB_MAIL_FROM?.trim();
  if (!accessKeyId || !secretAccessKey || !from) return null;
  return {
    region: env.WEB_SES_REGION?.trim() || "us-east-1",
    accessKeyId,
    secretAccessKey,
    from,
    admin: env.WEB_ADMIN_EMAIL?.trim() || null,
  };
}

const sha256Hex = (data: string) => createHash("sha256").update(data, "utf8").digest("hex");
const hmac = (key: Buffer | string, data: string) => createHmac("sha256", key).update(data, "utf8").digest();

/** `20150830T123600Z` and `20150830` for a date. */
export function amzDates(now: Date): { amzDate: string; dateStamp: string } {
  const amzDate = now.toISOString().replace(/[-:]/g, "").replace(/\.\d{3}/, "");
  return { amzDate, dateStamp: amzDate.slice(0, 8) };
}

export interface SignInput {
  method: string;
  host: string;
  path: string;
  /** Already-encoded canonical query string ("" for none). */
  query?: string;
  /** Headers to sign besides host and x-amz-date, lower-case names. */
  headers?: Record<string, string>;
  body: string;
  service: string;
  region: string;
  accessKeyId: string;
  secretAccessKey: string;
  now: Date;
}

/**
 * AWS Signature Version 4: the Authorization header, and the x-amz-date it
 * signed, for one request (docs.aws.amazon.com, "Create a signed AWS API
 * request"). Pure, so the test suite can check it against AWS's vectors.
 */
export function signV4(input: SignInput): { authorization: string; amzDate: string } {
  const { amzDate, dateStamp } = amzDates(input.now);
  const headers: Record<string, string> = { ...(input.headers ?? {}), host: input.host, "x-amz-date": amzDate };
  const names = Object.keys(headers).map((n) => n.toLowerCase()).sort();
  const canonicalHeaders = names.map((n) => `${n}:${String(headers[n]).trim().replace(/\s+/g, " ")}\n`).join("");
  const signedHeaders = names.join(";");
  const canonicalRequest = [
    input.method,
    input.path,
    input.query ?? "",
    canonicalHeaders,
    signedHeaders,
    sha256Hex(input.body),
  ].join("\n");
  const scope = `${dateStamp}/${input.region}/${input.service}/aws4_request`;
  const stringToSign = ["AWS4-HMAC-SHA256", amzDate, scope, sha256Hex(canonicalRequest)].join("\n");
  const kDate = hmac(`AWS4${input.secretAccessKey}`, dateStamp);
  const kRegion = hmac(kDate, input.region);
  const kService = hmac(kRegion, input.service);
  const kSigning = hmac(kService, "aws4_request");
  const signature = createHmac("sha256", kSigning).update(stringToSign, "utf8").digest("hex");
  return {
    amzDate,
    authorization: `AWS4-HMAC-SHA256 Credential=${input.accessKeyId}/${scope}, SignedHeaders=${signedHeaders}, Signature=${signature}`,
  };
}

/** A header-safe subject: control characters (CR, LF) become spaces, at most 200 characters. */
export function cleanSubject(subject: string): string {
  return subject.replace(/[\u0000-\u001f\u007f]+/g, " ").replace(/\s+/g, " ").trim().slice(0, 200);
}

/** SES error text with any email address taken out, so the log never keeps one. */
export function redactAddresses(text: string): string {
  return text.replace(/[^\s<>"'(),;:]+@[^\s<>"'(),;:]+/g, "<address>");
}

export interface Mail {
  to: string;
  subject: string;
  text: string;
  /** Where a reply should go (the person, for the operator's notices). */
  replyTo?: string;
}

/** Sends one plain-text message; true on success. Never throws. */
export async function sendMail(mail: Mail, config = mailConfig(), fetchImpl: typeof fetch = fetch): Promise<boolean> {
  if (!config) return false;
  const host = `email.${config.region}.amazonaws.com`;
  const path = "/v2/email/outbound-emails";
  const body = JSON.stringify({
    FromEmailAddress: config.from,
    Destination: { ToAddresses: [mail.to] },
    ...(mail.replyTo ? { ReplyToAddresses: [mail.replyTo] } : {}),
    Content: {
      Simple: {
        Subject: { Data: cleanSubject(mail.subject), Charset: "UTF-8" },
        Body: { Text: { Data: mail.text, Charset: "UTF-8" } },
      },
    },
  });
  const { authorization, amzDate } = signV4({
    method: "POST",
    host,
    path,
    headers: { "content-type": "application/json" },
    body,
    service: "ses",
    region: config.region,
    accessKeyId: config.accessKeyId,
    secretAccessKey: config.secretAccessKey,
    now: new Date(),
  });
  try {
    const res = await fetchImpl(`https://${host}${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Amz-Date": amzDate, Authorization: authorization },
      body,
      signal: AbortSignal.timeout(10_000),
    });
    if (!res.ok) {
      console.error(`[puls-web] email not sent: SES answered ${res.status}`);
      return false;
    }
    return true;
  } catch {
    console.error("[puls-web] email not sent: provider unavailable");
    return false;
  }
}
