import type { Session } from "./session";
import { issueDeviceToken, pairingLink } from "./signups";
import { errorMessage } from "./messages";

export type ConnectionResult = { ok: true; link: string } | { ok: false; code: string; error: string };

/** Both the account page and the app handoff use the same pairing rules. */
export async function connectPhone(account: Session | null, name: string): Promise<ConnectionResult> {
  if (!account) return { ok: false, code: "session", error: "Sign in again, then try once more." };
  let url: string;
  try {
    const parsed = new URL(process.env.WEB_INGEST_URL?.trim() ?? "");
    if (parsed.protocol !== "https:" || parsed.username || parsed.password || parsed.search || parsed.hash) throw new Error("invalid");
    url = parsed.toString().replace(/\/+$/, "");
  } catch {
    return { ok: false, code: "sync_address", error: "The operator has not set this viewer's sync address (WEB_INGEST_URL), so there is nothing to connect to yet." };
  }
  try {
    const { token } = await issueDeviceToken(account.id, name.trim().slice(0, 100) || "iPhone");
    return { ok: true, link: pairingLink(url, token, account.userId) };
  } catch (e) {
    const code = (e as { code?: string }).code;
    if (code === "54000") return { ok: false, code: "devices", error: errorMessage("devices")! };
    // Never log a database error's message: it may include credential fields.
    console.error("[puls-web] connect failed", { code: code ?? "unknown" });
    return { ok: false, code: "failed", error: "That did not work. Try again in a moment." };
  }
}
