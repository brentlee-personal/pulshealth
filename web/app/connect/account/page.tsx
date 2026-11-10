import { notFound, redirect } from "next/navigation";
import { AuthCard } from "@/components/AuthCard";
import { currentSession } from "@/lib/viewer";
import { viewerMode } from "@/lib/mode";
export const dynamic = "force-dynamic";
export default async function ConnectAccount({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  if (viewerMode() !== "accounts") notFound();
  const { state, challenge } = await searchParams;
  if (typeof challenge !== "string" || !/^[A-Za-z0-9_-]{43}$/.test(challenge)) notFound();
  if (typeof state !== "string" || !/^[a-f0-9]{64}$/.test(state)) notFound();
  const session = await currentSession();
  if (!session) redirect(`/login?next=${encodeURIComponent(`/connect/account?state=${state}&challenge=${challenge}`)}`);
  return <AuthCard title="Sign in to PulsHealth" subtitle={`Signed in as ${session.email}.`}>
    <p>Use your account identity in the app and save profile changes to your account. Signing in does not turn on Health data sync.</p>
    <form method="post" action="/api/auth/connect-account">
      <input type="hidden" name="state" value={state} />
      <input type="hidden" name="challenge" value={challenge} />
      <button className="btn btn-primary btn-block" type="submit">Use this account</button>
    </form>
    <form method="post" action="/api/auth/logout" style={{ marginTop: 16 }}>
      <input type="hidden" name="next" value={`/connect/account?state=${state}&challenge=${challenge}`} />
      <button className="btn" type="submit">Sign out to use a different account</button>
    </form>
  </AuthCard>;
}
