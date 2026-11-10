import { oauthResources } from "@/lib/oauth/config";
import type { Metadata } from "next";
import { notFound, redirect } from "next/navigation";

import { AuthCard } from "@/components/AuthCard";
import { DEMO_ASSISTANT_REFUSAL } from "@/lib/accounts/messages";
import { oauthConfig } from "@/lib/oauth/config";
import { findClient } from "@/lib/oauth/store";
import { checkAuthorizeRequest, redirectWith } from "@/lib/oauth/validate";
import { currentSession } from "@/lib/viewer";
import { CATALOG } from "@/lib/catalog";

// The authorization endpoint's consent page (OAuth on, accounts mode only).
// proxy.ts has already sent anyone without a session to /login, which comes
// back here with the whole query. A bad client or redirect URI is an error
// page with nowhere to go. Any other problem is an error page too, with a
// "Return to <host>" link carrying `error=`, `state` and `iss` — never an
// automatic redirect: anyone can register a client, so redirecting on a
// malformed request would make this an open redirector (RFC 9700 §4.11.2).
// Only the person's own Deny, on the decision route, redirects. The form posts to
// /oauth/authorize, which proxy.ts hands to ./decision/route.ts after the
// same-origin and session checks; that route checks every parameter again.
// The page cannot be framed (frame-ancestors 'none', X-Frame-Options DENY),
// and its CSP form-action names the redirect URI's origin, so the browser
// follows the 303 there (lib/securityHeaders.ts).
export const dynamic = "force-dynamic";
export const metadata: Metadata = { title: "Connect an app — PulsHealth" };

type Search = Promise<Record<string, string | string[] | undefined>>;

function searchParamsOf(search: Record<string, string | string[] | undefined>): URLSearchParams {
  const params = new URLSearchParams();
  for (const [k, v] of Object.entries(search)) {
    for (const value of Array.isArray(v) ? v : v === undefined ? [] : [v]) params.append(k, value);
  }
  return params;
}

/** Where the app is: its host, or "an app on this computer" for a loopback address. */
function destination(redirectUri: string): string {
  const url = new URL(redirectUri);
  return url.protocol === "http:" ? `an app on this computer (${url.host})` : url.host;
}

export default async function AuthorizePage({ searchParams }: { searchParams: Search }) {
  const config = oauthConfig();
  if (!config) notFound();
  const params = searchParamsOf(await searchParams);
  const session = await currentSession();
  if (!session) redirect(`/login?next=${encodeURIComponent(`/oauth/authorize?${params.toString()}`)}`);
  // The shared demo account never connects an assistant (the decision route
  // refuses it too): no form, and nothing sent back to the app.
  if (session.demo) {
    return (
      <div style={{ display: "grid", placeItems: "center", padding: "32px 0" }}>
        <AuthCard title="This is the demo account" error={DEMO_ASSISTANT_REFUSAL}>
          <form method="post" action="/api/auth/logout">
            <button type="submit" className="btn btn-block">Sign out of the demo</button>
          </form>
        </AuthCard>
      </div>
    );
  }

  const client = await findClient(params.get("client_id"));
  const check = checkAuthorizeRequest(params, client, oauthResources(config));
  if (check.kind === "fatal") {
    return (
      <div style={{ display: "grid", placeItems: "center", padding: "32px 0" }}>
        <AuthCard title="This link cannot connect an app" error={check.message} />
      </div>
    );
  }
  if (check.kind === "error") {
    const back = redirectWith(check.redirectUri, {
      error: check.error,
      error_description: check.description,
      state: check.state,
      iss: config.issuer,
    });
    return (
      <div style={{ display: "grid", placeItems: "center", padding: "32px 0" }}>
        <AuthCard title="This link cannot connect an app" error={`The app's request is not valid: ${check.description}.`}>
          <p className="form-hint" style={{ margin: "0 0 18px", lineHeight: 1.5 }}>
            Nothing was shared. You can tell the app what went wrong; it will not get access.
          </p>
          <a href={back} className="btn" style={{ display: "block", textAlign: "center" }} rel="noreferrer">
            Return to <span className="mono">{destination(check.redirectUri)}</span>
          </a>
        </AuthCard>
      </div>
    );
  }

  const req = check.request;
  const name = req.clientName || "An unnamed app";
  const until = new Date();
  const from = new Date(until.getTime() - 30 * 86400_000);
  return (
    <div style={{ display: "grid", placeItems: "center", padding: "32px 0" }}>
      <AuthCard title={`Connect ${name}?`} subtitle={<>Signed in as {session.email}.</>}>
        <p style={{ margin: "0 0 12px", fontSize: 14, lineHeight: 1.55 }}>
          <strong style={{ fontWeight: 600 }}>{name}</strong>{" "}
          <span style={{ color: "var(--muted)" }}>(the name the app gave itself; this viewer cannot vouch for it)</span>{" "}
          is asking to read your PulsHealth data. If you allow it, it is sent back to{" "}
          <span className="mono">{destination(req.redirectUri)}</span>.
        </p>
        <div className="panel" style={{ padding: "12px 14px", marginBottom: 14, fontSize: 13.5, lineHeight: 1.55 }}>
          Choose the data types and UTC time range below. Summaries are selected by default.
          Raw samples need a separate choice. Your profile is not shared. The app cannot change or delete data.
        </div>
        <p className="form-hint" style={{ margin: "0 0 18px", lineHeight: 1.5 }}>
          Revoke it any time under Connected apps on your <a href="/account" style={{ textDecoration: "underline" }}>account
          page</a>. Only allow an app you just asked to connect.
        </p>
        <form method="post" action="/oauth/authorize">
          <fieldset style={{ marginBottom: 14 }}>
            <legend>Read access</legend>
            <label style={{ display: "block" }}><input type="checkbox" name="operation" value="metrics:read" defaultChecked /> Summaries</label>
            <label style={{ display: "block" }}><input type="checkbox" name="operation" value="samples:read" /> Raw samples</label>
          </fieldset>
          <fieldset style={{ marginBottom: 14, maxHeight: 220, overflow: "auto" }}>
            <legend>Data types</legend>
            {CATALOG.map((type) => <label key={type.identifier} style={{ display: "block" }}>
              <input type="checkbox" name="record_type" value={type.identifier} /> {type.name}
            </label>)}
          </fieldset>
          <label style={{ display: "block", marginBottom: 10 }}>From, UTC
            <input type="datetime-local" name="from" step="1" required defaultValue={from.toISOString().slice(0, 19)} />
          </label>
          <label style={{ display: "block", marginBottom: 14 }}>Until, UTC, excluded
            <input type="datetime-local" name="until" step="1" required defaultValue={until.toISOString().slice(0, 19)} />
          </label>
          <input type="hidden" name="client_id" value={req.clientId} />
          <input type="hidden" name="redirect_uri" value={req.redirectUri} />
          <input type="hidden" name="response_type" value="code" />
          <input type="hidden" name="code_challenge" value={req.codeChallenge} />
          <input type="hidden" name="code_challenge_method" value="S256" />
          <input type="hidden" name="scope" value={req.scope} />
          {req.resource !== null && <input type="hidden" name="resource" value={req.resource} />}
          {req.state !== null && <input type="hidden" name="state" value={req.state} />}
          <div style={{ display: "flex", gap: 10 }}>
            <button type="submit" name="decision" value="allow" className="btn btn-primary" style={{ flex: 1 }}>
              Allow
            </button>
            <button type="submit" name="decision" value="deny" className="btn" style={{ flex: 1 }}>
              Deny
            </button>
          </div>
        </form>
      </AuthCard>
    </div>
  );
}
