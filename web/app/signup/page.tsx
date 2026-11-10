import type { Metadata } from "next";
import Link from "next/link";
import { notFound, redirect } from "next/navigation";

import { AuthCard } from "@/components/AuthCard";
import { errorMessage, noticeMessage, param } from "@/lib/accounts/messages";
import { signupsOpen } from "@/lib/mode";
import { currentSession } from "@/lib/viewer";

// Sign up (accounts mode, with WEB_SIGNUPS on). The form posts to
// /api/auth/signup, which puts the person on the waitlist (a pending
// request) and tells the operator; it creates nothing else, and the page
// never says an account was made. An administrator approves on /admin, and
// only then does the person get an email with a link to choose a password.
export const dynamic = "force-dynamic";
export const metadata: Metadata = { title: "Create account — PulsHealth" };

type Search = Promise<Record<string, string | string[] | undefined>>;

export default async function SignupPage({ searchParams }: { searchParams: Search }) {
  if (!signupsOpen()) notFound();
  // Signed in already: nothing to ask for — unless it is the shared demo.
  const session = await currentSession().catch(() => null);
  if (session) redirect("/");
  const params = await searchParams;
  const notice = param(params.notice);

  if (notice === "received") {
    return (
      <AuthCard title="You're on the waitlist" notice={noticeMessage("received")}>
        <p className="form-hint" style={{ margin: 0, lineHeight: 1.5 }}>
          <Link href="/login" style={{ textDecoration: "underline" }}>Back to sign in</Link>
        </p>
      </AuthCard>
    );
  }

  return (
    <AuthCard
      title="Create your PulsHealth account"
      subtitle="Sync your iPhone's health data here, and see it all on the web."
      error={errorMessage(param(params.error))}
    >
      <form method="post" action="/api/auth/signup">
        <div className="form-field">
          <label htmlFor="name">Your name</label>
          <input id="name" name="name" type="text" autoComplete="name" maxLength={200} required autoFocus />
        </div>
        <div className="form-field">
          <label htmlFor="email">Email</label>
          <input id="email" name="email" type="email" autoComplete="email" maxLength={254} required />
        </div>
        <div className="form-field">
          <label htmlFor="note">Anything you would like us to know (optional)</label>
          <textarea
            id="note"
            name="note"
            rows={3}
            maxLength={2000}
            placeholder="Who you are, or how you heard of PulsHealth"
            style={{
              width: "100%",
              padding: "10px 12px",
              borderRadius: 10,
              border: "1px solid var(--border-strong)",
              background: "var(--bg-elevated)",
              color: "var(--fg)",
              font: "inherit",
              fontSize: 15,
              resize: "vertical",
            }}
          />
        </div>
        {/* Left empty by people; filled by bots that fill every field. */}
        <div aria-hidden="true" style={{ position: "absolute", left: -10000, width: 1, height: 1, overflow: "hidden" }}>
          <label htmlFor="website">Website</label>
          <input id="website" name="website" type="text" tabIndex={-1} autoComplete="off" />
        </div>
        <label style={{ display: "flex", gap: 10, alignItems: "flex-start", fontSize: 13.5, color: "var(--fg-soft)", margin: "4px 0 16px", lineHeight: 1.45 }}>
          <input type="checkbox" name="consent" value="yes" required style={{ marginTop: 3 }} />
          <span>
            I have read the{" "}
            <a href="https://pulshealth.com/privacy" style={{ textDecoration: "underline" }}>privacy policy</a>: once my
            account is set up, the health data my iPhone sends is stored on the operator&apos;s server.
          </span>
        </label>
        <button type="submit" className="btn btn-primary btn-block">
          Create account
        </button>
      </form>
      <p className="form-hint" style={{ margin: "18px 0 0", lineHeight: 1.5 }}>
        This stores your name, email, note and your browser&apos;s address and name, and emails them to the operator.{" "}
        <Link href="/login" style={{ textDecoration: "underline" }}>Already have an account?</Link>
      </p>
    </AuthCard>
  );
}
