#!/usr/bin/env node
// Issues a one-time invite link for the web viewer's accounts mode.
//
//   docker compose exec web node scripts/invite.mjs --user <uuid> --email <address> [--admin]
//                                                   [--url https://<viewer host>] [--hours 48] [--send]
//   make web-invite ARGS='--user <uuid> --email <address> [--admin]'
//
// The person opens the link, creates a new account and is signed in. Existing
// accounts use owner-verified password recovery. The first administrator is
// invited with --admin.
//
// The user must already exist in the database: issue the person's device
// token first (`make issue-device NAME=… ARGS='--user <uuid>'`), which
// creates the row, or let their phone sync once. Issuing a device token stays
// a separate step on purpose — the viewer never hands out ingest credentials.
//
// The token is 32 random bytes; only its SHA-256 is stored (auth.invites),
// and the link is printed here once — or, with --send, emailed to the
// invitee through Amazon SES (the WEB_SES_* and WEB_MAIL_FROM settings the
// viewer uses) and not printed at all. It works once and expires after
// --hours (48 by default). The base URL comes from --url, else WEB_PUBLIC_URL.
//
// Connects with the container's DATABASE_URL — in accounts mode, the web_app
// role, which may write auth.invites and nothing that holds health data.

import { createHash, createHmac, randomBytes } from "node:crypto";
import { pathToFileURL } from "node:url";

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export const USAGE =
  "usage: node scripts/invite.mjs --user <uuid> --email <address> [--admin] [--url https://<viewer host>] [--hours 48] [--send]";

/** Parsed options, or { error } with a message for the operator. */
export function parseArgs(argv, env = {}) {
  const options = { user: null, email: null, admin: false, url: env.WEB_PUBLIC_URL || null, hours: 48, send: false };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    const value = () => {
      const next = argv[++i];
      if (next === undefined || next.startsWith("--")) throw new Error(`${arg} needs a value`);
      return next;
    };
    try {
      if (arg === "--user") options.user = value();
      else if (arg === "--email") options.email = value();
      else if (arg === "--url") options.url = value();
      else if (arg === "--hours") options.hours = Number(value());
      else if (arg === "--admin") options.admin = true;
      else if (arg === "--send") options.send = true;
      else if (arg === "--help" || arg === "-h") return { help: true };
      else return { error: `unknown argument: ${arg}` };
    } catch (e) {
      return { error: e.message };
    }
  }
  if (!options.user || !UUID.test(options.user)) return { error: "--user must be the user's UUID" };
  options.user = options.user.toLowerCase();
  const email = normalizeEmail(options.email);
  if (!email) return { error: "--email must be an email address" };
  options.email = email;
  if (!Number.isInteger(options.hours) || options.hours < 1 || options.hours > 24 * 14) {
    return { error: "--hours must be a whole number of hours between 1 and 336" };
  }
  if (options.url) {
    let url;
    try {
      url = new URL(options.url);
    } catch {
      return { error: `not a URL: ${options.url}` };
    }
    if (url.protocol !== "https:" && !["localhost", "127.0.0.1"].includes(url.hostname)) {
      return { error: "the viewer's URL must be https:// — accounts mode refuses plain HTTP" };
    }
    options.url = url.origin;
  }
  if (options.send && !options.url) return { error: "--send needs the viewer's address: --url, or WEB_PUBLIC_URL" };
  return { options };
}

/** Same rule as lib/accounts/store.ts normalizeEmail. */
export function normalizeEmail(raw) {
  const email = String(raw ?? "").trim().toLowerCase();
  if (email.length < 3 || email.length > 254 || !/^[^\s@]+@[^\s@]+$/.test(email)) return null;
  return email;
}

/** A fresh token and the hash that is stored for it. */
export function newInviteToken() {
  const bytes = randomBytes(32);
  return { token: bytes.toString("base64url"), hash: createHash("sha256").update(bytes).digest() };
}

/**
 * Emails a message through Amazon SES's v2 API with AWS Signature Version 4
 * (the same signing as web/lib/email.ts, which the app bundle cannot share
 * with this plain script). Throws on any failure.
 * @param {{ to: string, subject: string, text: string }} message
 * @param {Record<string, string | undefined>} [env]
 * @param {Date} [now]
 */
export async function sesSend({ to, subject, text }, env = process.env, now = new Date()) {
  const id = env.WEB_SES_ACCESS_KEY_ID, secret = env.WEB_SES_SECRET_ACCESS_KEY, from = env.WEB_MAIL_FROM;
  if (!id || !secret || !from) throw new Error("email is not set up (WEB_SES_ACCESS_KEY_ID, WEB_SES_SECRET_ACCESS_KEY, WEB_MAIL_FROM)");
  const region = env.WEB_SES_REGION || "us-east-1";
  const host = `email.${region}.amazonaws.com`, path = "/v2/email/outbound-emails";
  const body = JSON.stringify({
    FromEmailAddress: from,
    Destination: { ToAddresses: [to] },
    Content: { Simple: { Subject: { Data: subject, Charset: "UTF-8" }, Body: { Text: { Data: text, Charset: "UTF-8" } } } },
  });
  const amzDate = now.toISOString().replace(/[-:]/g, "").replace(/\.\d{3}/, "");
  const day = amzDate.slice(0, 8);
  const sha = (d) => createHash("sha256").update(d, "utf8").digest("hex");
  const hmac = (k, d) => createHmac("sha256", k).update(d, "utf8").digest();
  const canonical = ["POST", path, "", `content-type:application/json\nhost:${host}\nx-amz-date:${amzDate}\n`, "content-type;host;x-amz-date", sha(body)].join("\n");
  const scope = `${day}/${region}/ses/aws4_request`;
  const key = hmac(hmac(hmac(hmac(`AWS4${secret}`, day), region), "ses"), "aws4_request");
  const signature = createHmac("sha256", key).update(["AWS4-HMAC-SHA256", amzDate, scope, sha(canonical)].join("\n"), "utf8").digest("hex");
  const res = await fetch(`https://${host}${path}`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Amz-Date": amzDate,
      Authorization: `AWS4-HMAC-SHA256 Credential=${id}/${scope}, SignedHeaders=content-type;host;x-amz-date, Signature=${signature}`,
    },
    body,
    signal: AbortSignal.timeout(10_000),
  });
  if (!res.ok) throw new Error(`SES answered ${res.status}: ${(await res.text()).slice(0, 200)}`);
}

export function inviteMessage({ link, expires }) {
  return {
    subject: "You're invited to the PulsHealth viewer",
    text: [
      "You've been invited to view your own health records in the PulsHealth viewer.",
      "",
      `Choose a password here (the link works once, until ${expires.toUTCString()}):`,
      `  ${link}`,
      "",
      "If you weren't expecting this, ignore it; nothing happens unless the link is used.",
    ].join("\n"),
  };
}

async function main() {
  const parsed = parseArgs(process.argv.slice(2), process.env);
  if (parsed.help) {
    console.log(USAGE);
    return 0;
  }
  if (parsed.error) {
    console.error(`invite: ${parsed.error}\n${USAGE}`);
    return 2;
  }
  const { user, email, admin, url, hours, send } = parsed.options;
  if (!process.env.DATABASE_URL) {
    console.error("invite: DATABASE_URL is not set (run this inside the web container: docker compose exec web …)");
    return 1;
  }

  const { default: pg } = await import("pg");
  const client = new pg.Client({ connectionString: process.env.DATABASE_URL });
  await client.connect();
  try {
    const taken = await client.query(
      "SELECT user_id::text FROM auth.accounts WHERE email = $1 AND user_id <> $2",
      [email, user],
    );
    if (taken.rowCount) {
      console.error(`invite: ${email} already signs in another user's account (${taken.rows[0].user_id}).`);
      return 1;
    }
    const existing = await client.query("SELECT email FROM auth.accounts WHERE user_id = $1", [user]);
    if (existing.rowCount) {
      console.error("invite: this member already has an account. Use owner-verified password recovery; invitations cannot replace it.");
      return 1;
    }
    const { token, hash } = newInviteToken();
    const inserted = await client.query(
      `INSERT INTO auth.invites (token_hash, user_id, email, is_admin, expires_at)
       VALUES ($1, $2, $3, $4, now() + make_interval(hours => $5))
       RETURNING expires_at`,
      [hash, user, email, admin, hours],
    );
    const path = `/invite/${token}`;
    if (send) {
      const message = inviteMessage({ link: `${url}${path}`, expires: inserted.rows[0].expires_at });
      try {
        await sesSend({ to: email, ...message });
        console.log(`Emailed an invite link to ${email}${admin ? " (administrator)" : ""}, user ${user}.`);
        return 0;
      } catch (e) {
        console.error(`invite: the email was not sent (${e instanceof Error ? e.message : e}); here is the link to send yourself:`);
      }
    }
    console.log(`Invite for ${email}${admin ? ", as an administrator" : ""}, user ${user}.`);
    console.log(`Works once, until ${inserted.rows[0].expires_at.toISOString()}. Send it privately:\n`);
    if (url) {
      console.log(`  ${url}${path}\n`);
    } else {
      console.log(`  ${path}\n`);
      console.log("Prefix it with the viewer's https:// address (set WEB_PUBLIC_URL, or pass --url, to have it printed whole).");
    }
    return 0;
  } catch (e) {
    if (e && e.code === "23503") {
      console.error(
        `invite: there is no user ${user} in the database yet. Issue the person's device token first ` +
          "(make issue-device NAME='…' ARGS='--user <uuid>'), which creates the user, or let their phone sync once.",
      );
      return 1;
    }
    if (e && e.code === "42501") {
      console.error("invite: this database role may not write invites. The web container must connect as web_app (accounts mode).");
      return 1;
    }
    if (e && e.code === "3F000") {
      console.error("invite: the database has no `auth` schema yet; run the migrate service (docker compose up -d).");
      return 1;
    }
    throw e;
  } finally {
    await client.end();
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().then(
    (code) => process.exit(code),
    (e) => {
      console.error("invite: failed:", e instanceof Error ? e.message : e);
      process.exit(1);
    },
  );
}
