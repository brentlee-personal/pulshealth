import type { Metadata } from "next";
import { notFound, redirect } from "next/navigation";
import { headers } from "next/headers";

import { ReportingTimeZone } from "@/components/ReportingTimeZone";
import { reportingTimeZone } from "@/lib/reportingTimeZone";
import { ConnectIphone } from "@/components/ConnectIphone";
import { PageHeader } from "@/components/PageHeader";
import { errorMessage, noticeMessage, param } from "@/lib/accounts/messages";
import { PASSWORD_MIN_LENGTH } from "@/lib/accounts/password";
import { myDevices } from "@/lib/accounts/signups";
import { formatFull } from "@/lib/format";
import { listSessions } from "@/lib/accounts/session";
import { viewerMode } from "@/lib/mode";
import { oauthConfig } from "@/lib/oauth/config";
import { listConnectedApps } from "@/lib/oauth/store";
import { currentSession } from "@/lib/viewer";

// The personal account and its sessions, devices and grants.
export const dynamic = "force-dynamic";
export const metadata: Metadata = { title: "Account — PulsHealth" };

type Search = Promise<Record<string, string | string[] | undefined>>;

export default async function AccountPage({ searchParams }: { searchParams: Search }) {
  if (viewerMode() !== "accounts") notFound();
  const session = await currentSession();
  if (!session) redirect("/login?next=%2Faccount");
  const search = await searchParams;
  const returnToApp = /iPhone|iPad|iPod/.test((await headers()).get("user-agent") ?? "");
  const timeZone = await reportingTimeZone(session.userId);
  const [sessions, devices, assistants] = await Promise.all([
    listSessions(session.accountId, session.id),
    myDevices(session.id),
    listConnectedApps(session.accountId),
  ]);
  // Listed while OAuth is on, and afterwards for as long as any grant is left.
  const showAssistants = oauthConfig() !== null || assistants.length > 0;
  const error = errorMessage(param(search.error));
  const notice = noticeMessage(param(search.notice));

  return (
    <>
      <PageHeader title="Account" subtitle={`Signed in as ${session.email}.`} />

      {error && (
        <div className="form-message error" role="alert" style={{ maxWidth: 560 }}>
          {error}
        </div>
      )}
      {notice && (
        <div className="form-message notice" role="status" style={{ maxWidth: 560 }}>
          {notice}
        </div>
      )}

      <section className="rise" style={{ marginTop: 8 }}>
        <div className="eyebrow" style={{ marginBottom: 12 }}>Your iPhones</div>
        <ConnectIphone returnToApp={returnToApp} />
        {devices.length > 0 && (
          <div className="panel" style={{ maxWidth: 720, marginTop: 14 }}>
            {devices.map((d) => (
              <div key={d.id} className="session-row">
                <div style={{ minWidth: 0 }}>
                  <div style={{ fontSize: 14, fontWeight: 550 }}>
                    {d.name || "iPhone"} <span className="mono" style={{ color: "var(--muted)", fontSize: 12 }}>{d.prefix}…</span>
                    {!d.active && <span className="chip" style={{ marginLeft: 8 }}>Disconnected</span>}
                  </div>
                  <div style={{ fontSize: 12.5, color: "var(--muted)", marginTop: 3 }}>
                    Connected {formatFull(d.createdAt, timeZone)}
                    {d.lastSeenAt ? ` · last upload ${formatFull(d.lastSeenAt, timeZone)}` : " · no upload yet"}
                  </div>
                </div>
                {d.active && (
                  <form method="post" action="/api/auth/devices">
                    <input type="hidden" name="id" value={d.id} />
                    <button type="submit" className="btn">Disconnect</button>
                  </form>
                )}
              </div>
            ))}
          </div>
        )}
      </section>

      <ReportingTimeZone timeZone={timeZone} />

      {showAssistants && (
        <section className="rise" style={{ marginTop: 28 }}>
          <div className="eyebrow" style={{ marginBottom: 12 }}>Connected apps</div>
          <div className="panel" style={{ maxWidth: 720 }}>
            {assistants.length === 0 && (
              <div className="session-row" style={{ fontSize: 13.5, color: "var(--muted)" }}>
                No app is connected. Add PulsHealth in Home Assistant, or connect an AI assistant,
                to read your synced data. You will approve access here first.
              </div>
            )}
            {assistants.map((a) => (
              <div key={a.id} className="session-row">
                <div style={{ minWidth: 0 }}>
                  <div style={{ fontSize: 14, fontWeight: 550 }}>
                    {a.clientName || "An unnamed app"}{" "}
                    <span style={{ color: "var(--muted)", fontSize: 12, fontWeight: 400 }}>(self-reported name)</span>
                  </div>
                  <div style={{ fontSize: 12.5, color: "var(--muted)", marginTop: 3 }}>
                    Connected {formatFull(a.createdAt, timeZone)} · last used {formatFull(a.lastUsedAt, timeZone)} · reads all your data, read-only
                  </div>
                </div>
                <form method="post" action="/api/auth/assistants">
                  <input type="hidden" name="id" value={a.id} />
                  <button type="submit" className="btn">Revoke</button>
                </form>
              </div>
            ))}
          </div>
          {assistants.length > 0 && (
            <p className="form-hint" style={{ margin: "10px 0 0", maxWidth: 720, lineHeight: 1.5 }}>
              Revoking stops Home Assistant health API access immediately. AI assistant access stops renewing
              immediately; an existing assistant access token expires within 30 minutes.
            </p>
          )}
        </section>
      )}

      <section className="rise" style={{ marginTop: 28 }}>
        <div className="eyebrow" style={{ marginBottom: 12 }}>Password</div>
        <form method="post" action="/api/auth/password" className="panel" style={{ padding: "20px 20px 6px", maxWidth: 560 }}>
          <input type="text" name="username" value={session.email} autoComplete="username" readOnly hidden />
          <div className="form-field">
            <label htmlFor="current">Current password</label>
            <input id="current" name="current" type="password" autoComplete="current-password" required />
          </div>
          <div className="form-field">
            <label htmlFor="password">New password</label>
            <input id="password" name="password" type="password" autoComplete="new-password" minLength={PASSWORD_MIN_LENGTH} required />
            <span className="form-hint">
              At least {PASSWORD_MIN_LENGTH} characters. Changing it signs out every other browser and disconnects every AI
              assistant.
            </span>
          </div>
          <div className="form-field">
            <label htmlFor="confirm">Repeat the new password</label>
            <input id="confirm" name="confirm" type="password" autoComplete="new-password" minLength={PASSWORD_MIN_LENGTH} required />
          </div>
          <div style={{ marginBottom: 14 }}>
            <button type="submit" className="btn btn-primary">Change password</button>
          </div>
        </form>
      </section>

      <section className="rise" style={{ marginTop: 28 }}>
        <div className="eyebrow" style={{ marginBottom: 12 }}>Signed in</div>
        <div className="panel" style={{ maxWidth: 720 }}>
          {sessions.map((s) => (
            <div key={s.id} className="session-row">
              <div style={{ minWidth: 0 }}>
                <div style={{ fontSize: 14, fontWeight: 550 }}>
                  {describeAgent(s.userAgent)}
                  {s.current && <span className="chip" style={{ marginLeft: 8 }}>This browser</span>}
                </div>
                <div style={{ fontSize: 12.5, color: "var(--muted)", marginTop: 3 }}>
                  Last active {formatFull(s.lastSeenAt, timeZone)} · signed in {formatFull(s.createdAt, timeZone)}
                  {s.ip ? ` · ${s.ip}` : ""}
                </div>
              </div>
              <form method="post" action="/api/auth/sessions">
                <input type="hidden" name="session" value={s.id} />
                <button type="submit" className="btn">{s.current ? "Sign out" : "Sign out this browser"}</button>
              </form>
            </div>
          ))}
        </div>
        {sessions.length > 1 && (
          <form method="post" action="/api/auth/sessions" style={{ marginTop: 14 }}>
            <input type="hidden" name="session" value="others" />
            <button type="submit" className="btn">Sign out everywhere else</button>
          </form>
        )}
      </section>

      {session.canDelete && !session.isAdmin ? (
        // The anchor is the app's Delete PulsHealth Account link
        // (PulsHealthDatabase.deleteAccountURL): App Review wants a link
        // straight to account deletion. Keep the id.
        <section id="delete-account" className="rise" style={{ marginTop: 28, scrollMarginTop: 24 }}>
          <div className="eyebrow" style={{ marginBottom: 12 }}>Delete account</div>
          <form method="post" action="/api/auth/delete-account" className="panel" style={{ padding: 20, maxWidth: 560 }}>
            <p style={{ margin: "0 0 12px", fontSize: 14, lineHeight: 1.5 }}>
              Requesting deletion disables your account, signs you out everywhere and disconnects your iPhones at once.
              It then permanently removes your account and health records from this database. If removal is interrupted,
              it retries automatically. You receive a private link to check completion. Export anything you want to keep
              from the PulsHealth app first.
            </p>
            <label style={{ display: "flex", gap: 8, alignItems: "center", fontSize: 13.5, marginBottom: 14 }}>
              <input type="checkbox" name="confirm" value="yes" required /> Delete my account and my data
            </label>
            <button type="submit" className="btn" style={{ color: "#ff7b72" }}>Delete my account and data</button>
          </form>
        </section>
      ) : (
        <section id="delete-account" className="rise" style={{ marginTop: 28, scrollMarginTop: 24 }}>
          <div className="eyebrow" style={{ marginBottom: 12 }}>Account deletion</div>
          <p style={{ fontSize: 14, lineHeight: 1.6 }}>
            This is an operator-managed account. Contact the server operator to remove it.
            If this is your personal PulsHealth-hosted account, <a href="mailto:support@pulshealth.com">contact PulsHealth support</a> to correct its account classification.
          </p>
        </section>
      )}
    </>
  );
}

// "Safari on macOS", "Chrome on Android", … from a User-Agent; good enough to
// tell your own browsers apart, never used for anything else.
function describeAgent(agent: string | null): string {
  if (!agent) return "Unknown browser";
  const browser = /Edg\//.test(agent)
    ? "Edge"
    : /Firefox\//.test(agent)
      ? "Firefox"
      : /Chrome\//.test(agent)
        ? "Chrome"
        : /Safari\//.test(agent)
          ? "Safari"
          : "Browser";
  const os = /iPhone|iPad/.test(agent)
    ? "iOS"
    : /Android/.test(agent)
      ? "Android"
      : /Mac OS X/.test(agent)
        ? "macOS"
        : /Windows/.test(agent)
          ? "Windows"
          : /Linux/.test(agent)
            ? "Linux"
            : null;
  return os ? `${browser} on ${os}` : browser;
}
