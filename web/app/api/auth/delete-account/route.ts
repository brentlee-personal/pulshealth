import { after, type NextRequest } from "next/server";

import { accountsOnly, clearSessionCookie, field, readForm, requestSession, seeOther } from "@/lib/accounts/http";
import { completeDeletion, requestDeletion } from "@/lib/accounts/deletion";

// Revoke access and durably queue erasure, then try to finish it now. The
// database worker retries automatically if this request cannot finish.
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  const off = accountsOnly();
  if (off) return off;
  const session = await requestSession(request);
  if (!session) return seeOther("/login?next=%2Faccount");
  if (session.isAdmin || !session.canDelete) return seeOther("/account?error=forbidden");
  const form = await readForm(request);
  if (field(form, "confirm") !== "yes") return seeOther("/account?error=failed");
  try {
    const { userId, receipt } = await requestDeletion(session.id);
    // Deliver the private status link before lengthy compressed-data erasure
    // can hit a browser/proxy timeout. The durable worker is the fallback if
    // this process exits before the after-response attempt finishes.
    after(async () => {
      try {
        await completeDeletion(userId);
      } catch (e) {
        console.error("[puls-web] deletion queued for retry", { code: (e as { code?: string }).code ?? "unknown" });
      }
    });
    return clearSessionCookie(seeOther(`/deletion/${receipt}`));
  } catch (e) {
    console.error("[puls-web] account deletion failed:", e instanceof Error ? e.message : e);
    return seeOther("/account?error=failed");
  }
}
